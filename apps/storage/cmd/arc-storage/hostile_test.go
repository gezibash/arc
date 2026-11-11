package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/pion/webrtc/v4"
)

// hostileEnv makes the test program the hostile storage citizen.
const hostileEnv = "ARC_STORAGE_TEST_HOSTILE"

// hostile is the text that the hostile storage citizen puts in each field
// that it chooses: an OSC that changes the title of the terminal, an ESC
// sequence that clears the screen, U+202E that reverses the line, the tag
// character U+E0068 that a person cannot see, and newlines that forge a
// line of arc-storage and a line of arc.
const hostile = "P\x1b]0;pwned\x07\x1b[2J\u202egnp.exe\U000e0068\narc-storage: forged\narc: forged"

// escaped is hostile as a line shows it. It is worked out by hand from the
// table of docs/clients/SPEC.md, section 3.9: ESC is \x1b, BEL is \x07,
// U+202E is \u202e, U+E0068 is \U000e0068, and on one line a newline is \n.
const escaped = `P\x1b]0;pwned\x07\x1b[2J\u202egnp.exe\U000e0068\narc-storage: forged\narc: forged`

// The notes of a join tell the hostile storage citizen how to answer.
const (
	pendingNote  = "pending"
	admittedNote = "admitted"
	refusedNote  = "refused"
)

// hostileApp is a storage citizen that answers each request with hostile
// text in each field that a storage citizen chooses. The request tells it
// which answer to give:
//   - a join: by its note, pending with a hostile message, admitted with a
//     hostile plan and state, or refused with a hostile message;
//   - a put: a blob of wire.MinBlob bytes gets a hostile status, a larger
//     blob a refusal with a hostile code and message;
//   - a delete: an ID that starts with "a" gets a hostile status, each
//     other ID a hostile error code;
//   - an audit: a hostile error code for each ID;
//   - a usage or a drain: a hostile plan and state.
type hostileApp struct{}

func runHostile(ctx context.Context, opts provider.Options) error {
	return provider.Run(ctx, hostileApp{}, opts)
}

func (hostileApp) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(request.Message), &fields) != nil {
		return "", provider.Error(wire.ErrInvalidRequest)
	}
	var reply any
	switch {
	case fields["grants"] != nil:
		var req wire.Grants
		if json.Unmarshal([]byte(request.Message), &req) != nil {
			return "", provider.Error(wire.ErrInvalidRequest)
		}
		reply = wire.Granted{Version: 1, Devices: len(req.Devices)}
	case fields["join"] != nil:
		var req wire.JoinCall
		if json.Unmarshal([]byte(request.Message), &req) != nil {
			return "", provider.Error(wire.ErrInvalidRequest)
		}
		reply = joinReply(req.Join.Note)
	default:
		var req wire.Open
		if json.Unmarshal([]byte(request.Message), &req) != nil {
			return "", provider.Error(wire.ErrInvalidRequest)
		}
		answer, err := hostileSession(req.Session)
		if err != nil {
			return "", err
		}
		reply = answer
	}
	raw, err := json.Marshal(reply)
	return string(raw), err
}

func joinReply(note string) wire.JoinReply {
	switch note {
	case pendingNote:
		return wire.JoinReply{Version: 1, Account: &wire.Account{Status: wire.StatusPending, Message: hostile}}
	case admittedNote:
		return wire.JoinReply{Version: 1, Account: &wire.Account{Status: wire.StatusAdmitted, Quota: 1 << 30, Plan: hostile, State: hostile}}
	}
	return wire.JoinReply{Version: 1, Error: wire.ErrRefused, Message: hostile}
}

