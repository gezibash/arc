// Package ndjson reads newline-delimited JSON records from a session stream.
package ndjson

import (
	"bufio"
	"encoding/json"
	"io"

	"github.com/gezibash/arc/core/provider"
)

// Reader reads one record of type T from each line.
type Reader[T any] struct {
	scanner *bufio.Scanner
	valid   func(T) bool
}

// NewReader reads lines of at most maxLine bytes. If valid returns false for
// a record, Next returns provider.ErrInvalidRequest.
func NewReader[T any](r io.Reader, maxLine int, valid func(T) bool) *Reader[T] {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxLine)
	return &Reader[T]{scanner: scanner, valid: valid}
}

// Next returns the next record. It returns io.EOF at the end of the stream.
func (r *Reader[T]) Next() (T, error) {
	var record T
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return record, err
		}
		return record, io.EOF
	}
	if err := json.Unmarshal(r.scanner.Bytes(), &record); err != nil || !r.valid(record) {
		var zero T
		return zero, provider.ErrInvalidRequest
	}
	return record, nil
}
