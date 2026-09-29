// Package execadapter defines process I/O records carried by core sessions.
package execadapter

import (
	"bufio"
	"encoding/json"
	"github.com/gezibash/arc/core/provider"
	"io"
)

// Data is base64 in JSON. Terminal sessions combine stdout and stderr.
type Record struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
	Exit int    `json:"exit,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

type Reader struct{ scanner *bufio.Scanner }

func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 32*1024)
	return &Reader{scanner: s}
}
func (r *Reader) Next() (Record, error) {
	if !r.scanner.Scan() {
		if r.scanner.Err() != nil {
			return Record{}, r.scanner.Err()
		}
		return Record{}, io.EOF
	}
	var record Record
	if err := json.Unmarshal(r.scanner.Bytes(), &record); err != nil || len(record.Data) > 16*1024 {
		return Record{}, provider.ErrInvalidRequest
	}
	return record, nil
}
