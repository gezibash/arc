// Package server gives the offered files of a citizen to its callers on
// direct connections, and takes the files that callers give to it.
//
// A request is one JSON object, files.Fetch: the SHA-256 of a file, and a
// WebRTC offer. A fetch asks for an offered file. A put gives a file. The
// reply is files.Answer, with the WebRTC answer. The bytes of the file then
// go on a data channel between the two programs, not through ARC.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/apps/transfer/files"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/pion/webrtc/v4"
)

// MaxTransfers is the number of transfers that one sender runs at a time.
const MaxTransfers = 4

// The errors that a caller may see. An offer for another citizen looks like
// no offer, so a caller cannot learn which files a sender offers to others.
const (
	errInvalidRequest = provider.Error("invalid_request")
	errUnknownOffer   = provider.Error("unknown_offer")
	errFileChanged    = provider.Error("file_changed")
	errBusy           = provider.Error("busy")
	errFailed         = provider.Error("transfer_failed")
	errPutRefused     = provider.Error("put_refused")
	errTooLarge       = provider.Error("too_large")
)

type server struct {
	state string
	rtc   direct.Options
	slots chan struct{}
	log   io.Writer
	// putMax is the largest file that a caller can give. Zero refuses each
	// put.
	putMax int64
	// putting holds the files of the puts that run, so that two puts do not
	// write one part file.
	putting sync.Map
}

// Run serves the offers of the state directory through core ARC. The
// environment gives the settings:
//
//	TRANSFER_STATE           the state directory
//	TRANSFER_STUN            the STUN server, or "none"; unset asks the first
//	                         relay of ARC_RELAYS on port 3478
//	TRANSFER_MIN_WINDOW_KIB  the smallest send window after a loss
//	TRANSFER_LOOPBACK        if set, use the loopback address too
//	TRANSFER_PUT_MAX_MIB     the largest file that a caller can give; unset
//	                         or 0 refuses each put
func Run(ctx context.Context, opts provider.Options) error {
	window, err := strconv.ParseUint(os.Getenv("TRANSFER_MIN_WINDOW_KIB"), 10, 22)
	if err != nil && os.Getenv("TRANSFER_MIN_WINDOW_KIB") != "" {
		return errors.New("TRANSFER_MIN_WINDOW_KIB is not a number of KiB")
	}
	putMax, err := strconv.ParseInt(os.Getenv("TRANSFER_PUT_MAX_MIB"), 10, 32)
	if (err != nil && os.Getenv("TRANSFER_PUT_MAX_MIB") != "") || putMax < 0 {
		return errors.New("TRANSFER_PUT_MAX_MIB is not a number of MiB")
	}
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	s := &server{
		state: StateDir(""),
		rtc: direct.Options{
			STUN:      direct.STUNURL(os.Getenv("TRANSFER_STUN"), strings.Fields(os.Getenv("ARC_RELAYS"))),
			Loopback:  os.Getenv("TRANSFER_LOOPBACK") != "",
			MinWindow: uint32(window) << 10,
		},
		slots:  make(chan struct{}, MaxTransfers),
		log:    log,
		putMax: putMax << 20,
	}
	return provider.Run(ctx, s, opts)
}

// HandleRequest answers one call: it checks the request, opens its end of
// the connection, and returns the answer. The file goes when the data
// channel opens.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var req files.Fetch
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 ||
		!files.IsHex64(req.SHA256) || req.Offset < 0 || req.HoldMS < 0 ||
		time.Duration(req.HoldMS)*time.Millisecond > direct.MaxHold ||
		req.SDP.Type != webrtc.SDPTypeOffer || (req.STUN != "" && !direct.IsIPSTUN(req.STUN)) {
		return "", errInvalidRequest
	}
	switch req.Op {
	case "":
		return s.fetch(request.From, req)
	case files.OpPut:
		return s.put(request.From, req)
	}
	return "", errInvalidRequest
}

// fetch gives an offered file to the caller.
func (s *server) fetch(from string, req files.Fetch) (string, error) {
	offer, err := load(s.state, req.SHA256)
	if err != nil || (len(offer.To) > 0 && !slices.Contains(offer.To, from)) {
		return "", errUnknownOffer
	}
	info, err := os.Stat(offer.Path)
	if err != nil || info.Size() != offer.Size || !info.ModTime().Equal(offer.Modified) {
		return "", errFileChanged
	}
	if req.Offset > offer.Size {
		return "", errInvalidRequest
	}
	if !s.takeSlot() {
		return "", errBusy
	}
	return s.connect(req, 0, func() { <-s.slots }, func(pc *webrtc.PeerConnection, dc *webrtc.DataChannel, end func()) func() {
		received := make(chan struct{})
		refused := make(chan string, 1)
		var once sync.Once
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			// The receiver says "ok" after the size and the SHA-256 of all
			// the bytes pass, and the reason when they do not.
			if !m.IsString {
				return
			}
			once.Do(func() {
				if string(m.Data) == "ok" {
					close(received)
					return
				}
				refused <- string(m.Data)
			})
		})
		return func() {
			go func() {
				defer end()
				started := time.Now()
				label := fmt.Sprintf("%.12s to %.8s", offer.SHA256, from)
				if err := files.Send(dc, offer.Path, req.Offset, offer.Size); err != nil {
					fmt.Fprintf(s.log, "%s stopped: %s\n", label, terminal.EscapeLine(err.Error()))
					return
				}
				select {
				case <-received:
					path := direct.SelectedPath(pc)
					fmt.Fprintf(s.log, "%s: %d bytes in %.1f s on %s\n", label, offer.Size-req.Offset, time.Since(started).Seconds(), path)
					if err := recordDelivery(s.state, offer.SHA256, from, path); err != nil {
						fmt.Fprintf(s.log, "%s: the delivery is not recorded: %s\n", label, terminal.EscapeLine(err.Error()))
					}
				case reason := <-refused:
					fmt.Fprintf(s.log, "%s: the receiver refused the bytes: %s\n", label, terminal.EscapeLine(strings.TrimPrefix(reason, "refused: ")))
				case <-time.After(confirmWait(s.state)):
					fmt.Fprintf(s.log, "%s: the receiver did not confirm\n", label)
				}
			}()
		}
	})
}

