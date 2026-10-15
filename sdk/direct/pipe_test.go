package direct

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

// pipePair dials from offerer to answerer, with Accept in the open
// function, as a session would carry the offer and the answer. serve gets
// the pipe of the answerer after its data channel opens.
func pipePair(t *testing.T, offerer DialConfig, answerer AcceptConfig, serve func(*Pipe)) (*Pipe, error) {
	t.Helper()
	ctx := t.Context()
	return Dial(ctx, offerer, func(ctx context.Context, o Offer) (string, error) {
		p, answer, err := Accept(ctx, answerer, o)
		if err != nil {
			return "", err
		}
		go func() {
			if p.Wait(ctx) == nil {
				serve(p)
			}
		}()
		return answer, nil
	})
}

// TestPipe sends 8 MiB on a pipe between two ends on this machine. The
// reader gets each byte and then io.EOF, and the writer learns that the
// reader has the last byte.
func TestPipe(t *testing.T) {
	data := make([]byte, 8<<20)
	_, _ = rand.Read(data)
	local := Options{Loopback: true}
	acked := make(chan error, 1)
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: local}, AcceptConfig{Policy: Auto, Options: local}, func(p *Pipe) {
		defer p.Close()
		if _, err := p.Write(data); err != nil {
			acked <- err
			return
		}
		acked <- p.CloseWrite(t.Context())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if path := pipe.Path(); path != "direct (host to host)" {
		t.Errorf("path %q, want direct (host to host)", path)
	}
	got, err := io.ReadAll(pipe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, not the %d bytes that were written", len(got), len(data))
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer got no ok")
	}
}

// TestPipeClosedEarly closes the writer after part of the bytes. The reader
// gets ErrPipeClosed, not io.EOF, so it cannot take a short body as all of
// it.
func TestPipeClosedEarly(t *testing.T) {
	local := Options{Loopback: true}
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: local}, AcceptConfig{Options: local}, func(p *Pipe) {
		_, _ = p.Write(make([]byte, 64<<10))
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	got, err := io.ReadAll(pipe)
	if !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("read %d bytes and %v, want ErrPipeClosed", len(got), err)
	}
}

// TestPipeOpenedThenDead gives Wait a pipe that died before its open
// handler ran, as when the other end opens, writes and closes at once. A
// channel on a connected SCTP association found a path, so Wait returns nil
// and Dial makes no second attempt. A channel that dies before the
// association connects has no path.
func TestPipeOpenedThenDead(t *testing.T) {
	local := Options{Loopback: true}
	open, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: local}, AcceptConfig{Options: local}, func(*Pipe) {})
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	opened := newPipe(open.pc, 0)
	opened.dc.Store(open.dc.Load())
	opened.die()
	if err := opened.Wait(t.Context()); err != nil {
		t.Errorf("opened, then dead: Wait = %v, want nil", err)
	}

	pc, err := NewConnection(local)
	if err != nil {
		t.Fatal(err)
	}
	never := newPipe(pc, 0)
	dc, err := pc.CreateDataChannel("pipe", nil)
	if err != nil {
		t.Fatal(err)
	}
	never.attach(dc)
	never.die()
	if err := never.Wait(t.Context()); !errors.Is(err, ErrNoPath) {
		t.Errorf("dead before the association: Wait = %v, want ErrNoPath", err)
	}
}

