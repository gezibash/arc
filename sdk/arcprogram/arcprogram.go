// Package arcprogram runs the arc program for a client program. A client
// program holds no key: it asks arc to sign, send and read, so ARC signs
// with the key of the citizen. docs/clients/SPEC.md defines the commands
// and the JSON lines that this package reads.
package arcprogram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/gezibash/arc/core/keys"
)

// MaxLine is the longest JSON line that this package reads from arc. A
// message holds at most 24 KiB, and JSON can write each byte as six.
const MaxLine = 1 << 20

// Arc names the arc program and the arc home of this citizen.
type Arc struct {
	// Program is the arc program. The default is "arc" on PATH.
	Program string
	// Home is the arc home. The default is the home that arc picks.
	Home string
}

// Command makes a command of the arc program, in the arc home.
func (a Arc) Command(ctx context.Context, args ...string) *exec.Cmd {
	program := a.Program
	if program == "" {
		program = "arc"
	}
	if a.Home != "" {
		args = append([]string{"--home", a.Home}, args...)
	}
	return exec.CommandContext(ctx, program, args...)
}

// PublicKey asks arc for the public key of this citizen.
func (a Arc) PublicKey(ctx context.Context) (string, error) {
	out, err := a.output(ctx, nil, "whoami")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if key := strings.TrimSpace(line); IsKey(key) {
			return key, nil
		}
	}
	return "", errors.New("arc whoami printed no public key")
}

// Relays asks arc for the relay URLs of this citizen. It leaves out the
// indexers.
func (a Arc) Relays(ctx context.Context) ([]string, error) {
	out, err := a.output(ctx, nil, "relay", "ls")
	if err != nil {
		return nil, err
	}
	var relays []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 && (strings.HasPrefix(fields[0], "ws://") || strings.HasPrefix(fields[0], "wss://")) {
			relays = append(relays, fields[0])
		}
	}
	return relays, nil
}

// Message is one message that this citizen received.
type Message struct {
	ID string `json:"id"`
	// From is the public key of the sender, and Name is its petname.
	From string    `json:"from"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// Members are the public keys of the sender, then of each recipient.
	// A direct message has two. An arc older than group messages prints
	// none.
	Members []string `json:"members"`
	// ReplyTo is the id of the message that this one replies to, or empty.
	// The sender chose it: it can name any id. Show the text of the parent
	// only from a message of this machine, in the same conversation.
	ReplyTo string `json:"reply_to"`
}

// Sent is one message that this citizen sent. A group message has one Sent
// for each recipient, with the same ID.
type Sent struct {
	ID string `json:"id"`
	// To is the public key of the recipient, and Name is its petname.
	To   string    `json:"to"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// State is pending, delivered or expired.
	State string `json:"state"`
	// Members are the public keys of this citizen, then of each
	// recipient.
	Members []string `json:"members"`
	// ReplyTo is the id of the message that this one replies to, or empty.
	ReplyTo string `json:"reply_to"`
}

// Queued is a message that arc put in the outbox. To is the first
// recipient.
type Queued struct {
	ID      string   `json:"id"`
	To      string   `json:"to"`
	State   string   `json:"state"`
	Members []string `json:"members"`
	ReplyTo string   `json:"reply_to"`
}

// Inbox returns the messages that this citizen received, oldest first. It
// reads the store of this machine; arc sync and a watch fill it.
func (a Arc) Inbox(ctx context.Context) ([]Message, error) {
	return lines[Message](a.output(ctx, nil, "message", "inbox", "--json"))
}

// Outbox returns the messages that this citizen sent from this machine,
// oldest first.
func (a Arc) Outbox(ctx context.Context) ([]Sent, error) {
	return lines[Sent](a.output(ctx, nil, "message", "outbox", "--json"))
}

// Send puts a message to each recipient in the outbox, and sends it to the
// relays at once. With more than one recipient, it is one group message:
// each recipient sees the others.
func (a Arc) Send(ctx context.Context, to []string, text string) (Queued, error) {
	return a.send(ctx, to, text)
}

// SendReply sends a message that replies to the message with the id
// parent, as Send does. The parent must be a message of this machine, and
// to must be its members other than this citizen. An arc older than
// replies refuses the flag --reply.
func (a Arc) SendReply(ctx context.Context, to []string, parent, text string) (Queued, error) {
	if !IsKey(parent) {
		return Queued{}, errors.New("the id of a message is 64 lowercase hex digits")
	}
	return a.send(ctx, to, text, "--reply", parent)
}

