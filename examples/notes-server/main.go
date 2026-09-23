// Command notes-server is the notes service as a plain HTTP server. It uses
// no ARC library, so a server in any language does the same work:
// http-provider starts it, and serves it over ARC as the capability http.
//
// The server reads four variables. http-provider sets the first three:
//
//	PORT            the port on 127.0.0.1 where the server listens
//	ARC_CALL_URL    where the server posts a call to a capability
//	ARC_CALL_TOKEN  the bearer token of each call
//	NOTES_DB        the database, for example sqlite+arc://<provider>/main
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gezibash/arc/examples/notes/service"
)

func main() {
	port, address := os.Getenv("PORT"), os.Getenv("NOTES_DB")
	if port == "" || address == "" {
		fmt.Fprintln(os.Stderr, "notes-server: http-provider sets PORT, and NOTES_DB must name a database")
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", port),
		Handler:           service.Handler(call(address)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// call sends each request of the database as one POST to ARC_CALL_URL. See
// docs/http/SPEC.md, section 10.2.
func call(address string) service.Database {
	return func(ctx context.Context, body string) (string, error) {
		payload, err := json.Marshal(map[string]string{"address": address, "body": body})
		if err != nil {
			return "", err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("ARC_CALL_URL"), bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		request.Header.Set("Authorization", "Bearer "+os.Getenv("ARC_CALL_TOKEN"))
		request.Header.Set("Content-Type", "application/json")

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		// 200 holds the reply. 502 holds the refusal of the database, and
		// every other status holds an error.
		var result struct {
			Reply   *string `json:"reply"`
			Refused string  `json:"refused"`
			Error   string  `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			return "", fmt.Errorf("the endpoint answered %d with no result: %w", response.StatusCode, err)
		}
		if response.StatusCode != http.StatusOK || result.Reply == nil {
			return "", fmt.Errorf("the endpoint answered %d: %s%s", response.StatusCode, result.Refused, result.Error)
		}
		return *result.Reply, nil
	}
}
