package httpadapter

import (
	"io"

	"github.com/gezibash/arc/adapters/ndjson"
)

type SessionReader = ndjson.Reader[SessionRecord]

// NewSessionReader reads HTTP session records. One record carries at most
// MaxHTTPBody bytes of data or text.
func NewSessionReader(r io.Reader) *SessionReader {
	return ndjson.NewReader(r, 2*MaxHTTPBody+4096, func(record SessionRecord) bool {
		return len(record.Data) <= MaxHTTPBody && len(record.Text) <= MaxHTTPBody
	})
}
