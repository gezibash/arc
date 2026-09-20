package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gezibash/arc/go/provider"
	"zombiezen.com/go/sqlite"
)

// The errors that a caller may see. Every other failure answers
// "query_failed", so the message of SQLite never leaves the machine.
const (
	errInvalidRequest  = provider.Error("invalid_request")
	errRequestTooLarge = provider.Error("request_too_large")
	errUnauthorized    = provider.Error("unauthorized")
	errNotFound        = provider.Error("not_found")
	errQueryDenied     = provider.Error("query_denied")
	errWriteDenied     = provider.Error("write_denied")
	errQueryTimeout    = provider.Error("query_timeout")
	errResultTooLarge  = provider.Error("result_too_large")
	errQueryFailed     = provider.Error("query_failed")
)

// statement is one SQL statement and its parameters.
type statement struct {
	SQL    string          `json:"sql"`
	Params json.RawMessage `json:"params"`
}

// result is what one statement returned.
type result struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Changes int      `json:"changes"`
}

// query runs the statements of one request against one database.
func (s *server) query(caller, path, message string) (map[string]any, error) {
	if !publicKeyPattern.MatchString(caller) {
		return nil, errUnauthorized
	}
	if !pathPattern.MatchString(path) {
		return nil, errInvalidRequest
	}

	held, ok := s.config.Databases[strings.TrimPrefix(path, "/")]
	if !ok {
		return nil, errNotFound
	}

	role, ok := held.Grants[caller]
	if !ok {
		return nil, errUnauthorized
	}

	statements, err := s.parseRequest(message)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.Limits.QueryMS)*time.Millisecond)
	defer cancel()

	conn, guard, err := s.connect(ctx, held, role)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	begin := "BEGIN"
	if role == "write" {
		begin = "BEGIN IMMEDIATE"
	}
	if err := guard.internal(conn, begin); err != nil {
		return nil, errQueryFailed
	}

	results := make([]any, 0, len(statements))
	budget := 0

	for _, one := range statements {
		got, err := s.execute(ctx, conn, guard, one, &budget)
		if err != nil {
			guard.internal(conn, "ROLLBACK")
			return nil, err
		}
		results = append(results, got)
	}

	answer := map[string]any{"results": results}
	if err := s.withinOutput(answer); err != nil {
		guard.internal(conn, "ROLLBACK")
		return nil, err
	}
	if err := guard.internal(conn, "COMMIT"); err != nil {
		guard.internal(conn, "ROLLBACK")
		return nil, errQueryFailed
	}
	return answer, nil
}

// connect opens one database for one role, and puts the authorizer in place.
func (s *server) connect(ctx context.Context, held database, role string) (*sqlite.Conn, *guard, error) {
	flags := sqlite.OpenReadOnly
	if role == "write" {
		flags = sqlite.OpenReadWrite | sqlite.OpenCreate
		if err := os.MkdirAll(filepath.Dir(held.Path), 0o700); err != nil {
			return nil, nil, errQueryFailed
		}
	}

	conn, err := sqlite.OpenConn(held.Path, flags)
	if err != nil {
		return nil, nil, errQueryFailed
	}

	conn.SetInterrupt(ctx.Done())
	conn.SetBusyTimeout(time.Duration(s.config.Limits.BusyMS) * time.Millisecond)

	// SQLite checks these before it holds one value or parses a long
	// statement. The output budget is checked again as rows arrive.
	conn.Limit(sqlite.LimitLength, int32(s.config.Limits.OutputBytes))
	conn.Limit(sqlite.LimitSQLLength, int32(s.config.Limits.SQLBytes))

	held_guard := &guard{role: role, reason: errQueryDenied}
	if err := conn.SetAuthorizer(held_guard); err != nil {
		conn.Close()
		return nil, nil, errQueryFailed
	}

	for _, pragma := range []string{
		"PRAGMA trusted_schema=OFF",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA foreign_keys=ON",
	} {
		if err := held_guard.internal(conn, pragma); err != nil {
			conn.Close()
			return nil, nil, errQueryFailed
		}
	}
	return conn, held_guard, nil
}