// hostileSession opens the end of the hostile storage citizen for the
// offer of a session, and answers each request of the pipe.
func hostileSession(req wire.Session) (wire.Answer, error) {
	pc, err := direct.NewConnection(direct.Options{Loopback: true, PeerSTUN: req.STUN})
	if err != nil {
		return wire.Answer{}, err
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnClose(func() { go pc.Close() })
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			var req wire.Request
			if !m.IsString || json.Unmarshal(m.Data, &req) != nil {
				return
			}
			raw, _ := json.Marshal(hostileReply(req))
			_ = dc.SendText(string(raw))
		})
	})
	local := webrtc.SessionDescription{}
	err = pc.SetRemoteDescription(req.SDP)
	if err == nil {
		local, err = pc.CreateAnswer(nil)
	}
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		_ = pc.Close()
		return wire.Answer{}, err
	}
	return wire.Answer{Version: 1, SDP: local}, nil
}

func hostileReply(req wire.Request) wire.Reply {
	r := wire.Reply{N: req.N}
	switch req.Op {
	case wire.OpHello:
		r.Version, r.Features, r.Window = 2, []string{wire.FeatureBatch, wire.FeatureAudit, wire.FeatureDrain}, wire.Window
	case wire.OpPut:
		if req.Size == wire.MinBlob {
			r.Status = hostile
		} else {
			r.Error, r.Message = hostile, hostile
		}
	case wire.OpUsage, wire.OpDrain:
		r.Used, r.Quota, r.Blobs, r.Plan, r.State = 1, 2, 3, hostile, hostile
	case wire.OpAudit:
		for _, id := range req.IDs {
			r.Items = append(r.Items, wire.Item{ID: id, Error: hostile})
		}
	case wire.OpDelete:
		for _, id := range req.IDs {
			item := wire.Item{ID: id, Error: hostile}
			if strings.HasPrefix(id, "a") {
				item = wire.Item{ID: id, Status: hostile}
			}
			r.Items = append(r.Items, item)
		}
	default:
		r.Error, r.Message = wire.ErrInvalidRequest, "the hostile storage citizen answers no "+req.Op
	}
	return r
}