// TestPipeAnswererRelayOnly gives the answerer the policy turn: its answer
// holds only an address on the TURN server, and the bytes go through the
// relay. Both ends are behind symmetric NATs.
func TestPipeAnswererRelayOnly(t *testing.T) {
	a, b, relay := natPair(t)
	const stun = "stun:1.2.3.4:3478"
	turn := func(context.Context) (Credential, error) {
		c, err := relay.Credential("0000000000000000000000000000000000000000000000000000000000000003", "1.2.3.4")
		return Credential{URLs: c.URLs, Username: c.Username, Password: c.Password, QuotaLeft: 1 << 30}, err
	}
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: Options{STUN: stun, Net: a}},
		AcceptConfig{Policy: RelayOnly, Options: Options{STUN: stun, Net: b}, TURN: turn}, func(p *Pipe) {
			defer p.Close()
			if _, err := p.Write(make([]byte, 256<<10)); err == nil {
				_ = p.CloseWrite(t.Context())
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if path := pipe.Path(); path != "turn (relay)" {
		t.Errorf("path %q, want turn (relay)", path)
	}
	if n, err := io.Copy(io.Discard, pipe); err != nil || n != 256<<10 {
		t.Fatalf("read %d bytes, %v", n, err)
	}
}

func TestParsePolicy(t *testing.T) {
	for value, want := range map[string]Policy{"": Auto, "auto": Auto, "direct": DirectOnly, "turn": RelayOnly, "session": Off} {
		if got, err := ParsePolicy(value); err != nil || got != want {
			t.Errorf("ParsePolicy(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := ParsePolicy("relay"); err == nil {
		t.Error("ParsePolicy took relay")
	}
}

// fingerprintLine matches the DTLS fingerprint attribute of an SDP.
var fingerprintLine = regexp.MustCompile(`(?m)^a=fingerprint:\S+ \S+`)

// fingerprintOf returns the fingerprint attribute of the first line of an
// SDP, or fails the test.
func fingerprintOf(t *testing.T, sdp string) string {
	t.Helper()
	line := fingerprintLine.FindString(sdp)
	if line == "" {
		t.Fatalf("no fingerprint in the SDP:\n%s", sdp)
	}
	return line
}

// withFingerprint returns sdp with every fingerprint attribute replaced.
func withFingerprint(sdp, line string) string {
	return fingerprintLine.ReplaceAllLiteralString(sdp, line)
}

// TestPipeFingerprint checks that a pipe opens only if the certificate of
// the other end has the fingerprint of the signed description. Each case
// sends the answer or the offer through the open function, where the ARC
// call would carry it, and changes it there.
func TestPipeFingerprint(t *testing.T) {
	local := Options{Loopback: true}

	type attempt struct {
		pipe     *Pipe // the dialer's pipe
		err      error
		accepted chan error // the result of the Wait on the answerer
	}
	run := func(t *testing.T, change func(t *testing.T, o *Offer, answer *string, other string)) attempt {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		t.Cleanup(cancel)
		a := attempt{accepted: make(chan error, 1)}
		a.pipe, a.err = Dial(ctx, DialConfig{Policy: DirectOnly, Options: local}, func(ctx context.Context, o Offer) (string, error) {
			// A second end with its own certificate: the third party.
			other, otherAnswer, err := Accept(ctx, AcceptConfig{Options: local}, o)
			if err != nil {
				return "", err
			}
			t.Cleanup(func() { _ = other.Close() })
			p, answer, err := Accept(ctx, AcceptConfig{Options: local}, o)
			if err != nil {
				return "", err
			}
			t.Cleanup(func() { _ = p.Close() })
			change(t, &o, &answer, otherAnswer)
			go func() { a.accepted <- p.Wait(ctx) }()
			return answer, nil
		})
		if a.pipe != nil {
			t.Cleanup(func() { _ = a.pipe.Close() })
		}
		return a
	}

	t.Run("correct fingerprint connects", func(t *testing.T) {
		a := run(t, func(*testing.T, *Offer, *string, string) {})
		if a.err != nil {
			t.Fatalf("a pipe with the right fingerprint failed: %v", a.err)
		}
		if err := <-a.accepted; err != nil {
			t.Fatalf("the answerer: %v", err)
		}
	})

	t.Run("answer with the fingerprint of another certificate", func(t *testing.T) {
		// The signed answer says the certificate of the third party, but the
		// end that answers has its own certificate.
		a := run(t, func(t *testing.T, _ *Offer, answer *string, other string) {
			*answer = withFingerprint(*answer, fingerprintOf(t, other))
		})
		if a.err == nil {
			t.Fatal("the dialer opened a pipe to a certificate that has another fingerprint")
		}
		t.Logf("dialer error: %v", a.err)
		if err := <-a.accepted; err == nil {
			t.Error("the answerer opened a pipe to a dialer that refused its certificate")
		}
	})

	t.Run("answer with a changed fingerprint", func(t *testing.T) {
		a := run(t, func(t *testing.T, _ *Offer, answer *string, _ string) {
			zero := strings.Repeat("00:", 31) + "00"
			*answer = withFingerprint(*answer, "a=fingerprint:sha-256 "+zero)
		})
		if a.err == nil {
			t.Fatal("the dialer opened a pipe to a certificate that has a changed fingerprint")
		}
		t.Logf("dialer error: %v", a.err)
	})
}
