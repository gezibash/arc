package httpadapter

import (
	"bufio"
	"encoding/json"
	"github.com/gezibash/arc/core/provider"
	"io"
)

type SessionReader struct{ scanner *bufio.Scanner }

func NewSessionReader(r io.Reader) *SessionReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2*MaxHTTPBody+4096)
	return &SessionReader{scanner}
}
func (r *SessionReader) Next() (SessionRecord, error) {
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return SessionRecord{}, err
		}
		return SessionRecord{}, io.EOF
	}
	var record SessionRecord
	if err := json.Unmarshal(r.scanner.Bytes(), &record); err != nil || len(record.Data) > MaxHTTPBody || len(record.Text) > MaxHTTPBody {
		return record, provider.ErrInvalidRequest
	}
	return record, nil
}
