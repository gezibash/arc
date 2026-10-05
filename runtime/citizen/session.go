package citizen

import (
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/adapters/mailbox"
	blevesearch "github.com/gezibash/arc/adapters/search/bleve"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/wake"
)

// Session is a citizen's application state. Cobra supplies configuration;
// operations here can also serve another UI or an embedded caller.
type Session struct {
	Key      keys.Key
	Signer   keys.Signer
	Remote   bool
	Node     *node.Node
	Mail     *mail.Mail
	Relays   []transport.Transport
	Indexers []transport.Transport
	URLs     []string
	Waker    *wake.Waker
	NewRelay func(string) transport.Transport
	Errors   io.Writer
	store    *store.Store
	receipts kv.Store
	search   *blevesearch.Index
	// kept holds the relay of each URL that NewRelay gave.
	keptMu sync.Mutex
	kept   map[string]relay.Relay
}

type Config struct {
	Home, Root        string
	Key               keys.Key
	Signer            keys.Signer
	Remote            bool
	URLs, IndexerURLs []string
	Errors            io.Writer
}

// Open composes filesystem, relay, mail and wake adapters for a local citizen.
func Open(cfg Config) (*Session, error) {
	s, err := boltstore.Open(filepath.Join(cfg.Home, "store"))
	if err != nil {
		return nil, err
	}
	sess := &Session{Key: cfg.Key, Signer: cfg.Signer, Remote: cfg.Remote, store: s, URLs: cfg.URLs, Errors: cfg.Errors, search: blevesearch.New(filepath.Join(cfg.Home, "store", "search", "journal-v1.bleve")), kept: map[string]relay.Relay{}}
	sess.receipts = boltkv.Open(filepath.Join(cfg.Home, "store", "receipts.db"))
	sess.Node = &node.Node{Store: receiptStore{EventStore: s, db: sess.receipts, me: cfg.Signer.PublicKey()}}
	// Each relay keeps one connection for its sends. One URL gives one relay,
	// so a long run does not open a connection for each call.
	sess.NewRelay = func(url string) transport.Transport {
		sess.keptMu.Lock()
		defer sess.keptMu.Unlock()
		r, ok := sess.kept[url]
		if !ok {
			r = relay.New(url, sess.Signer)
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
		s.Close()
		return nil, err
	}
	sess.Mail.Indexers = sess.Indexers
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
	if s.store != nil {
		s.store.Close()
	}
}
func (s *Session) warnf(format string, args ...any) {
	if s.Errors != nil {
		fmt.Fprintf(s.Errors, format, args...)
	}
}
