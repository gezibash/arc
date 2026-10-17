package rings

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// logLine waits for the line of the log that holds each of parts, and
// returns it.
func (h *harness) logLine(parts ...string) string {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for line := range strings.Lines(h.logs.String()) {
			found := true
			for _, part := range parts {
				found = found && strings.Contains(line, part)
			}
			if found {
				return line
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no line of the log holds %q; the log is:\n%s", parts, h.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheLogSaysWhyARelayDoesNotRing makes a wrap that rings no one for
// each condition of docs/rings/SPEC.md, section 3.5, and a retry that
// waits (section 3.7). The log gives the reason, and names the citizen by
// the first 8 hex digits of its key only.
func TestTheLogSaysWhyARelayDoesNotRing(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		reason string
		// before makes the state in which the next wrap for bob does not
		// ring.
		before func(h *harness, bob keys.Key)
	}{
		{
			name:   "expired",
			cfg:    Config{TTL: time.Hour},
			reason: `reason=expired`,
			before: func(h *harness, bob keys.Key) {
				h.mustRegister(bob)
				h.advance(2 * time.Hour)
			},
		},
		{
			name:   "watching",
			reason: `reason="a read subscription is open"`,
			before: func(h *harness, bob keys.Key) {
				h.mustRegister(bob)
				h.subscribe(bob.Public, &bob)
			},
		},
		{
			name:   "rang",
			reason: `reason="already rang in this unread period"`,
			before: func(h *harness, bob keys.Key) {
				h.mustRegister(bob)
				h.mail(bob.Public, private.RelayForm, private.WrapKind)
				h.advance(2 * time.Second)
				h.tick()
			},
		},
		{
			name:   "citizen bucket",
			reason: `reason="citizen bucket empty, waits" wait=5m0s`,
			before: func(h *harness, bob keys.Key) {
				if code, body := h.register(bob, Request{URL: "https://example.com/hook", Burst: 1, Key: ringKey}); code != http.StatusOK {
					h.t.Fatalf("register: %d %s", code, body)
				}
				h.mail(bob.Public, private.RelayForm, private.WrapKind)
				h.tick()
				h.read(bob)
			},
		},
		{
			name:   "daily cap",
			reason: `reason="daily cap reached, waits"`,
			before: func(h *harness, bob keys.Key) {
				if code, body := h.register(bob, Request{URL: "https://example.com/hook", DailyCap: 1, Key: ringKey}); code != http.StatusOK {
					h.t.Fatalf("register: %d %s", code, body)
				}
				h.mail(bob.Public, private.RelayForm, private.WrapKind)
				h.tick()
				h.read(bob)
			},
		},
		{
			name:   "relay bucket",
			cfg:    Config{PerMinute: 1},
			reason: `reason="relay bucket empty, waits"`,
			before: func(h *harness, bob keys.Key) {
				carol := keys.Generate()
				h.mustRegister(bob)
				h.mustRegister(carol)
				h.mail(carol.Public, private.RelayForm, private.WrapKind)
				h.tick()
			},
		},
		{
			name:   "retry",
			reason: `reason="a retry waits"`,
			before: func(h *harness, bob keys.Key) {
				h.mustRegister(bob)
				h.status.Store(http.StatusInternalServerError)
				h.mail(bob.Public, private.RelayForm, private.WrapKind)
				h.tick()
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, c.cfg)
			bob := keys.Generate()
			c.before(h, bob)
			h.calls()
			h.mail(bob.Public, private.RelayForm, private.WrapKind)
			line := h.logLine(`msg="no ring"`, "citizen="+bob.Public.Hex()[:8]+" ", c.reason)
			if !strings.Contains(line, "level=INFO") {
				t.Errorf("the line is not at Info: %s", line)
			}
			h.tick()
			if n := len(h.calls()); n != 0 {
				t.Fatalf("the wrap made %d rings, want none", n)
			}
			if strings.Contains(h.logs.String(), bob.Public.Hex()) {
				t.Fatalf("the log holds the whole key of the citizen:\n%s", h.logs.String())
			}
		})
	}
}

// TestTheLogNamesTheCitizenOfARing checks the line of each try: the first
// 8 hex digits of the key of the citizen, the status and the attempt.
func TestTheLogNamesTheCitizenOfARing(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("the wrap made %d rings, want 1", n)
	}
	h.logLine("msg=ring citizen=" + bob.Public.Hex()[:8] + " status=200 failed=false attempt=1")
	if strings.Contains(h.logs.String(), `msg="no ring"`) {
		t.Fatalf("a wrap that rang logged a skip:\n%s", h.logs.String())
	}
}

// TestAWrapForNoRegistrationLogsOnlyForDebugging: most wraps of a relay are
// for citizens with no registration, so their line is at Debug, which the
// relay does not print by default.
func TestAWrapForNoRegistrationLogsOnlyForDebugging(t *testing.T) {
	h := start(t, Config{})
	h.rings.mu.Lock()
	h.rings.cfg.Logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.rings.mu.Unlock()
	bob := keys.Generate().Public
	h.mail(bob, private.RelayForm, private.WrapKind)
	line := h.logLine(`msg="no ring"`, "citizen="+bob.Hex()[:8]+" ", `reason="no registration"`)
	if !strings.Contains(line, "level=DEBUG") {
		t.Fatalf("the line is not at Debug: %s", line)
	}
}
