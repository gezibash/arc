package citizen_test

import (
	"encoding/hex"
	"errors"

	"github.com/gezibash/arc/client"
)

func hexOf(key []byte) string { return hex.EncodeToString(key) }

func asRemote(err error, into **client.RemoteError) bool {
	return errors.As(err, into)
}
