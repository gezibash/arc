// Package service is the HTTP service of the notes examples. It keeps each
// note in SQLite over ARC. Each caller sees only its own notes: the header
// field Arc-Caller names the caller, as docs/http/SPEC.md section 5 defines.
//
// The package uses no ARC library. Each example gives it a Database: one
// calls through provider.Caller, and one posts to ARC_CALL_URL.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Database sends one request to the sqlite capability, and returns its
// reply. See docs/sqlite/SPEC.md.
type Database func(ctx context.Context, body string) (string, error)

// maxNote caps the body of one note.
const maxNote = 64 * 1024

const schema = "CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, author TEXT NOT NULL, body TEXT NOT NULL)"

// Handler serves POST /notes, which keeps a note, and GET /notes, which
// lists the notes of the caller.
func Handler(db Database) http.Handler {
	s := notes{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /notes", s.list)
	mux.HandleFunc("POST /notes", s.add)
	return mux
}

type notes struct {
	db Database
}

type statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

type result struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// sql runs statements as one batch, in one transaction of the database.
func (n notes) sql(ctx context.Context, statements ...statement) ([]result, error) {
	body, err := json.Marshal(map[string]any{"statements": statements})
	if err != nil {
		return nil, err
	}
	reply, err := n.db(ctx, string(body))
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []result `json:"results"`
	}
	if err := json.Unmarshal([]byte(reply), &out); err != nil || len(out.Results) != len(statements) {
		return nil, fmt.Errorf("notes: the database sent %q", reply)
	}
	return out.Results, nil
}

// add keeps the body of the request as a note of the caller.
func (n notes) add(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxNote+1))
	if err != nil || len(body) == 0 || len(body) > maxNote {
		http.Error(w, fmt.Sprintf("a note has 1 to %d bytes", maxNote), http.StatusBadRequest)
		return
	}
	results, err := n.sql(r.Context(),
		statement{SQL: schema},
		statement{SQL: "INSERT INTO notes (author, body) VALUES (?, ?)", Params: []any{r.Header.Get("Arc-Caller"), string(body)}},
		statement{SQL: "SELECT last_insert_rowid() AS id"},
	)
	if err != nil {
		fail(w, err)
		return
	}
	id := results[2].Rows[0][0]
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", fmt.Sprintf("/notes/%v", id))
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": id})
}

// list answers the notes of the caller, the oldest first.
func (n notes) list(w http.ResponseWriter, r *http.Request) {
	results, err := n.sql(r.Context(),
		statement{SQL: schema},
		statement{SQL: "SELECT id, body FROM notes WHERE author = ? ORDER BY id", Params: []any{r.Header.Get("Arc-Caller")}},
	)
	if err != nil {
		fail(w, err)
		return
	}
	out := []map[string]any{}
	for _, row := range results[1].Rows {
		out = append(out, map[string]any{"id": row[0], "body": row[1]})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// fail answers 502: the database behind this service did not do the work.
// The detail goes to the log of the operator, not to the caller.
func fail(w http.ResponseWriter, err error) {
	fmt.Fprintf(os.Stderr, "notes: %v\n", err)
	http.Error(w, "the database did not answer", http.StatusBadGateway)
}
