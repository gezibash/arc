// Package wire defines the language-independent provider JSON protocol.
package wire

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"
)

const (
	RequestTimeout = 2 * time.Minute
	// Nested calls and command execution leave time to return their result.
	WorkTimeout   = RequestTimeout - 5*time.Second
	MaxConcurrent = 64
)

// Event is one newline-delimited message. Pointers distinguish empty payloads
// from absent payloads. Request IDs can be strings or integer JSON numbers.
type Event struct {
	Op           string         `json:"op"`
	RequestID    any            `json:"request_id"`
	From         string         `json:"from,omitempty"`
	Message      *string        `json:"message,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
	ArcSessionID string         `json:"arc_session_id,omitempty"`
	AppSessionID string         `json:"app_session_id,omitempty"`
	Framed       bool           `json:"framed,omitempty"`
	DeadlineMS   int64          `json:"deadline_ms,omitempty"`
	CallID       string         `json:"call_id,omitempty"`
	Address      string         `json:"address,omitempty"`
	Body         *string        `json:"body,omitempty"`
	Reply        *string        `json:"reply,omitempty"`
	Refused      string         `json:"refused,omitempty"`
	Error        string         `json:"error,omitempty"`
}

func Text(s string) *string { return &s }

// Budget bounds an operation by the parent's deadline, a supplied deadline,
// and the protocol's maximum request lifetime, whichever comes first.
func Budget(ctx context.Context, deadlineMS int64) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(RequestTimeout)
	if deadlineMS > 0 && time.UnixMilli(deadlineMS).Before(deadline) {
		deadline = time.UnixMilli(deadlineMS)
	}
	return context.WithDeadline(ctx, deadline)
}
func Deadline(ctx context.Context) int64 {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline.UnixMilli()
	}
	return 0
}

var ErrLineTooLong = errors.New("provider: the line is too long")

// ReadLine discards an oversized line without retaining its bytes, including
// a final unterminated line. The next call starts at the next line.
func ReadLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			tooLong = true
			line = nil
		}
		if !tooLong {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, ErrLineTooLong
		}
		if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
			return nil, err
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		return line, nil
	}
}
