package server

import (
	"context"
	"encoding/json"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/testsession"
	"io"
	"strings"
	"testing"
)

func sqlSession(t *testing.T, s *server, caller string, mode session.Mode, body string) *session.Stream {
	return testsession.Start(t, mode, func(ctx context.Context, stream *session.Stream) error {
		return s.HandleSession(ctx, provider.Request{From: caller, Message: body, Meta: map[string]any{"method": "QUERY", "path": "/main"}}, stream)
	})
}
func sqlRead(t *testing.T, d *json.Decoder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func sqlSend(t *testing.T, stream *session.Stream, d *json.Decoder, text string) []map[string]any {
	t.Helper()
	if _, err := io.WriteString(stream, text+"\n"); err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for {
		v := sqlRead(t, d)
		events = append(events, v)
		if v["type"] == "done" || v["type"] == "error" {
			return events
		}
	}
}
func TestSQLSessionTransactionsAndIsolation(t *testing.T) {
	s := testServer(t)
	s.config.Limits.OutputBytes = 4096
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE items(n INTEGER)"})
	stream := sqlSession(t, s, aliceKey, session.Duplex, "")
	d := json.NewDecoder(stream)
	if sqlRead(t, d)["type"] != "ready" {
		t.Fatal("no prompt before EOF")
	}
	for _, sql := range []string{"BEGIN", "INSERT INTO items VALUES(7)", "COMMIT", "CREATE TEMP TABLE local(n)", "BEGIN", "INSERT INTO items VALUES(9)"} {
		e := sqlSend(t, stream, d, sql)
		if e[len(e)-1]["type"] != "done" {
			t.Fatalf("%s: %v", sql, e)
		}
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	got := reply(t, s, aliceKey, map[string]any{"sql": "SELECT n FROM items"})
	if b, _ := json.Marshal(rowsOf(got)); string(b) != "[[7]]" {
		t.Fatalf("EOF committed uncommitted changes: %s", b)
	}
	other := sqlSession(t, s, aliceKey, session.Duplex, "")
	od := json.NewDecoder(other)
	sqlRead(t, od)
	events := sqlSend(t, other, od, "SELECT * FROM local")
	if events[len(events)-1]["type"] != "error" {
		t.Fatal("temporary state leaked across sessions")
	}
	events = sqlSend(t, other, od, "SELECT 42")
	if events[1]["row"].([]any)[0] != float64(42) {
		t.Fatal("query error ended session", events)
	}
}
func TestSQLSessionAuthorizationAndLimitRollback(t *testing.T) {
	s := testServer(t)
	s.config.Limits.OutputBytes = 4096
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE items(n INTEGER)"})
	denied := sqlSession(t, s, carolKey, session.Duplex, "")
	if _, err := io.ReadAll(denied); err == nil || err.Error() != "unauthorized" {
		t.Fatalf("grant: %v", err)
	}
	reader := sqlSession(t, s, bobKey, session.Duplex, "")
	rd := json.NewDecoder(reader)
	sqlRead(t, rd)
	for _, sql := range []string{"INSERT INTO items VALUES(1)", "ATTACH ':memory:' AS other", "PRAGMA user_version=1"} {
		events := sqlSend(t, reader, rd, sql)
		if events[len(events)-1]["type"] != "error" {
			t.Fatalf("reader executed %s", sql)
		}
	}
	writer := sqlSession(t, s, aliceKey, session.Duplex, "")
	wd := json.NewDecoder(writer)
	sqlRead(t, wd)
	events := sqlSend(t, writer, wd, "INSERT INTO items VALUES(1),(2),(3) RETURNING n")
	if events[len(events)-1]["error"] != "result_too_large" {
		t.Fatal(events)
	}
	got := reply(t, s, aliceKey, map[string]any{"sql": "SELECT count(*) FROM items"})
	if b, _ := json.Marshal(rowsOf(got)); string(b) != "[[0]]" {
		t.Fatalf("limit failed to roll back: %s", b)
	}
}
func TestSQLServerStreamAndCancellation(t *testing.T) {
	s := testServer(t)
	s.config.Limits.OutputBytes = 4096
	stream := sqlSession(t, s, aliceKey, session.ServerStream, `{"sql":"SELECT 42 AS answer"}`)
	data, err := io.ReadAll(stream)
	if err != nil || !strings.Contains(string(data), `"row":[42]`) || stream.Wait() != nil {
		t.Fatalf("stream: %s %v", data, err)
	}
	active := sqlSession(t, s, aliceKey, session.Duplex, "")
	d := json.NewDecoder(active)
	sqlRead(t, d)
	sqlSend(t, active, d, "BEGIN")
	sqlSend(t, active, d, "CREATE TABLE canceled(n)")
	_ = active.Close()
	// A new query must eventually acquire the writer lock after session cancellation.
	refused(t, s, aliceKey, map[string]any{"sql": "SELECT * FROM canceled"}, "query_failed")
}
