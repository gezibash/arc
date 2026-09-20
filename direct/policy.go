package direct

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// A citizen carries a conversation off the relay only where its owner said
// so. The policy file names each peer, the capability, and the literal
// addresses that this machine may dial or listen on.
//
//	{"version": 1, "rules": [
//	  {"peer": "<64 hex>", "capability": "primary", "scheme": "exec",
//	   "path": "/", "lease_ms": 30000, "dial": ["203.0.113.4"],
//	   "listen": {"bind": "0.0.0.0", "address": "203.0.113.9", "port": 0}}
//	]}
//
// A rule needs somewhere to dial, or somewhere to listen, or both.
const (
	// MaxPolicyBytes caps the file.
	MaxPolicyBytes = 32 * 1024
	// MaxRules is how many rules one citizen holds.
	MaxRules = 32
	// MaxDialAddresses is how many addresses one rule may dial.
	MaxDialAddresses = 4
	// DefaultLeaseMS is how long a route lives without a renewal.
	DefaultLeaseMS = 30_000
	// MinLeaseMS and MaxLeaseMS bound a lease.
	MinLeaseMS = 1_000
	MaxLeaseMS = 120_000
)

// Errors of the policy.
var (
	ErrInvalidPolicy = errors.New("direct: the policy does not hold together")
	ErrAddressDenied = errors.New("direct: the policy does not allow that address")
	ErrNoRule        = errors.New("direct: no rule allows this conversation")
)

var capabilityPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Endpoint is where a citizen waits for its peer.
type Endpoint struct {
	// Bind is the address of this machine to listen on.
	Bind netip.Addr
	// Address is what the peer dials. It may differ from Bind behind a
	// router.
	Address netip.Addr
	// Port is the port to listen on. Zero takes a free one.
	Port int
}

// Rule is one permission of the owner.
type Rule struct {
	Peer       []byte
	Capability string
	Scheme     string
	Path       string
	LeaseMS    int
	Dial       []netip.Addr
	HolePunch  bool
	Listen     *Endpoint
}

type policyFile struct {
	Version int          `json:"version"`
	Rules   []policyRule `json:"rules"`
}

type policyRule struct {
	Peer       string   `json:"peer"`
	Capability string   `json:"capability"`
	Scheme     string   `json:"scheme"`
	Path       string   `json:"path"`
	LeaseMS    *int     `json:"lease_ms"`
	Dial       []string `json:"dial"`
	HolePunch  bool     `json:"hole_punch"`
	Listen     *struct {
		Bind    string `json:"bind"`
		Address string `json:"address"`
		Port    int    `json:"port"`
	} `json:"listen"`
}

// LoadPolicy reads the rules of the owner.
func LoadPolicy(path string) ([]Rule, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxPolicyBytes {
		return nil, ErrInvalidPolicy
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrInvalidPolicy
	}
	return DecodePolicy(data)
}

// DecodePolicy reads the rules out of one document.
func DecodePolicy(data []byte) ([]Rule, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	var file policyFile
	if err := decoder.Decode(&file); err != nil || decoder.More() {
		return nil, ErrInvalidPolicy
	}
	if file.Version != 1 || len(file.Rules) > MaxRules {
		return nil, ErrInvalidPolicy
	}

	rules := make([]Rule, 0, len(file.Rules))
	seen := map[string]bool{}

	for _, given := range file.Rules {
		rule, err := readRule(given)
		if err != nil {
			return nil, err
		}

		key := fmt.Sprintf("%x|%s|%s|%s", rule.Peer, rule.Capability, rule.Scheme, rule.Path)
		if seen[key] {
			return nil, ErrInvalidPolicy
		}
		seen[key] = true

		rules = append(rules, rule)
	}
	return rules, nil
}

