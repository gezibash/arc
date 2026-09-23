// Command origin is the HTTP server that the tests of http-provider run. It
// writes to standard output, and it listens late, as a real server can.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	fmt.Println("the origin writes this line to standard output")
	time.Sleep(300 * time.Millisecond)

	mux := http.NewServeMux()
	// /echo answers with what the request carried.
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
			"caller": r.Header.Get("Arc-Caller"), "body": string(body), "port": os.Getenv("PORT"),
		})
	})
	// /relay calls the address of its query through ARC_CALL_URL, and answers
	// with the status and the body of that endpoint. A token in the query
	// replaces ARC_CALL_TOKEN.
	mux.HandleFunc("/relay", func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("ARC_CALL_TOKEN")
		if given := r.URL.Query().Get("token"); given != "" {
			token = given
		}
		call, _ := json.Marshal(map[string]string{"address": r.URL.Query().Get("address"), "body": "q"})
		request, _ := http.NewRequest(http.MethodPost, os.Getenv("ARC_CALL_URL"), bytes.NewReader(call))
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer response.Body.Close()
		out, _ := io.ReadAll(response.Body)
		fmt.Fprintf(w, "%d %s", response.StatusCode, strings.TrimSpace(string(out)))
	})
	// /exit ends the server.
	mux.HandleFunc("/exit", func(http.ResponseWriter, *http.Request) { os.Exit(0) })

	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