// startHostile serves the storage app with the test program as the
// hostile storage citizen, and returns its key.
func startHostile(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := filepath.Abs(filepath.Join("..", "..", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := t.TempDir()
	arcfile := fmt.Sprintf("version = 2\n\n[serve]\ncommand = %q\nmanifest = %q\n", self, manifest)
	if err := os.WriteFile(filepath.Join(app, "Arcfile"), []byte(arcfile), 0o600); err != nil {
		t.Fatal(err)
	}
	home := newCitizen(t)
	serveApp(t, home, app, hostileEnv+"=1")
	return whoami(t, home)
}

// lines splits the output of a program into its lines.
func lines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// blobID is the ID of the blob of a file of data: the SHA-256 of the
// padded bytes.
func blobID(t *testing.T, data []byte) string {
	t.Helper()
	blob, err := wire.Pad(data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// mustBeClean fails the test if an output holds a character that the
// hostile text hides from the terminal, or a line that it forged.
func mustBeClean(t *testing.T, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if strings.ContainsAny(out, "\x1b\x07\u202e\U000e0068") {
			t.Errorf("the output holds a raw character of the hostile text:\n%q", out)
		}
		for _, line := range lines(out) {
			if strings.HasPrefix(line, "arc-storage: forged") || strings.HasPrefix(line, "arc: forged") {
				t.Errorf("the hostile text forged the line %q in:\n%q", line, out)
			}
		}
	}
}

// mustHaveLine fails the test if no line of out is want.
func mustHaveLine(t *testing.T, out, want string) {
	t.Helper()
	if !slices.Contains(lines(out), want) {
		t.Errorf("no line is %q in:\n%q", want, out)
	}
}

// TestAHostileStorageCitizenIsEscaped runs arc-storage and arc keys storage
// add against a storage citizen that answers with hostile text, through
// the relay, arc serve and the pipe of sdk/direct. Each line that shows
// the text keeps it on one line, escaped (docs/clients/SPEC.md, section
// 3.9).
func TestAHostileStorageCitizenIsEscaped(t *testing.T) {
	key := startHostile(t)
	owner := newCitizen(t)
	arc(t, owner, "install", key, "storage", "--yes")
	small, smallData := file(t, 100) // a blob of wire.MinBlob bytes
	large, _ := file(t, 5000)        // a larger blob

	t.Run("put", func(t *testing.T) {
		stdout, stderr, err := run(owner, "put", small, large)
		if err == nil {
			t.Fatal("a put that the storage citizen refused worked")
		}
		mustBeClean(t, stdout, stderr)
		if got, want := lines(stdout), []string{blobID(t, smallData) + "\t" + escaped + "\t" + small}; !slices.Equal(got, want) {
			t.Errorf("put printed %q, want %q", got, want)
		}
		mustHaveLine(t, stderr, "arc-storage: "+large+": the storage citizen refused the put: "+escaped)
	})
	t.Run("delete", func(t *testing.T) {
		deleted, refused := strings.Repeat("a", 64), strings.Repeat("b", 64)
		stdout, stderr, err := run(owner, "delete", deleted, refused)
		if err == nil {
			t.Fatal("a delete that the storage citizen refused worked")
		}
		mustBeClean(t, stdout, stderr)
		if got, want := lines(stdout), []string{deleted + "\t" + escaped}; !slices.Equal(got, want) {
			t.Errorf("delete printed %q, want %q", got, want)
		}
		mustHaveLine(t, stderr, "arc-storage: "+refused+": the storage citizen refused the delete: "+escaped)
	})
	t.Run("usage", func(t *testing.T) {
		stdout, stderr, err := run(owner, "usage")
		if err != nil {
			t.Fatalf("usage: %v\n%s", err, stderr)
		}
		mustBeClean(t, stdout, stderr)
		if got, want := lines(stdout), []string{"1 bytes of 2 in use, 3 blobs, plan " + escaped + ", " + escaped}; !slices.Equal(got, want) {
			t.Errorf("usage printed %q, want %q", got, want)
		}
	})
	t.Run("audit", func(t *testing.T) {
		stdout, stderr, err := run(owner, "audit", small)
		if err == nil {
			t.Fatal("an audit that failed worked")
		}
		mustBeClean(t, stdout, stderr)
		if got, want := lines(stdout), []string{blobID(t, smallData) + "\t" + escaped + "\t" + small}; !slices.Equal(got, want) {
			t.Errorf("audit printed %q, want %q", got, want)
		}
		mustHaveLine(t, stderr, "arc-storage: 1 of 1 blobs failed the audit")
	})
	t.Run("drain", func(t *testing.T) {
		stdout, stderr, err := run(owner, "drain")
		if err != nil {
			t.Fatalf("drain: %v\n%s", err, stderr)
		}
		mustBeClean(t, stdout, stderr)
		if got, want := lines(stdout), []string{escaped + ": 3 blobs of 1 bytes remain"}; !slices.Equal(got, want) {
			t.Errorf("drain printed %q, want %q", got, want)
		}
	})

	// arc keys storage add prints the message, the plan and the state of
	// the answer, and arc prints the error line of a refusal.
	t.Run("join pending", func(t *testing.T) {
		out, err := join(owner, key, "--note", pendingNote)
		if err != nil {
			t.Fatalf("join: %v\n%s", err, out)
		}
		mustBeClean(t, out)
		mustHaveLine(t, out, "storage: pending: "+escaped)
	})
	t.Run("join refused", func(t *testing.T) {
		out, err := join(owner, key, "--note", refusedNote)
		if err == nil {
			t.Fatalf("a refused join worked:\n%s", out)
		}
		mustBeClean(t, out)
		mustHaveLine(t, out, "arc: storage refused the account: "+escaped)
	})
	t.Run("join admitted", func(t *testing.T) {
		out, err := join(owner, key, "--note", admittedNote)
		if err != nil {
			t.Fatalf("join: %v\n%s", err, out)
		}
		mustBeClean(t, out)
		admitted := slices.ContainsFunc(lines(out), func(line string) bool {
			return strings.HasPrefix(line, "storage: admitted: ") && strings.HasSuffix(line, ", plan "+escaped+", "+escaped)
		})
		if !admitted {
			t.Errorf("no line is storage: admitted: <size> of <quota> in use, plan %s, %s in:\n%q", escaped, escaped, out)
		}
	})
}
