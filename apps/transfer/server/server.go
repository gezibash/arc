// Package server gives the offered files of a citizen to its callers on
// direct connections.
//
// A request is one JSON object, direct.Fetch: the SHA-256 of an offer, an
// offset, and a WebRTC offer. The reply is direct.Answer, with the WebRTC
// answer. The bytes of the file then go on a data channel between the two
// programs, not through ARC.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/apps/transfer/direct"
	"github.com/gezibash/arc/sdk/provider"
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
)

type server struct {
	state string
	rtc   direct.Options
	slots chan struct{}
	log   io.Writer
}

// Run serves the offers of the state directory through core ARC. The
// environment gives the settings:
//
//	TRANSFER_STATE           the state directory
//	TRANSFER_STUN            the STUN server, or "none"
//	TRANSFER_MIN_WINDOW_KIB  the smallest send window after a loss
//	TRANSFER_LOOPBACK        if set, use the loopback address too
func Run(ctx context.Context, opts provider.Options) error {
	window, err := strconv.ParseUint(os.Getenv("TRANSFER_MIN_WINDOW_KIB"), 10, 22)
	if err != nil && os.Getenv("TRANSFER_MIN_WINDOW_KIB") != "" {
		return errors.New("TRANSFER_MIN_WINDOW_KIB is not a number of KiB")
	}
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	s := &server{
		state: StateDir(""),
		rtc: direct.Options{
			STUN:      direct.STUNURL(os.Getenv("TRANSFER_STUN")),
			Loopback:  os.Getenv("TRANSFER_LOOPBACK") != "",
			MinWindow: uint32(window) << 10,
		},
		slots: make(chan struct{}, MaxTransfers),
		log:   log,
	}
	return provider.Run(ctx, s, opts)
}

// HandleRequest answers one call: it checks the offer, opens its end of the
// connection, and returns the answer. The file goes when the data channel
// opens.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var fetch direct.Fetch
	if json.Unmarshal([]byte(request.Message), &fetch) != nil || fetch.Version != 1 ||
		!direct.IsHex64(fetch.SHA256) || fetch.Offset < 0 || fetch.HoldMS < 0 ||
		fetch.SDP.Type != webrtc.SDPTypeOffer {
		return "", errInvalidRequest
	}
	hold := time.Duration(fetch.HoldMS) * time.Millisecond
	if hold > direct.MaxHold {
		return "", errInvalidRequest
	}
	offer, err := load(s.state, fetch.SHA256)
	if err != nil || (len(offer.To) > 0 && !slices.Contains(offer.To, request.From)) {
		return "", errUnknownOffer
	}
	info, err := os.Stat(offer.Path)
	if err != nil || info.Size() != offer.Size || !info.ModTime().Equal(offer.Modified) {
		return "", errFileChanged
	}
	if fetch.Offset > offer.Size {
		return "", errInvalidRequest
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return "", errBusy
	}

	pc, err := direct.NewConnection(s.rtc)
	if err != nil {
		<-s.slots
		return "", errFailed
	}
	// Close reports the closed state to the callback below, which calls end
	// again. A sync.Once would wait for itself there.
	var ended atomic.Bool
	end := func() {
		if ended.CompareAndSwap(false, true) {
			<-s.slots
			_ = pc.Close()
		}
	}
	// An end that never connects must not keep its slot.
	unopened := time.AfterFunc(direct.OpenWait+hold, end)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			end()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		received := make(chan struct{})
		var once sync.Once
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			// The receiver says "ok" after it has the last byte.
			if m.IsString && string(m.Data) == "ok" {
				once.Do(func() { close(received) })
			}
		})
		dc.OnOpen(func() {
			unopened.Stop()
			go func() {
				defer end()
				started := time.Now()
				label := fmt.Sprintf("%.12s to %.8s", offer.SHA256, request.From)
				if err := push(dc, offer.Path, fetch.Offset, offer.Size); err != nil {
					fmt.Fprintf(s.log, "%s stopped: %v\n", label, err)
					return
				}
				select {
				case <-received:
					fmt.Fprintf(s.log, "%s: %d bytes in %.1f s on %s\n", label, offer.Size-fetch.Offset, time.Since(started).Seconds(), direct.SelectedPath(pc))
					if err := recordDelivery(s.state, offer.SHA256, request.From); err != nil {
						fmt.Fprintf(s.log, "%s: the delivery is not recorded: %v\n", label, err)
					}
				case <-time.After(30 * time.Second):
					fmt.Fprintf(s.log, "%s: the receiver did not confirm\n", label)
				}
			}()
		})
	})

	remote := fetch.SDP
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
	raw, err := json.Marshal(direct.Answer{Version: 1, SDP: local})
	if err != nil {
		end()
		return "", errFailed
	}
	return string(raw), nil
}

// push sends the bytes of the file from offset to size, and then the text
// "end". It waits when the buffer of the data channel is full.
func push(dc *webrtc.DataChannel, path string, offset, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(direct.BufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})
	closed := make(chan struct{})
	dc.OnClose(func() { close(closed) })

	block := make([]byte, direct.ChunkSize)
	for sent := offset; sent < size; {
		n, err := io.ReadFull(f, block[:min(int64(direct.ChunkSize), size-sent)])
		if err != nil {
			return err
		}
		if err := dc.Send(block[:n]); err != nil {
			return err
		}
		sent += int64(n)
		if dc.BufferedAmount() > direct.BufferHigh {
			select {
			case <-low:
			case <-closed:
				return errors.New("the receiver closed the channel")
			}
		}
	}
	return dc.SendText("end")
}
