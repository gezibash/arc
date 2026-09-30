// Package limitio retains a bounded prefix while continuing to drain output.
package limitio

import "bytes"

type Buffer struct {
	Max       int
	buffer    bytes.Buffer
	Truncated bool
}

func (b *Buffer) Len() int      { return b.buffer.Len() }
func (b *Buffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, max(0, b.Max-b.Len()))
	_, _ = b.buffer.Write(p[:keep])
	b.Truncated = b.Truncated || keep < n
	return n, nil
}
