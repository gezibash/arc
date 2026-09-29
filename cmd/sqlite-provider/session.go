package main

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"zombiezen.com/go/sqlite"
)

// A session owns a connection. SQL and transaction policy remain in this provider.
// Results are NDJSON: columns, row, statement, then done (or error).
func (s *server) HandleSession(ctx context.Context, req provider.Request, stream *session.Stream) error {
	if req.Method() != "QUERY" {
		return errInvalidRequest
	}
	held, role, err := s.authorized(req.From, req.Path())
	if err != nil {
		return err
	}
	conn, guard, err := s.connect(ctx, held, role)
	if err != nil {
		return err
	}
	defer func() {
		conn.SetInterrupt(nil)
		if !conn.AutocommitEnabled() {
			_ = guard.internal(conn, "ROLLBACK")
		}
		_ = conn.Close()
	}()
	out := json.NewEncoder(stream)
	run := func(text string) error {
		err := s.sessionQuery(ctx, conn, guard, text, out)
		if err != nil {
			if stream.Mode() == session.ServerStream {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A query error is recoverable. It never becomes a successful final result.
			return out.Encode(map[string]any{"type": "error", "error": err.Error(), "transaction": !conn.AutocommitEnabled()})
		}
		return nil
	}
	if strings.TrimSpace(req.Message) != "" {
		if err := run(req.Message); err != nil {
			return err
		}
	} else if stream.Mode() == session.ServerStream {
		return errInvalidRequest
	}
	if stream.Mode() == session.ServerStream {
		return nil
	}
	if err := out.Encode(map[string]string{"type": "ready"}); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 4096), s.config.Limits.BodyBytes+1)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == ".quit" {
			return nil
		}
		if text == "" {
			continue
		}
		if err := run(text); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return errRequestTooLarge
	}
	return ctx.Err()
}

func (s *server) sessionQuery(ctx context.Context, conn *sqlite.Conn, guard *guard, text string, out *json.Encoder) error {
	if len(text) > s.config.Limits.BodyBytes {
		return errRequestTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.config.Limits.QueryMS)*time.Millisecond)
	defer cancel()
	conn.SetInterrupt(ctx.Done())
	defer conn.SetInterrupt(nil)
	guard.mu.Lock()
	guard.reason = errQueryDenied
	guard.mu.Unlock()
	// Only these exact control commands use the trusted path. Arbitrary SQL still
	// passes the authorizer, including ATTACH, PRAGMA and SAVEPOINT denials.
	control := strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ";")))
	switch control {
	case "BEGIN", "BEGIN TRANSACTION", "COMMIT", "END", "ROLLBACK":
		if err := guard.internal(conn, control); err != nil {
			return guard.failure(ctx, err)
		}
		return out.Encode(map[string]any{"type": "done", "transaction": !conn.AutocommitEnabled()})
	}
	var statements []statement
	var err error
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		statements, err = s.parseRequest(text)
	} else {
		statements, err = s.checkStatements([]statement{{SQL: text}})
	}
	if err != nil {
		return err
	}
	// The savepoint keeps a failed request atomic, even inside an explicit transaction.
	if err := guard.internal(conn, "SAVEPOINT arc_request"); err != nil {
		return guard.failure(ctx, err)
	}
	released := false
	defer func() {
		if !released {
			conn.SetInterrupt(nil)
			_ = guard.internal(conn, "ROLLBACK TO arc_request")
			_ = guard.internal(conn, "RELEASE arc_request")
		}
	}()
	budget, encoded := 0, 0
	emit := func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return errQueryFailed
		}
		encoded += len(data) + 1
		if encoded > s.config.Limits.OutputBytes {
			return errResultTooLarge
		}
		return out.Encode(v)
	}
	for index, one := range statements {
		got, err := s.executeRows(ctx, conn, guard, one, &budget, func(columns []string, row []any) error {
			if columns != nil {
				return emit(map[string]any{"type": "columns", "statement": index, "columns": columns})
			}
			return emit(map[string]any{"type": "row", "statement": index, "row": row})
		})
		if err != nil {
			return err
		}
		if err := emit(map[string]any{"type": "statement", "statement": index, "changes": got.Changes}); err != nil {
			return err
		}
	}
	if err := guard.internal(conn, "RELEASE arc_request"); err != nil {
		return guard.failure(ctx, err)
	}
	released = true
	return out.Encode(map[string]any{"type": "done", "transaction": !conn.AutocommitEnabled()})
}