func readRule(given policyRule) (Rule, error) {
	peer, err := hex.DecodeString(strings.ToLower(given.Peer))
	if err != nil || len(peer) != 32 {
		return Rule{}, ErrInvalidPolicy
	}
	if !capabilityPattern.MatchString(given.Capability) {
		return Rule{}, ErrInvalidPolicy
	}
	if given.Scheme == "" || !strings.HasPrefix(given.Path, "/") {
		return Rule{}, ErrInvalidPolicy
	}

	lease := DefaultLeaseMS
	if given.LeaseMS != nil {
		lease = *given.LeaseMS
	}
	if lease < MinLeaseMS || lease > MaxLeaseMS {
		return Rule{}, ErrInvalidPolicy
	}

	if len(given.Dial) > MaxDialAddresses {
		return Rule{}, ErrInvalidPolicy
	}

	dial := make([]netip.Addr, 0, len(given.Dial))
	for _, text := range given.Dial {
		address, err := readAddress(text)
		if err != nil {
			return Rule{}, err
		}
		dial = append(dial, address)
	}

	var listen *Endpoint
	if given.Listen != nil {
		bind, err := netip.ParseAddr(given.Listen.Bind)
		if err != nil || given.Listen.Port < 0 || given.Listen.Port > 65535 {
			return Rule{}, ErrInvalidPolicy
		}
		if !allowedAddress(bind) && !bind.IsUnspecified() {
			return Rule{}, ErrInvalidPolicy
		}

		address, err := readAddress(given.Listen.Address)
		if err != nil {
			return Rule{}, err
		}
		if address.Is4() != bind.Is4() {
			return Rule{}, ErrInvalidPolicy
		}
		listen = &Endpoint{Bind: bind, Address: address, Port: given.Listen.Port}
	}

	// A rule names somewhere to dial, somewhere to listen, or both. A hole
	// punch needs somewhere to dial.
	if len(dial) == 0 && listen == nil {
		return Rule{}, ErrInvalidPolicy
	}
	if given.HolePunch && len(dial) == 0 {
		return Rule{}, ErrInvalidPolicy
	}

	return Rule{
		Peer: peer, Capability: given.Capability, Scheme: given.Scheme, Path: given.Path,
		LeaseMS: lease, Dial: dial, HolePunch: given.HolePunch, Listen: listen,
	}, nil
}

// FindRule finds the rule that allows one conversation.
func FindRule(rules []Rule, peer []byte, capability, scheme, path string) *Rule {
	for index := range rules {
		rule := &rules[index]
		if string(rule.Peer) == string(peer) && rule.Capability == capability &&
			rule.Scheme == scheme && rule.Path == path {
			return rule
		}
	}
	return nil
}

// Candidate reads the address that a peer offered, and answers where to dial
// it. An address that the owner did not name is refused.
func (r *Rule) Candidate(candidate map[string]any) (string, error) {
	if len(candidate) != 2 {
		return "", ErrAddressDenied
	}

	host, _ := candidate["host"].(string)
	port, ok := wholeNumber(candidate["port"])
	if !ok || port < 1 || port > 65535 {
		return "", ErrAddressDenied
	}

	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", ErrAddressDenied
	}

	for _, allowed := range r.Dial {
		if allowed == address {
			return netip.AddrPortFrom(address, uint16(port)).String(), nil
		}
	}
	return "", ErrAddressDenied
}

// readAddress reads one literal address, and refuses the ranges that no
// citizen dials.
func readAddress(text string) (netip.Addr, error) {
	address, err := netip.ParseAddr(text)
	if err != nil || !allowedAddress(address) {
		return netip.Addr{}, ErrAddressDenied
	}
	return address, nil
}

// allowedAddress refuses an address that names no one machine: the
// unspecified address, a multicast address, and a link local address.
func allowedAddress(address netip.Addr) bool {
	switch {
	case !address.IsValid(), address.IsUnspecified():
		return false
	case address.IsMulticast(), address.IsLinkLocalUnicast(), address.IsLinkLocalMulticast():
		return false
	case address.Is4In6():
		return false
	case address.Is4() && address.As4()[0] == 0:
		return false
	default:
		return true
	}
}

func wholeNumber(value any) (int, bool) {
	switch value := value.(type) {
	case json.Number:
		number, err := value.Int64()
		return int(number), err == nil
	case float64:
		return int(value), value == float64(int(value))
	case int:
		return value, true
	default:
		return 0, false
	}
}
