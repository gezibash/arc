package provider

import (
	"context"
	"io"
	"sync"
	"time"
)

// Output has a short drain period after EOF. Cancellation notices use the
// same allowance; they must not keep a canceled caller waiting indefinitely.
const outputGrace = time.Second

// output serializes complete protocol lines. Only one Write can be active,
// including when a custom writer cannot be interrupted. A blocking writer
// must implement Close so an aborted stream releases that last Write too.
type output struct {
	writer    io.Writer
	ctx       context.Context
	cancel    context.CancelCauseFunc
	gate      chan struct{}
	closeOnce sync.Once
}

func newOutput(ctx context.Context, writer io.Writer) *output {
	ctx, cancel := context.WithCancelCause(ctx)
	return &output{writer: writer, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
}

func (o *output) abort(err error) {
	o.cancel(err)
	o.closeOnce.Do(func() {
		if closer, ok := o.writer.(io.Closer); ok {
			closer.Close()
		}
	})
}

func (o *output) write(ctx context.Context, line []byte) error {
	select {
	case o.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-o.ctx.Done():
		return context.Cause(o.ctx)
	}
	if err := ctx.Err(); err != nil {
		<-o.gate
		return err
	}
	if err := context.Cause(o.ctx); err != nil {
		<-o.gate
		return err
	}
	// The active writer keeps the gate until Write returns. Cancellation
	// cannot start another writer behind a partial JSON line.
	done := make(chan error, 1)
	go func() {
		defer func() { <-o.gate }()
		body := append(line, '\n')
		n, err := o.writer.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		if err != nil {
			o.abort(err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			o.abort(err)
		}
		return err
	case <-ctx.Done():
		o.abort(ctx.Err())
		return ctx.Err()
	case <-o.ctx.Done():
		err := context.Cause(o.ctx)
		o.abort(err)
		return err
	}
}
