package rings

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/core/devices"
)

// ServeHTTP answers GET, PUT and DELETE on Path, and POST on TestPath.
func (r *Rings) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	allowed := map[string][]string{
		Path:     {http.MethodGet, http.MethodPut, http.MethodDelete},
		TestPath: {http.MethodPost},
	}[req.URL.Path]
	if allowed == nil {
		http.NotFound(w, req)
		return
	}
	if !slices.Contains(allowed, req.Method) {
		w.Header().Set("Allow", fmt.Sprint(allowed))
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body []byte
	if req.Method == http.MethodPut {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody)); err != nil {
			fail(w, http.StatusRequestEntityTooLarge, "the body is too large")
			return
		}
	}
	citizen, relay, err := r.authenticate(req, body)
	if err != nil {
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	switch req.Method {
	case http.MethodGet:
		r.show(w, citizen)
	case http.MethodPut:
		r.register(w, req.Context(), citizen, relay, body)
	case http.MethodDelete:
		r.mu.Lock()
		r.remove(citizen)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		r.test(w, req.Context(), citizen)
	}
}

// authenticate checks the NIP-98 header and returns the citizen of the
// request, and the wss URL of this relay as the request names it.
func (r *Rings) authenticate(req *http.Request, body []byte) (nostr.PubKey, string, error) {
	now := r.cfg.Now()
	event, err := turn.VerifyAuthBody(req.Header.Get("Authorization"), req.Method, req.Host, req.URL.Path, body, now)
	if err != nil {
		return nostr.PubKey{}, "", err
	}
	citizen := event.PubKey
	if encoded := req.Header.Get(TokenHeader); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nostr.PubKey{}, "", errors.New("the token is not base64")
		}
		var token nostr.Event
		if err := json.Unmarshal(data, &token); err != nil {
			return nostr.PubKey{}, "", errors.New("the token is not an event")
		}
		if citizen, _, err = devices.CheckToken(token, event.PubKey, nostr.Timestamp(now.Unix())); err != nil {
			return nostr.PubKey{}, "", err
		}
	}
	u, _ := url.Parse(event.Tags.Find("u")[1])
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	return citizen, scheme + "://" + u.Host, nil
}

func (r *Rings) show(w http.ResponseWriter, citizen nostr.PubKey) {
	r.mu.Lock()
	s := r.by[citizen]
	var status Status
	if s != nil {
		status = r.status(citizen, s)
	}
	r.mu.Unlock()
	if s == nil {
		fail(w, http.StatusNotFound, errNoRegistration)
		return
	}
	answer(w, http.StatusOK, status)
}

// status is the registration as GET shows it. The caller holds r.mu.
func (r *Rings) status(citizen nostr.PubKey, s *state) Status {
	return Status{Settings: s.Settings, Citizen: citizen.Hex(), Expires: s.Expires, LastRing: s.LastRing, LastStatus: s.LastStatus, Left: s.left(r.cfg.Now())}
}

// register checks and keeps a registration, until the TTL ends. A new
// registration keeps the count of the day and the bucket, so that it
// cannot reset the caps.
func (r *Rings) register(w http.ResponseWriter, ctx context.Context, citizen nostr.PubKey, relay string, body []byte) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, http.StatusBadRequest, "the body is not a registration: "+err.Error())
		return
	}
	if err := r.check(ctx, &request); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	sealed, err := r.seal(request.Key)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the relay cannot keep the key")
		return
	}
	now := r.cfg.Now()
	s := &state{Settings: request.Settings, SealedKey: sealed, Relay: relay, Created: now.Unix(), Expires: now.Add(r.cfg.TTL).Unix(), LastRead: now.Unix()}
	r.mu.Lock()
	if old := r.by[citizen]; old != nil {
		s.LastRing, s.LastStatus, s.Day, s.Used = old.LastRing, old.LastStatus, old.Day, old.Used
		s.Tokens, s.Refilled = old.Tokens, old.Refilled
	}
	r.stop(citizen)
	r.by[citizen] = s
	r.save(citizen, s)
	status := r.status(citizen, s)
	r.mu.Unlock()
	answer(w, http.StatusOK, status)
}

