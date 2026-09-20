package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/provider"
	"zombiezen.com/go/sqlite"
)

const (
	aliceKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bobKey   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	carolKey = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// testServer gives alice the write role and bob the read role on one
// database, with small limits.
func testServer(t *testing.T) *server {
	t.Helper()

	dir := t.TempDir()
	return &server{config: &config{
		Databases: map[string]database{
			"main": {
				Path:   filepath.Join(dir, "main.db"),
				Grants: map[string]string{aliceKey: "write", bobKey: "read"},
			},
		},
		Limits: limits{
			BodyBytes: 256 * 1024, SQLBytes: 64 * 1024, BatchStatements: 32,
			Rows: 2, OutputBytes: 256, QueryMS: 1000, BusyMS: 1000,
		},
	}}
}

func ask(t *testing.T, s *server, caller string, body map[string]any, path ...string) (map[string]any, error) {
	t.Helper()

	where := "/main"
	if len(path) == 1 {
		where = path[0]
	}

	message, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	reply, err := s.HandleRequest(context.Background(), provider.Request{
		From:    caller,
		Message: string(message),
		Meta:    map[string]any{"method": "QUERY", "path": where},
	})
	if err != nil {
		return nil, err
	}

	var answer map[string]any
	if err := json.Unmarshal([]byte(reply), &answer); err != nil {
		t.Fatalf("the reply is not JSON: %q", reply)
	}
	return answer, nil
}

func reply(t *testing.T, s *server, caller string, body map[string]any) map[string]any {
	t.Helper()

	answer, err := ask(t, s, caller, body)
	if err != nil {
		t.Fatalf("%v: %v", body, err)
	}
	return answer
}

// rowsOf reads the rows of the first result.
func rowsOf(answer map[string]any) []any {
	results, _ := answer["results"].([]any)
	if len(results) == 0 {
		return nil
	}
	first, _ := results[0].(map[string]any)
	rows, _ := first["rows"].([]any)
	return rows
}

func refused(t *testing.T, s *server, caller string, body map[string]any, want string, path ...string) {
	t.Helper()

	_, err := ask(t, s, caller, body, path...)
	if err == nil {
		t.Fatalf("%v passed", body)
	}
	if err.Error() != want {
		t.Errorf("%v gave %q, want %q", body, err, want)
	}
}

func TestWriteReadAndBlobRoundTrip(t *testing.T) {
	s := testServer(t)

	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(id INTEGER PRIMARY KEY, body BLOB)"})
	reply(t, s, aliceKey, map[string]any{
		"sql":    "INSERT INTO notes(body) VALUES(?)",
		"params": []any{map[string]any{"base64": "AAEC"}},
	})

	answer := reply(t, s, bobKey, map[string]any{"sql": "SELECT body FROM notes"})
	results, _ := answer["results"].([]any)
	first, _ := results[0].(map[string]any)

	columns, _ := first["columns"].([]any)
	if len(columns) != 1 || columns[0] != "body" {
		t.Errorf("columns = %v", columns)
	}

	rows := rowsOf(answer)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	row, _ := rows[0].([]any)
	blob, _ := row[0].(map[string]any)
	if blob["base64"] != "AAEC" {
		t.Errorf("the blob is %v", blob)
	}
}

func TestNamedParametersAndTypes(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE kinds(a, b, c, d)"})

	reply(t, s, aliceKey, map[string]any{
		"sql":    "INSERT INTO kinds VALUES(:a, :b, :c, :d)",
		"params": map[string]any{"a": "text", "b": 42, "c": 1.5, "d": nil},
	})

	rows := rowsOf(reply(t, s, bobKey, map[string]any{"sql": "SELECT a, b, c, d FROM kinds"}))
	row, _ := rows[0].([]any)

	if row[0] != "text" || row[1] != float64(42) || row[2] != 1.5 || row[3] != nil {
		t.Errorf("the row is %v", row)
	}
}

func TestAReaderCannotWrite(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(body TEXT)"})

	refused(t, s, bobKey, map[string]any{"sql": "INSERT INTO notes(body) VALUES('no')"}, "write_denied")
	refused(t, s, bobKey, map[string]any{"sql": "DROP TABLE notes"}, "write_denied")

	// A reader still reads.
	reply(t, s, aliceKey, map[string]any{"sql": "INSERT INTO notes(body) VALUES('visible')"})
	rows := rowsOf(reply(t, s, bobKey, map[string]any{"statements": []any{
		map[string]any{"sql": "SELECT body FROM notes"},
	}}))
	if len(rows) != 1 {
		t.Errorf("the reader saw %v", rows)
	}
}

func TestADatabaseWithoutAGrantIsRefused(t *testing.T) {
	s := testServer(t)

	refused(t, s, carolKey, map[string]any{"sql": "SELECT 1"}, "unauthorized")
	refused(t, s, aliceKey, map[string]any{"sql": "SELECT 1"}, "not_found", "/missing")
	refused(t, s, "not-a-key", map[string]any{"sql": "SELECT 1"}, "unauthorized")
	refused(t, s, aliceKey, map[string]any{"sql": "SELECT 1"}, "invalid_request", "/UPPER")
}

func TestABatchIsAllOrNothing(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(body TEXT UNIQUE)"})

	refused(t, s, aliceKey, map[string]any{"statements": []any{
		map[string]any{"sql": "INSERT INTO notes(body) VALUES(?)", "params": []any{"first"}},
		map[string]any{"sql": "INSERT INTO notes(body) VALUES(?)", "params": []any{"first"}},
	}}, "query_failed")

	rows := rowsOf(reply(t, s, aliceKey, map[string]any{"sql": "SELECT count(*) FROM notes"}))
	row, _ := rows[0].([]any)
	if row[0] != float64(0) {
		t.Errorf("the table holds %v rows after the failed batch", row[0])
	}
}

// The authorizer is the boundary, and it holds for the SQL of a transaction.
func TestTheAuthorizerRefusesWhatIsNotSafe(t *testing.T) {
	s := testServer(t)

	for _, sql := range []string{
		"BEGIN",
		"COMMIT",
		"SAVEPOINT one",
		"ATTACH DATABASE ':memory:' AS other",
		"DETACH DATABASE main",
		"PRAGMA user_version",
		"VACUUM INTO 'outside.db'",
	} {
		refused(t, s, aliceKey, map[string]any{"sql": sql}, "query_denied")
	}
}

// SQLite itself refuses to load an extension, and this build holds no
// readfile or writefile.
func TestTheDangerousFunctionsDoNotRun(t *testing.T) {
	s := testServer(t)

	refused(t, s, aliceKey, map[string]any{"sql": "SELECT load_extension('/tmp/x.so')"}, "query_failed")
	refused(t, s, aliceKey, map[string]any{"sql": "SELECT readfile('/etc/passwd')"}, "query_failed")
	refused(t, s, aliceKey, map[string]any{"sql": "SELECT writefile('/tmp/x', 'y')"}, "query_failed")
}

func TestTheRowLimitRefusesAndNeverCuts(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(body TEXT)"})

	for _, body := range []string{"one", "two", "three"} {
		reply(t, s, aliceKey, map[string]any{
			"sql": "INSERT INTO notes(body) VALUES(?)", "params": []any{body},
		})
	}

	// The limit of this test is two rows.
	refused(t, s, bobKey, map[string]any{"sql": "SELECT body FROM notes"}, "result_too_large")

	rows := rowsOf(reply(t, s, bobKey, map[string]any{"sql": "SELECT body FROM notes LIMIT 2"}))
	if len(rows) != 2 {
		t.Errorf("the reader saw %d rows", len(rows))
	}
}

func TestAWriteThatReturnsTooMuchRollsBack(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(body TEXT)"})

	long := strings.Repeat("x", 300)
	refused(t, s, aliceKey, map[string]any{
		"sql":    "INSERT INTO notes(body) VALUES(?) RETURNING body",
		"params": []any{long},
	}, "result_too_large")

	rows := rowsOf(reply(t, s, aliceKey, map[string]any{"sql": "SELECT count(*) FROM notes"}))
	row, _ := rows[0].([]any)
	if row[0] != float64(0) {
		t.Errorf("the table holds %v rows after the failed write", row[0])
	}
}

func TestAValueOverTheLimitIsRefused(t *testing.T) {
	s := testServer(t)

	refused(t, s, aliceKey, map[string]any{
		"sql": "SELECT zeroblob(1000000)",
	}, "result_too_large")
}

func TestABodyThatIsNotARequest(t *testing.T) {
	s := testServer(t)

	cases := []any{
		map[string]any{},
		map[string]any{"sql": "SELECT 1", "statements": []any{}},
		map[string]any{"statements": []any{}},
		map[string]any{"sql": "   "},
		map[string]any{"sql": "SELECT 1", "colour": "red"},
		map[string]any{"statements": []any{map[string]any{"sql": "SELECT 1", "colour": "red"}}},
		[]any{"SELECT 1"},
	}

	for _, body := range cases {
		message, _ := json.Marshal(body)
		_, err := s.HandleRequest(context.Background(), provider.Request{
			From:    aliceKey,
			Message: string(message),
			Meta:    map[string]any{"method": "QUERY", "path": "/main"},
		})
		if err == nil {
			t.Errorf("%s passed", message)
		}
	}

	// The method of the capability names the operation.
	_, err := s.HandleRequest(context.Background(), provider.Request{
		From: aliceKey, Message: `{"sql":"SELECT 1"}`,
		Meta: map[string]any{"method": "EXEC", "path": "/main"},
	})
	if err == nil {
		t.Error("another method passed")
	}
}

func TestABodyOverTheLimitIsRefused(t *testing.T) {
	s := testServer(t)
	s.config.Limits.BodyBytes = 64

	refused(t, s, aliceKey, map[string]any{"sql": "SELECT '" + strings.Repeat("x", 100) + "'"}, "request_too_large")
}

func TestOneRequestHoldsOneStatement(t *testing.T) {
	s := testServer(t)

	refused(t, s, aliceKey, map[string]any{
		"sql": "CREATE TABLE a(x); CREATE TABLE b(x)",
	}, "invalid_request")
}

func TestABatchOverTheStatementLimit(t *testing.T) {
	s := testServer(t)
	s.config.Limits.BatchStatements = 2

	statements := make([]any, 3)
	for index := range statements {
		statements[index] = map[string]any{"sql": "SELECT 1"}
	}
	refused(t, s, aliceKey, map[string]any{"statements": statements}, "invalid_request")
}

func TestLoadConfigChecksTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("SQLITE_CONFIG", path)

	good := fmt.Sprintf(`{"databases":{"main":{"path":"%s/main.db","grants":{"%s":"write"}}}}`, dir, aliceKey)
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if held.Limits != defaultLimits || held.Databases["main"].Grants[aliceKey] != "write" {
		t.Errorf("config = %+v", held)
	}

	bad := map[string]string{
		"no databases":             `{"databases":{}}`,
		"a relative path":          fmt.Sprintf(`{"databases":{"main":{"path":"main.db","grants":{"%s":"write"}}}}`, aliceKey),
		"an unknown role":          fmt.Sprintf(`{"databases":{"main":{"path":"/tmp/a.db","grants":{"%s":"root"}}}}`, aliceKey),
		"a key that is not one":    `{"databases":{"main":{"path":"/tmp/a.db","grants":{"abc":"read"}}}}`,
		"no grants":                `{"databases":{"main":{"path":"/tmp/a.db","grants":{}}}}`,
		"a name with a space":      fmt.Sprintf(`{"databases":{"my db":{"path":"/tmp/a.db","grants":{"%s":"read"}}}}`, aliceKey),
		"an unknown field":         fmt.Sprintf(`{"databases":{"main":{"path":"/tmp/a.db","grants":{"%s":"read"}}},"colour":"red"}`, aliceKey),
		"a limit over the ceiling": fmt.Sprintf(`{"databases":{"main":{"path":"/tmp/a.db","grants":{"%s":"read"}}},"limits":{"rows":99999999}}`, aliceKey),
	}

	for name, body := range bad {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s: the configuration passed", name)
		}
	}

	t.Setenv("SQLITE_CONFIG", "relative.json")
	if _, err := loadConfig(); err == nil {
		t.Error("a relative configuration path passed")
	}
}

// A reader opens the file read only, so it cannot write even if the
// authorizer let it through.
func TestAReaderOpensTheFileReadOnly(t *testing.T) {
	s := testServer(t)
	reply(t, s, aliceKey, map[string]any{"sql": "CREATE TABLE notes(body TEXT)"})

	held := s.config.Databases["main"]
	conn, guard, err := s.connect(context.Background(), held, "read")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	guard.mu.Lock()
	guard.inside = true
	guard.mu.Unlock()

	stmt, _, err := conn.PrepareTransient("INSERT INTO notes(body) VALUES('x')")
	if err != nil {
		t.Skipf("the statement did not prepare: %v", err)
	}
	defer stmt.Finalize()

	if _, err := stmt.Step(); err == nil {
		t.Error("a read only connection wrote")
	} else if sqlite.ErrCode(err) == sqlite.ResultOK {
		t.Errorf("the error is %v", err)
	}
}
