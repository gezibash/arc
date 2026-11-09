package citizen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

// opening is the pipe to one storage citizen. notes holds what the pipe
// printed, so that the notes of two storage citizens do not mix.
type opening struct {
	conn   *storage.Conn
	opened time.Time
	err    error
	silent time.Duration
	// locked says that the account of a draining storage citizen takes no
	// put now, from any device.
	locked bool
	said   bytes.Buffer
	notes  bytes.Buffer
}

// openOne opens the pipe to one storage citizen, and keeps the record of
// its silence.
func (sess *Session) openOne(ctx context.Context, cfg SelfSync, syncer *selfsync.Syncer, t target) (o opening) {
	install := t.install
	// A repair opens its pipes as a sync does: it deletes over TURN, and
	// waits for a direct pipe only to rebuild or audit (docs/storage/SPEC.md,
	// section 4.6). So it never adds a direct failure inside the window.
	conn, err := openPipe(syncer, install.Provider, install.As, cfg.Path, time.Now, &o.notes,
		func(noRelay, onlyRelay bool) (*storage.Conn, error) {
			client := storage.Client{
				Call: func(ctx context.Context, session string) ([]byte, error) {
					reply, err := sess.openStorage(ctx, cfg.Installs, install.As, session)
					if err != nil {
						return nil, callError{err}
					}
					return reply, nil
				},
				TURN: cfg.TURN, Options: cfg.Options, Notes: &o.notes, NoRelay: noRelay, OnlyRelay: onlyRelay,
			}
			return client.Open(ctx, 0)
		})
	o.opened = time.Now()
	if err != nil {
		o.err = err
		if silent, serr := syncer.Unanswered(install.Provider, time.Now()); serr != nil {
			o.err = errors.Join(err, serr)
		} else {
			o.silent = silent
		}
		return o
	}
	if err := syncer.Answered(install.Provider, time.Now()); err != nil {
		conn.Close()
		o.err = err
		return o
	}
	o.conn = conn
	o.locked, o.err = sess.setDrain(ctx, conn, t, &o.said, &o.notes)
	return o
}

// callError is an error of the ARC call that carries the offer of a pipe:
// the storage citizen did not answer, so another attempt does not help.
type callError struct{ error }

func (e callError) Unwrap() error { return e.error }

// openPipe opens the pipe to one storage citizen with the path of the
// citizen. With the path auto or session, it records whether the direct
// attempts found a path. For selfsync.TURNFirstFor after they found none,
// a sync tries the TURN server of the relay first, and the direct attempts
// only if that fails (docs/storage/SPEC.md, section 4.6). open runs the
// attempts.
func openPipe(syncer *selfsync.Syncer, name, as string, policy direct.Policy, now func() time.Time, notes io.Writer,
	open func(noRelay, onlyRelay bool) (*storage.Conn, error)) (*storage.Conn, error) {
	if policy == direct.DirectOnly || policy == direct.RelayOnly {
		return open(policy == direct.DirectOnly, policy == direct.RelayOnly)
	}
	record := func(conn *storage.Conn, found bool) (*storage.Conn, error) {
		if err := syncer.DirectPath(name, now(), found); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	until, err := syncer.TURNFirst(name, now())
	if err != nil {
		return nil, err
	}
	if !until.IsZero() {
		fmt.Fprintln(notes, terminal.EscapeLine(fmt.Sprintf("%s: the direct attempts found no path; until %s each sync tries the TURN server of the relay first, so the bytes go through the relay",
			as, until.Local().Format("15:04"))))
		conn, err := open(false, true)
		if err == nil || errors.As(err, new(callError)) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return conn, err
		}
		fmt.Fprintln(notes, terminal.EscapeLine(fmt.Sprintf("%s: no path through the relay: %v; trying the direct attempts", as, err)))
		if conn, err = open(true, false); err != nil {
			return nil, err
		}
		return record(conn, true)
	}

	conn, err := open(false, false)
	if err != nil {
		return nil, err
	}
	return record(conn, conn.Path != "relay")
}

// setDrain makes the account follow the storage list before the sync
// lists it. Each device sends a drain request to a draining storage
// citizen, so that it refuses each put, also from an older device. Only
// the signer ends a drain, for an entry that the list names active: a
// device can hold an older list. It reports if the account takes no put.
func (sess *Session) setDrain(ctx context.Context, conn *storage.Conn, t target, out, notes io.Writer) (bool, error) {
	as := terminal.EscapeLine(t.install.As)
	switch {
	case t.state == storage.StateDraining:
		usage, err := conn.Drain(ctx, false)
		if errors.Is(err, storage.ErrOlderApp) {
			fmt.Fprintf(notes, "%s runs a storage app from before drain requests: it still takes a put from an older device; update it\n", as)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("the drain request failed: %w", err)
		}
		return usage.State == storage.StateDraining || usage.State == storage.StateClosing || usage.State == storage.StateClosed, nil
	case t.state == storage.StateActive && !sess.Remote:
		usage, err := conn.Usage(ctx)
		if err != nil || usage.State != storage.StateDraining {
			return false, err
		}
		if _, err := conn.Drain(ctx, true); err != nil {
			fmt.Fprintf(notes, "%s: the storage list names it active, but the end of its drain failed, so it takes no put: %s\n", as, terminal.EscapeLine(err.Error()))
			return false, nil
		}
		fmt.Fprintf(out, "%s: the storage list names it active, so its drain ended\n", as)
	}
	return false, nil
}

// usageLine says how much of its quota an account uses: the numbers of
// arc-storage usage, in units for a person.
func usageLine(u storage.Usage) string {
	line := fmt.Sprintf("%s of %s in use, %d blobs", storage.HumanSize(u.Used), storage.HumanSize(u.Quota), u.Blobs)
	if u.Plan != "" {
		line += fmt.Sprintf(", plan %s, %s", u.Plan, u.State)
	}
	if u.Expires > 0 {
		line += ", ends " + time.Unix(u.Expires, 0).UTC().Format(time.DateOnly)
	}
	return line
}

// openStorage runs the manifest command "open" of an installed storage
// app, as "arc <name> open <session>" does. The command adds the pseudonym
// of the account, and makes the ARC call.
func (sess *Session) openStorage(ctx context.Context, installs catalog.Installs, name, session string) ([]byte, error) {
	in, err := sess.Installed(ctx, installs, name)
	if err != nil {
		return nil, err
	}
	env := &Environment{Session: sess, Installs: installs, Tool: name}
	var out, errs bytes.Buffer
	err = iface.Run(ctx, env, in, []string{"open", session}, iface.Stdio{In: strings.NewReader(""), Out: &out, Err: &errs})
	if err != nil {
		text := strings.TrimSpace(errs.String())
		refusal := text + " " + err.Error()
		switch {
		case strings.Contains(refusal, "not_owner"):
			return nil, errors.New("the storage citizen refused this machine: its key is neither an account key in STORAGE_OWNERS nor a device key with a grant; the signer sends grants with arc keys devices add and arc sync")
		case strings.Contains(refusal, "busy"):
			return nil, errors.New("the storage citizen runs too many sessions now; try again later")
		case text != "":
			return nil, fmt.Errorf("the ARC call failed: %s", text)
		}
		return nil, fmt.Errorf("the ARC call failed: %w", err)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}
