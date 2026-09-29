// Package mailbox composes durable mail with its on-disk journal adapter.
package mailbox

import (
	"path/filepath"

	boltjournal "github.com/gezibash/arc/adapters/journal/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/transport"
)

func Open(dir string, key keys.Signer, n *node.Node, via []transport.Transport) (*mail.Mail, error) {
	return mail.New(boltjournal.Open(filepath.Join(dir, "mail.db")), key, n, via)
}
