// Command http-handler is a test fixture. It serves one Go http.Handler over
// ARC with httpadapter.New, and opens no port.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	httpadapter "github.com/gezibash/arc/adapters/http"
	"github.com/gezibash/arc/adapters/provider/stdio"
	"github.com/gezibash/arc/core/provider"
)

func main() {
	mux := http.NewServeMux()
	// The reply repeats what the handler saw, so a test can check each part.
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Location", "/echo/7")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"caller": r.Header.Get(httpadapter.CallerHeader),
			"query":  r.URL.RawQuery,
			"body":   string(body),
		})
	})
	if err := stdio.Run(context.Background(), httpadapter.New(mux), provider.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
