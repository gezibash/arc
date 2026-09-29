package citizen

import (
	"fmt"
	"io"
	"path/filepath"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/adapters/mailbox"
	blevesearch "github.com/gezibash/arc/adapters/search/bleve"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/application/wake"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
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
	search   *blevesearch.Index
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
	sess := &Session{Key: cfg.Key, Signer: cfg.Signer, Remote: cfg.Remote, Node: &node.Node{Store: s}, store: s, URLs: cfg.URLs, Errors: cfg.Errors, search: blevesearch.New(filepath.Join(cfg.Home, "store", "search", "journal-v1.bleve"))}
	sess.NewRelay = func(url string) transport.Transport { return relay.Relay{URL: url, Signer: sess.Signer} }
	for _, url := range cfg.URLs {
		sess.Relays = append(sess.Relays, sess.NewRelay(url))
	}
	once := keyer.NewPlainKeySigner(nostr.Generate())
	for _, url := range cfg.IndexerURLs {
		sess.Indexers = append(sess.Indexers, relay.Relay{URL: url, Signer: once})
	}
	sess.Mail, err = mailbox.Open(filepath.Join(cfg.Home, "store"), cfg.Signer, sess.Node, sess.Relays)
	if err != nil {
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
	if s.search != nil {
		s.search.Close()
	}
	if s.Mail != nil {
		s.Mail.Close()
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
