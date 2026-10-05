// Package direct holds what the two ends of a transfer share: the settings
// of the WebRTC connection, the messages of the ARC call, and the link of an
// offer.
package direct

import (
	"fmt"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	// ChunkSize is the size of one message on the data channel.
	ChunkSize = 16 << 10
	// The sender waits when more than BufferHigh bytes are in the buffer of
	// the data channel, until BufferLow bytes or less are there.
	BufferHigh = 8 << 20
	BufferLow  = 2 << 20

	// DefaultSTUN is the STUN server of an end that names none.
	DefaultSTUN = "stun:stun.l.google.com:19302"

	// OpenWait bounds the wait for the data channel of one attempt.
	OpenWait = 12 * time.Second
	// MaxHold bounds the wait that a receiver can ask for.
	MaxHold = 3 * time.Second
)

// Fetch is the body of the ARC call of a receiver.
type Fetch struct {
	Version int    `json:"v"`
	SHA256  string `json:"sha256"`
	Offset  int64  `json:"offset"`
	// HoldMS is the time that the sender waits before it sends its first
	// packet, in milliseconds. The receiver then sends first.
	HoldMS int                       `json:"hold_ms,omitempty"`
	SDP    webrtc.SessionDescription `json:"sdp"`
}

// Answer is the reply of the sender.
type Answer struct {
	Version int                       `json:"v"`
	SDP     webrtc.SessionDescription `json:"sdp"`
}

// Options are the settings of one end of a connection.
type Options struct {
	// STUN is the URL of the STUN server. If it is empty, the end uses the
	// addresses of the machine only.
	STUN string
	// Loopback adds the loopback address, for two ends on one machine.
	Loopback bool
	// MinWindow keeps the SCTP send window at or above this number of bytes
	// after a loss. Zero keeps the default of pion.
	MinWindow uint32
}

// STUNURL is the URL of a STUN setting. The value "none" gives no server,
// and an empty value gives the default server.
func STUNURL(value string) string {
	switch value {
	case "":
		return DefaultSTUN
	case "none":
		return ""
	}
	return value
}

// virtualInterfaces are the name prefixes of interfaces that an end does
// not use. A UDP write on a bridge of a virtual machine can block with no
// end, and pion then stops all ICE work.
var virtualInterfaces = []string{"bridge", "vmenet", "utun", "awdl", "llw", "docker", "veth", "br-", "wg"}

// NewConnection makes one end of a connection.
func NewConnection(o Options) (*webrtc.PeerConnection, error) {
	var engine webrtc.SettingEngine
	engine.SetInterfaceFilter(func(name string) bool {
		for _, prefix := range virtualInterfaces {
			if strings.HasPrefix(name, prefix) {
				return false
			}
		}
		return true
	})
	// STUN on IPv6 with no route adds seconds to the gathering.
	engine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	engine.SetSTUNGatherTimeout(2 * time.Second)
	// pion stops after 8 pings for each pair by default, about 1.6 seconds.
	// The other end can start one second later, so ping for 6 seconds. An
	// attempt that finds no path fails after 8 seconds, not after 30.
	engine.SetICEMaxBindingRequests(30)
	engine.SetICETimeouts(4*time.Second, 4*time.Second, time.Second)
	engine.SetIncludeLoopbackCandidate(o.Loopback)
	if o.MinWindow > 0 {
		engine.SetSCTPMinCwnd(o.MinWindow)
		engine.SetSCTPFastRtxWnd(o.MinWindow)
	}
	config := webrtc.Configuration{}
	if o.STUN != "" {
		config.ICEServers = []webrtc.ICEServer{{URLs: []string{o.STUN}}}
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(engine)).NewPeerConnection(config)
}

// Describe sets a local description, waits for the addresses of this end,
// and returns the description with those addresses.
func Describe(pc *webrtc.PeerConnection, local webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(local); err != nil {
		return local, err
	}
	<-gathered
	return *pc.LocalDescription(), nil
}

// SelectedPath names the kinds of the two addresses that ICE selected:
// "host" is an address of the machine, "srflx" is an address behind a NAT.
func SelectedPath(pc *webrtc.PeerConnection) string {
	pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "unknown"
	}
	return fmt.Sprintf("%s to %s", pair.Local.Typ, pair.Remote.Typ)
}

// HoldCandidates takes the addresses out of an offer, and returns them. An
// answerer that holds them sends no packet until it gives them to ICE, so
// the offerer sends first.
func HoldCandidates(offer *webrtc.SessionDescription) []webrtc.ICECandidateInit {
	var held []webrtc.ICECandidateInit
	var kept []string
	mid, index := "0", uint16(0)
	for line := range strings.SplitSeq(offer.SDP, "\r\n") {
		switch {
		case strings.HasPrefix(line, "a=candidate:"):
			held = append(held, webrtc.ICECandidateInit{Candidate: strings.TrimPrefix(line, "a="), SDPMid: &mid, SDPMLineIndex: &index})
		case line == "a=end-of-candidates":
		default:
			kept = append(kept, line)
		}
	}
	offer.SDP = strings.Join(kept, "\r\n")
	return held
}
