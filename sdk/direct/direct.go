// Package direct opens a pipe between two citizens: a WebRTC data channel
// that carries bulk bytes outside the relay. One ARC call carries the offer
// and the answer. If no direct path opens, the pipe goes through the TURN
// server of a relay. docs/direct/SPEC.md defines the rules.
package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/webrtc/v4"
)

const (
	// ChunkSize is the size of one message on the data channel.
	ChunkSize = 16 << 10
	// The sender waits when more than BufferHigh bytes are in the buffer of
	// the data channel, until BufferLow bytes or less are there.
	BufferHigh = 8 << 20
	BufferLow  = 2 << 20

	// RelaySTUNPort is the port on which a relay answers STUN. An end that
	// names no STUN server asks the host of each of its relays on this port.
	RelaySTUNPort = "3478"

	// OpenWait bounds the wait for the data channel of one attempt.
	OpenWait = 12 * time.Second
	// MaxHold bounds the wait that a receiver can ask for.
	MaxHold = 3 * time.Second
)

// Options are the settings of one end of a connection.
type Options struct {
	// STUN is the URL of the STUN server. If it and MoreSTUN are empty,
	// the end uses the addresses of the machine only.
	STUN string
	// MoreSTUN are more STUN servers, such as those of the other relays of
	// the citizen. The end asks each server at once. A server that does not
	// answer delays the setup until the gather timeout, and does not stop
	// it while another one answers.
	MoreSTUN []string
	// TURN is the TURN server of a relay. If it is set, the end uses only
	// an address on that server, and the bytes go through the relay. An
	// end sets it only after the direct attempts failed.
	TURN *webrtc.ICEServer
	// PeerSTUN is a second STUN server, which the other end named in its
	// request.
	PeerSTUN string
	// Loopback adds the loopback address, for two ends on one machine.
	Loopback bool
	// MinWindow keeps the SCTP send window at or above this number of bytes
	// after a loss. Zero keeps the default of pion.
	MinWindow uint32

	// Net replaces the network of the machine. A test sets it for an
	// emulated link. Nil uses the network of the machine.
	Net transport.Net

	// gather keeps why the ICE agent of a relay attempt gathered no address
	// on the TURN server. RelayOptions sets it.
	gather *gatherLog
}

// sctpCAStep is the growth of the SCTP send window for each round trip
// after a loss. The default of pion is one packet, about 1.2 KiB, so the
// window needs seconds to grow back. A loss still halves the window.
// Larger steps lost speed on a link with a short queue; see
// docs/direct/SPEC.md, section 3.3.
var sctpCAStep uint32 = 8 << 10

// STUNURLs are the URLs of a STUN setting. The value "none" gives no
// server. An empty value gives the STUN server of each relay, in the order
// of the relays. Another value is the one STUN URL.
func STUNURLs(value string, relays []string) []string {
	switch value {
	case "none":
		return nil
	case "":
		var urls []string
		for _, relay := range relays {
			if stun := RelaySTUN(relay); stun != "" && !slices.Contains(urls, stun) {
				urls = append(urls, stun)
			}
		}
		return urls
	}
	return []string{value}
}

// STUNURL is the first of STUNURLs, or empty.
func STUNURL(value string, relays []string) string {
	urls := STUNURLs(value, relays)
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}

// RelaySTUN is the STUN URL of the host of a relay URL, or empty if the
// URL is not a ws:// or wss:// URL with a host.
func RelaySTUN(relay string) string {
	u, err := url.Parse(relay)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" {
		return ""
	}
	return "stun:" + net.JoinHostPort(u.Hostname(), RelaySTUNPort)
}

// TURNSTUN is the STUN URL of a TURN server, with its IPv4 address. A TURN
// server of a relay answers STUN on its own port. The other end gets the
// address, not the host name, so it does not depend on its own DNS.
func TURNSTUN(ctx context.Context, server *webrtc.ICEServer) (string, error) {
	if server == nil || len(server.URLs) == 0 {
		return "", errors.New("no TURN server")
	}
	rest, ok := strings.CutPrefix(server.URLs[0], "turn:")
	if !ok {
		return "", fmt.Errorf("%q is not a turn: URL", server.URLs[0])
	}
	rest, _, _ = strings.Cut(rest, "?")
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return "", err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	return "stun:" + net.JoinHostPort(ips[0].String(), port), nil
}

// IsIPSTUN says if stun is a STUN URL with an IPv4 address and a port.
func IsIPSTUN(stun string) bool {
	rest, ok := strings.CutPrefix(stun, "stun:")
	if !ok {
		return false
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	ip := net.ParseIP(host)
	return err == nil && n > 0 && ip != nil && ip.To4() != nil
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
	if o.Net != nil {
		engine.SetNet(o.Net)
	}
	if o.gather != nil {
		engine.LoggerFactory = o.gather
	}
	engine.SetSCTPCwndCAStep(sctpCAStep)
	if o.MinWindow > 0 {
		engine.SetSCTPMinCwnd(o.MinWindow)
		engine.SetSCTPFastRtxWnd(o.MinWindow)
	}
	config := webrtc.Configuration{}
	if o.TURN != nil {
		config.ICEServers = []webrtc.ICEServer{*o.TURN}
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	} else {
		var servers []string
		for _, stun := range slices.Concat([]string{o.STUN}, o.MoreSTUN, []string{o.PeerSTUN}) {
			if stun != "" && !slices.Contains(servers, stun) {
				servers = append(servers, stun)
				config.ICEServers = append(config.ICEServers, webrtc.ICEServer{URLs: []string{stun}})
			}
		}
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
// It is "relay" if the bytes go through the TURN server of a relay.
func SelectedPath(pc *webrtc.PeerConnection) string {
	pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "unknown"
	}
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		return "relay"
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
