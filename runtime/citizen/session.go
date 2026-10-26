package citizen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/adapters/mailbox"
	blevesearch "github.com/gezibash/arc/adapters/search/bleve"
	"github.com/gezibash/arc/adapters/store/bodies"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/wake"
	"github.com/gezibash/arc/sdk/terminal"
)

// Session is a citizen's application state. Cobra supplies configuration;
// operations here can also serve another UI or an embedded caller.
type Session struct {
	Key    keys.Key
	Signer keys.Signer
	Remote bool
	// Device is the device key of a machine that signs through a remote
	// signer. It opens the data roots wrapped to this device.
	Device   keys.Key
	Node     *node.Node
	Mail     *mail.Mail
	Relays   []transport.Transport
	Indexers []transport.Transport
	URLs     []string
	Waker    *wake.Waker
	NewRelay func(string) transport.Transport
	Errors   io.Writer
	store    *store.Store
	// storeDir is the folder of the store, which holds the lock of writes.
	storeDir string
	receipts kv.Store
	roots    kv.Store
	signers  *signers
	// delegation gives the relays of a device its token.
	delegation *delegation
	search     *blevesearch.Index
	// bodies holds the encrypted bodies of drafts, such as large files.
	bodies bodies.Dir
	// kept holds the relay of each URL that NewRelay gave.
	keptMu sync.Mutex
	kept   map[string]relay.Relay
	// offline warns once that no relay answered.
	offline sync.Once
}

type Config struct {
	Home, Root        string
	Key               keys.Key
	Signer            keys.Signer
	Remote            bool
	Device            keys.Key
	URLs, IndexerURLs []string
	Errors            io.Writer
}

// Open composes filesystem, relay, mail and wake adapters for a local citizen.
func Open(cfg Config) (*Session, error) {
	s, err := boltstore.Open(filepath.Join(cfg.Home, "store"))
	if err != nil {
		return nil, err
	}
	sess := &Session{Key: cfg.Key, Signer: cfg.Signer, Remote: cfg.Remote, Device: cfg.Device, store: s, storeDir: filepath.Join(cfg.Home, "store"), URLs: cfg.URLs, Errors: cfg.Errors, search: blevesearch.New(filepath.Join(cfg.Home, "store", "search", "journal-v1.bleve")), kept: map[string]relay.Relay{}}
	sess.bodies = bodies.Dir{Path: filepath.Join(cfg.Home, "store", "bodies")}
	sess.receipts = boltkv.Open(filepath.Join(cfg.Home, "store", "receipts.db"))
	sess.roots = boltkv.Open(filepath.Join(cfg.Home, "store", "data-roots.db"))
	sess.Node = &node.Node{Store: receiptStore{EventStore: s, db: sess.receipts, me: cfg.Signer.PublicKey()}}
	sess.loadSigners(context.Background())
	if cfg.Remote {
		sess.delegation = &delegation{sess: sess}
	}
	// Each relay keeps one connection for its sends. One URL gives one relay,
	// so a long run does not open a connection for each call.
	sess.NewRelay = func(url string) transport.Transport {
		sess.keptMu.Lock()
		defer sess.keptMu.Unlock()
		r, ok := sess.kept[url]
		if !ok {
			r = relay.New(url, sess.Signer)
			if sess.delegation != nil {
				r.Delegation = sess.delegation
			}
			if sess.Errors != nil {
				r.OnNotice(func(text string) { fmt.Fprintf(sess.Errors, "%s: %s\n", url, notice(text)) })
				r.OnWatchEnd(func(by, why string) {
					sess.warnf("arc: info: a watch of %s ended, closed by %s: %s\n", url, by, why)
				})
				r.OnAuthFailure(func(err error) {
					sess.warnf("%s: could not authenticate before it read mail, so the relay does not count the read for rings: %s\n", url, err)
				})
			}
			sess.kept[url] = r
		}
		return r
	}
	for _, url := range cfg.URLs {
		sess.Relays = append(sess.Relays, sess.NewRelay(url))
	}
	once := keyer.NewPlainKeySigner(nostr.Generate())
	for _, url := range cfg.IndexerURLs {
		sess.Indexers = append(sess.Indexers, relay.Relay{URL: url, Signer: once})
	}
	sess.Mail, err = mailbox.Open(filepath.Join(cfg.Home, "store"), cfg.Signer, sess.Node, sess.Relays)
	if err != nil {
		sess.receipts.Close()
		sess.roots.Close()
		s.Close()
		return nil, err
	}
	sess.Mail.Indexers = sess.Indexers
	sess.Mail.Contacts = sess.Contacts()
	sess.Mail.InboxRelay = func(url string) transport.Transport {
		return relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(nostr.Generate())}
	}
	sess.Waker = wake.Load(filepath.Join(cfg.Root, wake.FileName), filepath.Join(cfg.Root, wake.StateDirName))
	return sess, nil
}
func (s *Session) Close() {
	s.keptMu.Lock()
	for _, r := range s.kept {
		_ = r.Close()
	}
	s.keptMu.Unlock()
	if s.search != nil {
		s.search.Close()
	}
	if s.Mail != nil {
		_ = s.Mail.Close()
	}
	if s.receipts != nil {
		s.receipts.Close()
	}
	if s.roots != nil {
		s.roots.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}

// warnf writes a warning on the errors of the session. An argument can
// carry text that a relay or a peer chose, so each string, error or
// Stringer argument is escaped on one line (docs/clients/SPEC.md, section
// 3.9). The format is arc's own, and keeps its lines.
func (s *Session) warnf(format string, args ...any) {
	if s.Errors == nil {
		return
	}
	safe := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case error:
			safe[i] = terminal.EscapeLine(v.Error())
		case string:
			safe[i] = terminal.EscapeLine(v)
		case fmt.Stringer:
			safe[i] = terminal.EscapeLine(v.String())
		default:
			safe[i] = arg
		}
	}
	fmt.Fprintf(s.Errors, format, safe...)
}