// check applies the rules of a registration, and fills in the defaults.
func (r *Rings) check(ctx context.Context, request *Request) error {
	if n := len(request.Key); n < MinKey || n > MaxKey {
		return fmt.Errorf("the key holds %d bytes; it must hold %d to %d", n, MinKey, MaxKey)
	}
	switch interval := request.MinInterval; {
	case interval == 0:
		request.MinInterval = DefaultInterval
	case interval < MinInterval || interval > MaxInterval:
		return fmt.Errorf("min_interval is %d; it must be %d to %d seconds", interval, MinInterval, MaxInterval)
	}
	switch b := request.Burst; {
	case b == 0:
		request.Burst = DefaultBurst
	case b < 1 || b > MaxBurst:
		return fmt.Errorf("burst is %d; it must be 1 to %d", b, MaxBurst)
	}
	switch c := request.DailyCap; {
	case c == 0:
		request.DailyCap = min(DefaultDailyCap, r.cfg.DailyCap)
	case c < 1 || c > r.cfg.DailyCap:
		return fmt.Errorf("daily_cap is %d; this relay takes 1 to %d", c, r.cfg.DailyCap)
	}
	switch request.RingOn {
	case "":
		request.RingOn = RingOnAny
	case RingOnAny:
	case RingOnStamped:
		return errors.New(errStampedNotBuilt)
	default:
		return fmt.Errorf("ring_on is %q; it must be %q or %q", request.RingOn, RingOnAny, RingOnStamped)
	}
	return r.checkURL(ctx, request.URL)
}

// checkURL refuses a ring URL that is not public https on an allowed port.
func (r *Rings) checkURL(ctx context.Context, raw string) error {
	if len(raw) > maxURL {
		return fmt.Errorf("the URL holds more than %d bytes", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("the url must be an https URL with a host, and no user or fragment")
	}
	port := defaultPort
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return fmt.Errorf("the port %q is not a number", p)
		}
	}
	if !slices.Contains(r.cfg.Ports, port) {
		return fmt.Errorf("the port %d is not one that this relay rings: %v", port, r.cfg.Ports)
	}
	_, err = r.resolve(ctx, u.Hostname())
	return err
}

// test rings once now. It counts against the cap of the day and of the
// relay, takes no token from the bucket of the citizen, so that it delays
// no ring of mail, and plans no retry.
func (r *Rings) test(w http.ResponseWriter, ctx context.Context, citizen nostr.PubKey) {
	now := r.cfg.Now()
	r.mu.Lock()
	s := r.by[citizen]
	switch {
	case s == nil:
		r.mu.Unlock()
		fail(w, http.StatusNotFound, errNoRegistration)
		return
	case s.left(now) == 0:
		r.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "the rings of today are used")
		return
	case r.busy[citizen]:
		r.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "a ring of this citizen is on the way; try again in a few seconds")
		return
	}
	if _, ok := r.relayToken(now); !ok {
		r.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "the relay rings too much now; try again in a minute")
		return
	}
	s.count(now)
	r.save(citizen, s)
	r.busy[citizen] = true
	snapshot := *s
	r.mu.Unlock()

	try := job{ID: ringID(), Since: snapshot.Since, Attempt: 1}
	if try.Since == 0 {
		try.Since = now.Unix()
	}
	status, err := r.ring(ctx, citizen, snapshot, try)
	r.mu.Lock()
	delete(r.busy, citizen)
	if s := r.by[citizen]; s != nil {
		s.LastStatus = status
		r.save(citizen, s)
		r.schedule(citizen)
	}
	r.mu.Unlock()
	answer(w, http.StatusOK, TestAnswer{Status: status, Error: errString(err)})
}

func answer(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, code int, message string) {
	answer(w, code, map[string]string{"error": message})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
