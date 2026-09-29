package mail

import (
	"path/filepath"

	boltjournal "github.com/gezibash/arc/adapters/journal/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/transport"
)

func openDiskMail(dir string, key keys.Signer, n *node.Node, via []transport.Transport) (*Mail, error) {
	return New(boltjournal.Open(filepath.Join(dir, "mail.db")), key, n, via)
}