// warnUnreached warns of a pull that no relay answered, after which the
// command uses what this machine holds. When the network reached no relay,
// it says so in one short line, once in a session: the error of each relay
// is a few hundred characters, and each pull of one command repeats it
// (#396). A relay that answered with a refusal keeps its reason.
func (s *Session) warnUnreached(err error, uses string) {
	if !offline(err) {
		s.warnf("%v\n%s\n", err, uses)
		return
	}
	s.warnOffline()
}

// warnOffline says once in a session that the network reached no relay.
func (s *Session) warnOffline() {
	s.offline.Do(func() {
		s.warnf("arc: no relay answered, so this uses what this machine holds\n")
	})
}

// UnreachedReason is the short reason of an error that Unreachable takes:
// the refusal of a connection, a host that does not exist, or no answer.
func UnreachedReason(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.As(err, &dns) && dns.IsNotFound:
		return "no such host"
	}
	return "no answer"
}

// Unreachable says whether no relay heard the request of err: a dial, a
// lookup or an address that failed, or a relay that gave no answer in time.
func Unreachable(err error) bool { return offline(err) }

// offline says whether each error in err is a dial, a name lookup or an
// address that failed, or a relay that gave no answer in time, so that no
// relay answered the request (#396, #439).
func offline(err error) bool {
	if err == context.DeadlineExceeded || err == relay.ErrStalled || err == relay.ErrNoAnswer { //nolint:errorlint // leaves of the walk below
		return true
	}
	// Each branch of a joined error must be offline, and errors.As takes
	// the first branch that matches, so walk the tree by hand.
	switch e := err.(type) { //nolint:errorlint // walks every branch, as the comment above says
	case *net.OpError, *net.DNSError, *net.AddrError:
		return true
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		return len(errs) > 0 && !slices.ContainsFunc(errs, func(err error) bool { return !offline(err) })
	case interface{ Unwrap() error }:
		return offline(e.Unwrap())
	}
	return false
}

// RefusedLine is the line of sync for the relay name, which refused with
// err: a CLOSED, an OK that is false, a request for authentication, or an
// HTTP status for the WebSocket. ok is false for each other error (#463).
func RefusedLine(name string, err error) (line string, ok bool) {
	reason, ok := relay.Refusal(err)
	if !ok {
		return "", false
	}
	return name + ": the relay refused: " + notice(reason), true
}

// Unusable says whether each relay of err gave no answer or refused, so
// that the one line of sync for each relay says all there is (#463).
func Unusable(err error) bool {
	return err != nil && every(err, func(err error) bool {
		_, refused := relay.Refusal(err)
		return refused || offline(err)
	})
}

// every says whether leaf holds for each branch of the joined errors in
// err. A branch with no joined error under it is a leaf.
func every(err error, leaf func(error) bool) bool {
	switch e := err.(type) { //nolint:errorlint // walks every branch, as offline does
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		return len(errs) > 0 && !slices.ContainsFunc(errs, func(err error) bool { return !every(err, leaf) })
	case interface{ Unwrap() error }:
		if joined(e.Unwrap()) {
			return every(e.Unwrap(), leaf)
		}
	}
	return leaf(err)
}

// joined says whether err holds a joined error.
func joined(err error) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if _, ok := err.(interface{ Unwrap() []error }); ok {
			return true
		}
	}
	return false
}

// noticeLimit is the most characters of a relay NOTICE that arc prints,
// after it escapes the text. A notice that rings fail has about 150.
const noticeLimit = 300

// notice is the text of a relay NOTICE as a terminal can show it: on one
// line, escaped, and cut to noticeLimit characters. The relay chooses the
// text.
func notice(text string) string { return terminal.EscapeLineCut(text, noticeLimit) }

// Reach asks each relay of the session one question at once, which each
// relay answers at once: the event of no ID. A relay that does not answer
// is then down for a while, so the calls that follow fail at once on it.
// It returns the error of each relay, in the order of s.Relays, nil for
// a relay that answered.
func (s *Session) Reach(ctx context.Context) []error {
	errs := make([]error, len(s.Relays))
	var wg sync.WaitGroup
	for i, t := range s.Relays {
		wg.Go(func() { _, errs[i] = t.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{{}}}) })
	}
	wg.Wait()
	return errs
}
