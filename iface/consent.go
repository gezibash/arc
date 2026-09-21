package iface

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// reserved are the kinds that no manifest can name. They speak for the
// citizen's identity, or core makes them itself. See section 12 of the spec.
var reserved = map[int]string{
	0: "profile", 3: "follows", 5: "deletion",
	13: "seal", 1059: "gift wrap", 21059: "gift wrap",
	62:   "request to vanish",
	1234: "draft checkpoint", 31234: "draft", 3275: "part",
	3272: "call request", 3273: "call reply", 3274: "acknowledgement",
	9734: "zap request", 9735: "zap",
	10002: "relay list", 10013: "private relay list", 10050: "direct message relays",
	10272: "migration record", 30272: "capability announcement",
	13194: "wallet connect", 23194: "wallet connect", 23195: "wallet connect",
	22242: "authentication", 24133: "remote signing", 27235: "HTTP authentication",
}

// checkReserved refuses a kind that is reserved, a moderation kind of NIP-29
// outside a group, and the group state that a relay signs.
func checkReserved(name string, k Kind) error {
	if why, ok := reserved[k.Kind]; ok {
		return fmt.Errorf("kind %s: %d is reserved (%s); no manifest can name it", name, k.Kind, why)
	}
	if k.Kind >= 39000 && k.Kind <= 39009 {
		return fmt.Errorf("kind %s: %d is group state, which only a relay signs", name, k.Kind)
	}
	if k.Kind >= 9000 && k.Kind <= 9020 && k.Visibility != "group" {
		return fmt.Errorf("kind %s: %d moderates a NIP-29 group, so its visibility must be group", name, k.Kind)
	}
	return nil
}

// visibleness orders the visibilities, from the least visible.
var visibleness = []string{"sealed", "private", "group", "public"}

// Consent is what a citizen agreed to when they installed a capability: the
// kinds it uses, each with its visibility, and its group relay.
type Consent struct {
	// Kinds maps each event kind, as a number, to its visibility.
	Kinds map[string]string `json:"kinds"`
	Group string            `json:"group,omitempty"`
}

// ConsentOf is what installing a manifest agrees to.
func ConsentOf(m *Manifest) Consent {
	c := Consent{Kinds: map[string]string{}}
	for _, k := range m.Kinds {
		c.Kinds[strconv.Itoa(k.Kind)] = k.Visibility
	}
	if m.Group != nil {
		c.Group = m.Group.Relay
	}
	return c
}

// Changes lists what a manifest does beyond a consent: a kind that is new, a
// kind that is more visible, or another group relay. Each needs the citizen
// to agree again.
func (c Consent) Changes(m *Manifest) []string {
	var out []string
	for name, k := range m.Kinds {
		was, ok := c.Kinds[strconv.Itoa(k.Kind)]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("it uses kind %d (%s) now, %s", k.Kind, name, k.Visibility))
		case slices.Index(visibleness, k.Visibility) > slices.Index(visibleness, was):
			out = append(out, fmt.Sprintf("kind %d (%s) is %s now, not %s", k.Kind, name, k.Visibility, was))
		}
	}
	if m.Group != nil && m.Group.Relay != c.Group {
		out = append(out, "it uses the group relay "+m.Group.Relay+" now")
	}
	sort.Strings(out)
	return out
}

// Describe says what installing a capability lets it do, as install shows it.
func Describe(m *Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  a %s with %d commands, interface version %d\n", m.Shape, len(m.Commands), m.Interface)
	names := make([]string, 0, len(m.Kinds))
	for name := range m.Kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	speaks := false
	for _, name := range names {
		k := m.Kinds[name]
		fmt.Fprintf(&b, "  kind %d (%s): %s\n", k.Kind, name, visibilityText[k.Visibility])
		speaks = speaks || k.Visibility == "public" || k.Visibility == "group"
	}
	if m.Group != nil {
		fmt.Fprintf(&b, "  group %s on %s\n", m.Group.ID, m.Group.Relay)
	}
	if speaks {
		b.WriteString("  it can post under your name, where others read it\n")
	}
	return b.String()
}

var visibilityText = map[string]string{
	"sealed":  "sealed to your own key",
	"private": "sealed to the citizens you name",
	"group":   "posted to the group, signed by you",
	"public":  "posted in public, signed by you",
}