// execute runs one statement and reads its rows.
func (s *server) execute(ctx context.Context, conn *sqlite.Conn, held *guard, one statement, budget *int) (*result, error) {
	if ctx.Err() != nil {
		return nil, errQueryTimeout
	}

	stmt, trailing, err := conn.PrepareTransient(one.SQL)
	if err != nil {
		return nil, held.failure(ctx, err)
	}
	defer stmt.Finalize()

	// One request holds one statement. Anything after it is a second
	// statement that nothing checked.
	if trailing > 0 {
		return nil, errInvalidRequest
	}
	if err := bind(stmt, one.Params); err != nil {
		return nil, err
	}

	got := &result{Columns: []string{}, Rows: [][]any{}}
	for index := 0; index < stmt.ColumnCount(); index++ {
		got.Columns = append(got.Columns, stmt.ColumnName(index))
	}

	if len(got.Columns) > 0 {
		*budget += size(got.Columns)
	}

	for {
		if ctx.Err() != nil {
			return nil, errQueryTimeout
		}

		hasRow, err := stmt.Step()
		if err != nil {
			return nil, held.failure(ctx, err)
		}
		if !hasRow {
			break
		}
		if len(got.Rows) >= s.config.Limits.Rows {
			return nil, errResultTooLarge
		}

		row, err := readRow(stmt)
		if err != nil {
			return nil, err
		}

		*budget += size(row)
		if *budget > s.config.Limits.OutputBytes {
			return nil, errResultTooLarge
		}
		got.Rows = append(got.Rows, row)
	}

	// A statement that returns no column changed rows instead.
	if len(got.Columns) == 0 && conn.Changes() > 0 {
		got.Changes = conn.Changes()
	}
	return got, nil
}

// guard is the authorizer. It is the boundary of the provider, and it holds
// for the SQL of a transaction as well.
type guard struct {
	role string

	mu     sync.Mutex
	inside bool
	reason provider.Error
}

// Authorize answers one action of SQLite.
func (g *guard) Authorize(action sqlite.Action) sqlite.AuthResult {
	g.mu.Lock()
	inside := g.inside
	g.mu.Unlock()

	if inside {
		return sqlite.AuthResultOK
	}

	switch action.Type() {
	case sqlite.OpAttach, sqlite.OpDetach, sqlite.OpPragma,
		sqlite.OpTransaction, sqlite.OpSavepoint:
		return sqlite.AuthResultDeny

	case sqlite.OpFunction:
		// The name of the function does not reach an authorizer in this
		// library. The build of SQLite here holds no load_extension,
		// readfile or writefile, and a test proves it.
		return sqlite.AuthResultOK
	}

	if g.role == "read" && changes(action.Type()) {
		g.mu.Lock()
		g.reason = errWriteDenied
		g.mu.Unlock()
		return sqlite.AuthResultDeny
	}
	return sqlite.AuthResultOK
}

// internal runs one statement of the provider itself, which the authorizer
// lets through.
func (g *guard) internal(conn *sqlite.Conn, sql string) error {
	g.mu.Lock()
	g.inside = true
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		g.inside = false
		g.mu.Unlock()
	}()

	stmt, _, err := conn.PrepareTransient(sql)
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	for {
		hasRow, err := stmt.Step()
		if err != nil {
			return err
		}
		if !hasRow {
			return nil
		}
	}
}

// failure turns the error of SQLite into one that is safe to send.
func (g *guard) failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errQueryTimeout
	}

	// A statement that asks for its own transaction is refused, because the
	// provider holds one already.
	if strings.Contains(strings.ToLower(err.Error()), "within a transaction") {
		return errQueryDenied
	}

	switch sqlite.ErrCode(err) {
	case sqlite.ResultInterrupt:
		return errQueryTimeout
	case sqlite.ResultTooBig:
		return errResultTooLarge
	case sqlite.ResultAuth:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.reason
	default:
		return errQueryFailed
	}
}

// changes says whether an action changes the database.
func changes(op sqlite.OpType) bool {
	switch op {
	case sqlite.OpInsert, sqlite.OpUpdate, sqlite.OpDelete,
		sqlite.OpCreateIndex, sqlite.OpCreateTable, sqlite.OpCreateTempIndex,
		sqlite.OpCreateTempTable, sqlite.OpCreateTempTrigger, sqlite.OpCreateTempView,
		sqlite.OpCreateTrigger, sqlite.OpCreateView, sqlite.OpCreateVTable,
		sqlite.OpDropIndex, sqlite.OpDropTable, sqlite.OpDropTempIndex,
		sqlite.OpDropTempTable, sqlite.OpDropTempTrigger, sqlite.OpDropTempView,
		sqlite.OpDropTrigger, sqlite.OpDropView, sqlite.OpDropVTable,
		sqlite.OpAlterTable, sqlite.OpReindex, sqlite.OpAnalyze:
		return true
	default:
		return false
	}
}

// -- the request -------------------------------------------------------------