func (a Arc) send(ctx context.Context, to []string, text string, flags ...string) (Queued, error) {
	if len(to) == 0 {
		return Queued{}, errors.New("a message needs a recipient")
	}
	for _, key := range to {
		if !IsKey(key) {
			return Queued{}, errors.New("a recipient is a public key of 64 hex digits")
		}
	}
	// The text goes on standard input, so a text that starts with a dash
	// is not a flag.
	args := append([]string{"message", "send", "--json"}, flags...)
	out, err := a.output(ctx, strings.NewReader(text), append(args, strings.Join(to, ","))...)
	if err != nil {
		return Queued{}, err
	}
	var q Queued
	if err := json.Unmarshal(out, &q); err != nil {
		return Queued{}, fmt.Errorf("arc message send printed no message: %w", err)
	}
	return q, nil
}

// WatchOptions are the settings of a watch.
type WatchOptions struct {
	// Since first gives the stored messages at or after an RFC 3339 time,
	// or after a message id. Empty gives only new messages.
	Since string
	// Notes gets what arc writes on standard error, such as each relay
	// that it watches. Nil discards it.
	Notes io.Writer
}

// Watch gives each message that arrives to fn, once for each id, until ctx
// ends or fn returns an error. It holds a watch on each relay of this
// citizen, so the store of this machine also keeps each message.
func (a Arc) Watch(ctx context.Context, opts WatchOptions, fn func(Message) error) error {
	args := []string{"message", "watch", "--json"}
	if opts.Since != "" {
		args = append(args, "--since", opts.Since)
	}
	command := a.Command(ctx, args...)
	inGroup(command)
	command.Cancel = func() error { return interrupt(command) }
	command.WaitDelay = 5 * time.Second
	problems := &tail{}
	command.Stderr = problems
	if opts.Notes != nil {
		command.Stderr = io.MultiWriter(problems, opts.Notes)
	}
	out, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("arc message watch: %w", a.missing(err))
	}

	seen := map[string]bool{}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64<<10), MaxLine)
	var failed error
	for scanner.Scan() {
		var m Message
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			failed = fmt.Errorf("arc message watch printed a line that is not a message: %w", err)
			break
		}
		// arc can give a message twice, for example after a restart.
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if failed = fn(m); failed != nil {
			break
		}
	}
	if failed == nil {
		failed = scanner.Err()
	}
	if failed != nil {
		_ = interrupt(command)
		_, _ = io.Copy(io.Discard, out)
	}
	waited := command.Wait()
	switch {
	case failed != nil:
		return failed
	case ctx.Err() != nil:
		return nil
	case waited != nil:
		return fmt.Errorf("arc message watch: %s", problems.reason(waited))
	default:
		return errors.New("arc message watch stopped")
	}
}

// IsKey says whether text is a public key: 64 lowercase hex digits.
func IsKey(text string) bool {
	if len(text) != 64 {
		return false
	}
	for _, c := range text {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// output runs an arc command and returns its standard output. A failure
// holds what arc wrote on standard error.
func (a Arc) output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	command := a.Command(ctx, args...)
	// At the end of ctx, the whole process group of arc ends: an arc that
	// a wrapper runs, or a child of arc, does not stay behind, hung.
	inGroup(command)
	command.Stdin = stdin
	problems := &tail{}
	command.Stderr = problems
	out, err := command.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("arc %s: %w", strings.Join(args[:min(2, len(args))], " "), a.missing(err))
	}
	if err != nil {
		return nil, fmt.Errorf("arc %s: %s", strings.Join(args[:min(2, len(args))], " "), problems.reason(err))
	}
	return out, nil
}

// ErrNoArc is the error when the arc program cannot be found.
var ErrNoArc = errors.New("the arc program is not installed")

// missing explains an error that says the arc program cannot be found.
func (a Arc) missing(err error) error {
	if !errors.Is(err, exec.ErrNotFound) {
		return err
	}
	program := a.Program
	if program == "" {
		program = "arc"
	}
	return fmt.Errorf("%w: no %q on PATH; install arc with the steps at https://republic.sh/join.md, or give the path of arc", ErrNoArc, program)
}

// Petname is the petname of a public key in hex, as arc prints it, for
// example "bold-einstein-3a7f0bc1", or "" for text that is not a key. The
// suffix comes from a hash of the key, not from the start of the key.
func Petname(key string) string {
	if !IsKey(key) {
		return ""
	}
	b, _ := hex.DecodeString(key)
	return keys.Name(b)
}

// lines reads one JSON object from each line of out.
func lines[T any](out []byte, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	var records []T
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64<<10), MaxLine)
	for scanner.Scan() {
		var record T
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("arc printed a line that is not JSON: %w", err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

// tail keeps the last 4 KiB that arc writes on standard error.
type tail struct{ buf []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - 4<<10; extra > 0 {
		t.buf = t.buf[extra:]
	}
	return len(p), nil
}

// reason is what arc wrote, or the error of the process if arc wrote
// nothing.
func (t *tail) reason(err error) string {
	if text := strings.TrimSpace(string(t.buf)); text != "" {
		return text
	}
	return err.Error()
}
