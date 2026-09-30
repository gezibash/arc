// Package execadapter defines process I/O records carried by core sessions.
package execadapter

import (
	"io"

	"github.com/gezibash/arc/adapters/ndjson"
)

// Data is base64 in JSON. Terminal sessions combine stdout and stderr.
type Record struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
	Exit int    `json:"exit,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

type Reader = ndjson.Reader[Record]

// NewReader reads Exec records. One record carries at most 16 KiB of data.
func NewReader(r io.Reader) *Reader {
	return ndjson.NewReader(r, 32*1024, func(record Record) bool { return len(record.Data) <= 16*1024 })
}