// parseRequest reads the body: one statement, or a batch of them.
func (s *server) parseRequest(message string) ([]statement, error) {
	if len(message) > s.config.Limits.BodyBytes {
		return nil, errRequestTooLarge
	}

	var body struct {
		SQL        *string         `json:"sql"`
		Params     json.RawMessage `json:"params"`
		Statements []statement     `json:"statements"`
	}

	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		return nil, errInvalidRequest
	}

	single := body.SQL != nil
	batch := body.Statements != nil

	// A request names one statement, or a batch. Never both, and never
	// neither.
	if single == batch {
		return nil, errInvalidRequest
	}

	if single {
		if body.Statements != nil {
			return nil, errInvalidRequest
		}
		return s.checkStatements([]statement{{SQL: *body.SQL, Params: body.Params}})
	}

	if body.Params != nil {
		return nil, errInvalidRequest
	}
	if len(body.Statements) == 0 || len(body.Statements) > s.config.Limits.BatchStatements {
		return nil, errInvalidRequest
	}
	return s.checkStatements(body.Statements)
}

func (s *server) checkStatements(statements []statement) ([]statement, error) {
	for _, one := range statements {
		if strings.TrimSpace(one.SQL) == "" || len(one.SQL) > s.config.Limits.SQLBytes {
			return nil, errInvalidRequest
		}
	}
	return statements, nil
}

// bind puts the parameters of a request on a statement. A list binds by
// position, and an object binds by name.
func bind(stmt *sqlite.Stmt, params json.RawMessage) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}

	var list []any
	if err := json.Unmarshal(params, &list); err == nil {
		for index, value := range list {
			if err := bindOne(stmt, index+1, "", value); err != nil {
				return err
			}
		}
		return nil
	}

	var named map[string]any
	if err := json.Unmarshal(params, &named); err != nil {
		return errInvalidRequest
	}
	for name, value := range named {
		if err := bindOne(stmt, 0, ":"+name, value); err != nil {
			return err
		}
	}
	return nil
}

func bindOne(stmt *sqlite.Stmt, position int, name string, value any) error {
	set := func(text func(), named func()) {
		if name == "" {
			text()
			return
		}
		named()
	}

	switch value := value.(type) {
	case nil:
		set(func() { stmt.BindNull(position) }, func() { stmt.SetNull(name) })
	case bool:
		set(func() { stmt.BindBool(position, value) }, func() { stmt.SetBool(name, value) })
	case string:
		set(func() { stmt.BindText(position, value) }, func() { stmt.SetText(name, value) })
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return errInvalidRequest
		}
		if value == math.Trunc(value) && math.Abs(value) < (1<<53) {
			set(func() { stmt.BindInt64(position, int64(value)) }, func() { stmt.SetInt64(name, int64(value)) })
			return nil
		}
		set(func() { stmt.BindFloat(position, value) }, func() { stmt.SetFloat(name, value) })
	case map[string]any:
		// A blob travels as {"base64": "..."}.
		encoded, ok := value["base64"].(string)
		if !ok || len(value) != 1 {
			return errInvalidRequest
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return errInvalidRequest
		}
		set(func() { stmt.BindBytes(position, raw) }, func() { stmt.SetBytes(name, raw) })
	default:
		return errInvalidRequest
	}
	return nil
}

// readRow reads one row. A blob travels as {"base64": "..."}.
func readRow(stmt *sqlite.Stmt) ([]any, error) {
	row := make([]any, 0, stmt.ColumnCount())

	for index := 0; index < stmt.ColumnCount(); index++ {
		switch stmt.ColumnType(index) {
		case sqlite.TypeNull:
			row = append(row, nil)
		case sqlite.TypeInteger:
			row = append(row, stmt.ColumnInt64(index))
		case sqlite.TypeFloat:
			value := stmt.ColumnFloat(index)
			if math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, errQueryFailed
			}
			row = append(row, value)
		case sqlite.TypeText:
			row = append(row, stmt.ColumnText(index))
		case sqlite.TypeBlob:
			raw := make([]byte, stmt.ColumnLen(index))
			stmt.ColumnBytes(index, raw)
			row = append(row, map[string]any{"base64": base64.StdEncoding.EncodeToString(raw)})
		default:
			return nil, errQueryFailed
		}
	}
	return row, nil
}

// withinOutput refuses an answer over the output budget.
func (s *server) withinOutput(answer map[string]any) error {
	encoded, err := json.Marshal(answer)
	if err != nil {
		return errQueryFailed
	}
	if len(encoded) > s.config.Limits.OutputBytes {
		return errResultTooLarge
	}
	return nil
}

func size(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}
