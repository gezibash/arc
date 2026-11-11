package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/terminal"
)

// arc reads its own releases from a citizen that serves them, and replaces
// its program with one that the publisher signed. See docs/updates/SPEC.md.

// Update is one run of arc update: the channel to read, from whom, and what
// to replace.
type Update struct {
	Session   *citizen.Session
	Installs  catalog.Installs
	Provider  nostr.PubKey
	Publisher nostr.PubKey
	Channel   string
	// Dir holds the checkpoint of the channel.
	Dir      string
	Pipes    direct.DialConfig
	Platform Platform
	// Version is the release that runs now.
	Version string
	// Apply replaces the program. Without it, Run says what it would do.
	Apply bool
	// Program is the program to replace; "" is the one that runs.
	Program   string
	Out, Errs io.Writer
}

// caller asks one provider for releases with live calls.
type caller struct {
	sess     *citizen.Session
	provider nostr.PubKey
}

func (r caller) Request(ctx context.Context, body []byte) ([]byte, error) {
	request := call.Request{Capability: "releases", Method: "RAW", Path: "/releases", Body: string(body)}
	reply, _, _, err := r.sess.LiveCall(ctx, r.provider, request, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if reply.Err != "" {
		return nil, fmt.Errorf("the releases provider refused: %s", reply.Err)
	}
	return []byte(reply.Body), nil
}

// Run reads the channel, says which release it names, and with Apply
// replaces the program with it.
func (u Update) Run(ctx context.Context) error {
	source, piped, streams, err := u.source(ctx)
	if err != nil {
		return err
	}
	checkpoint := &Checkpoint{Dir: u.Dir}
	newest, err := Newest(ctx, source, checkpoint, u.Publisher[:], u.Channel, u.Platform, u.Version)
	if errors.Is(err, ErrNoRelease) {
		fmt.Fprintf(u.Out, "arc %s is the newest release of %s for %s/%s\n", u.Version, u.Channel, u.Platform.OS, u.Platform.Arch)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(u.Out, "%s names arc %s for %s/%s\n", u.Channel, newest.Version, u.Platform.OS, u.Platform.Arch)
	fmt.Fprintf(u.Out, "  build   %s\n", newest.Build)
	fmt.Fprintf(u.Out, "  hash    %s\n", newest.Archive().SHA256)
	fmt.Fprintf(u.Out, "  size    %d bytes\n", newest.Archive().Size)
	if !u.Apply {
		fmt.Fprintln(u.Out, "\nrun arc update apply to install it")
		return nil
	}
	return u.install(ctx, source, piped, streams, newest)
}

// source chooses how the archive comes. Keep the existing updater usable
// without installation. An installed provider that advertises streaming
// selects the session path before any archive request.
func (u Update) source(ctx context.Context) (Requester, *pipeCaller, bool, error) {
	c := caller{sess: u.Session, provider: u.Provider}
	trusted, err := u.Installs.Trusted(u.Provider, "releases")
	if err != nil {
		return nil, nil, false, err
	}
	streams := false
	if trusted {
		_, offer, _, err := u.Session.Target(ctx, u.Installs, "releases+arc://"+u.Provider.Hex()+"/releases", "")
		if err != nil {
			return nil, nil, false, err
		}
		streams = offer.SupportsInteraction(session.ServerStream)
	}
	// With no pipe, the archive comes in chunks: on a local relay they were
	// faster than session frames, and they need fewer events.
	if streams && u.Pipes.Policy != direct.Off {
		address := "releases+arc://" + u.Provider.Hex() + "/releases"
		open := func(ctx context.Context, body string) (*session.Stream, error) {
			return u.Session.OpenSessionAddress(ctx, u.Installs, address, body, session.ServerStream)
		}
		piped := &pipeCaller{Requester: c, open: open, dial: u.Pipes}
		return piped, piped, streams, nil
	}
	return c, nil, streams, nil
}

// install replaces the program with the newest release, and says which
// path the archive took.
func (u Update) install(ctx context.Context, source Requester, piped *pipeCaller, streams bool, newest *Release) error {
	program, err := programPath(u.Program)
	if err != nil {
		return err
	}
	if !streams && u.Pipes.Policy != direct.Off {
		// A pipe needs a session, and a session needs the releases app
		// installed with server_stream.
		reason := fmt.Sprintf("the releases app of %s is not installed with server_stream, so arc opens no pipe", u.Provider.Hex())
		if u.Pipes.Policy != direct.Auto {
			return fmt.Errorf("%s; run arc install %s releases, or run again with --path session", reason, u.Provider.Hex())
		}
		fmt.Fprintln(u.Errs, terminal.EscapeLine(fmt.Sprintf("%s; using chunks over the relay", reason)))
	}
	if piped != nil {
		piped.dial.Need = newest.Archive().Size
	}
	started, finished := time.Now(), time.Time{}
	progress := func(read, total int64) {
		fmt.Fprintf(u.Out, "\r  read    %d of %d bytes", read, total)
		if read == total {
			finished = time.Now()
		}
	}
	if err := Apply(ctx, source, newest, program, progress); err != nil {
		return err
	}
	fmt.Fprintln(u.Out)
	path := chunkPath
	if piped != nil {
		path = piped.path
	}
	fmt.Fprintln(u.Errs, direct.Report(path, newest.Archive().Size, finished.Sub(started)))
	fmt.Fprintf(u.Out, "arc %s is installed at %s\n", newest.Version, program)
	fmt.Fprintf(u.Out, "the release it replaced stands at %s.previous\n", program)
	return nil
}

// programPath is the program to replace: the one given, or the one that
// runs, with its links resolved.
func programPath(program string) (string, error) {
	if program != "" {
		return filepath.Abs(program)
	}
	running, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(running)
}