// put takes a file that the caller gives. The file goes to
// received/<key of the caller>/<sha256> in the state directory. A part file
// stays after a stop, and the next put of the caller gets only the rest.
func (s *server) put(from string, req files.Fetch) (string, error) {
	if s.putMax == 0 {
		return "", errPutRefused
	}
	if req.Size < 0 || req.Size > s.putMax {
		return "", errTooLarge
	}
	file := files.ReceivedFile(s.state, from, req.SHA256)
	if _, err := os.Stat(file); err == nil {
		raw, err := json.Marshal(files.Answer{Version: 1, Offset: req.Size})
		return string(raw), err
	}
	if _, running := s.putting.LoadOrStore(file, true); running {
		return "", errBusy
	}
	if !s.takeSlot() {
		s.putting.Delete(file)
		return "", errBusy
	}
	var part *files.Part
	err := os.MkdirAll(filepath.Dir(file), 0o700)
	if err == nil {
		part, err = files.OpenPart(file, req.SHA256, req.Size)
	}
	if err != nil {
		<-s.slots
		s.putting.Delete(file)
		return "", errFailed
	}
	release := func() {
		part.Close()
		<-s.slots
		s.putting.Delete(file)
	}
	return s.connect(req, part.Have(), release, func(pc *webrtc.PeerConnection, dc *webrtc.DataChannel, end func()) func() {
		started, have := time.Now(), part.Have()
		label := fmt.Sprintf("%.12s from %.8s", req.SHA256, from)
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !m.IsString {
				if err := part.Write(m.Data); err != nil {
					fmt.Fprintf(s.log, "%s stopped: %s\n", label, terminal.EscapeLine(err.Error()))
					if errors.Is(err, files.ErrTooManyBytes) {
						part.Discard()
					}
					_ = dc.SendText(files.Refusal(err))
					end()
				}
				return
			}
			if string(m.Data) != "end" {
				end()
				return
			}
			// The caller closes the connection when it has the answer.
			if beforePutFinish(s.state) {
				end() // a test build can act as an app that is gone
				return
			}
			if err := part.Finish(); err != nil {
				fmt.Fprintf(s.log, "%s: %s\n", label, terminal.EscapeLine(err.Error()))
				_ = dc.SendText(files.Refusal(err))
			} else {
				fmt.Fprintf(s.log, "%s: %d bytes in %.1f s on %s\n", label, req.Size-have, time.Since(started).Seconds(), direct.SelectedPath(pc))
				_ = dc.SendText("ok")
			}
			time.AfterFunc(5*time.Second, end)
		})
		return nil
	})
}

func (s *server) takeSlot() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// connect opens the end of the app for the WebRTC offer of a request, and
// returns the reply with the answer. offset goes in the reply. release runs
// one time, when the connection ends. setup gets the data channel of the
// caller, and returns what to do when it opens, or nil.
func (s *server) connect(req files.Fetch, offset int64, release func(),
	setup func(*webrtc.PeerConnection, *webrtc.DataChannel, func()) func()) (string, error) {
	rtc := s.rtc
	rtc.PeerSTUN = req.STUN
	pc, err := direct.NewConnection(rtc)
	if err != nil {
		release()
		return "", errFailed
	}
	// Close reports the closed state to the callback below, which calls end
	// again. A sync.Once would wait for itself there.
	var ended atomic.Bool
	end := func() {
		if ended.CompareAndSwap(false, true) {
			release()
			_ = pc.Close()
		}
	}
	hold := time.Duration(req.HoldMS) * time.Millisecond
	// An end that never connects must not keep its slot.
	unopened := time.AfterFunc(direct.OpenWait+hold, end)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			end()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		opened := setup(pc, dc, end)
		dc.OnOpen(func() {
			unopened.Stop()
			if opened != nil {
				opened()
			}
		})
	})

	remote := req.SDP
	var held []webrtc.ICECandidateInit
	if hold > 0 {
		held = direct.HoldCandidates(&remote)
	}
	if err := pc.SetRemoteDescription(remote); err != nil {
		end()
		return "", errInvalidRequest
	}
	if hold > 0 {
		time.AfterFunc(hold, func() {
			for _, candidate := range held {
				_ = pc.AddICECandidate(candidate)
			}
		})
	}
	local, err := pc.CreateAnswer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		end()
		return "", errFailed
	}
	raw, err := json.Marshal(files.Answer{Version: 1, Offset: offset, SDP: local})
	if err != nil {
		end()
		return "", errFailed
	}
	return string(raw), nil
}
