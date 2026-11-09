package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// turnCommand asks a relay for a TURN credential with a NIP-98 request,
// and prints the JSON reply. The transfer app runs it when it finds no
// direct path.
func turnCommand() *cobra.Command {
	return &cobra.Command{
		Use: "turn [url]", Short: "Get a TURN credential from a relay (default: the first that also takes TCP, else the first that answers)", Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, err := home(command)
			if err != nil {
				return err
			}
			relays := args
			if len(relays) == 0 {
				if relays, err = readRelays(dir); err != nil {
					return err
				}
			}
			body, err := fetchAnyTURN(command.Context(), dir, relays)
			if err != nil {
				return err
			}
			// The relay chose the JSON; it stays valid JSON.
			_, err = io.WriteString(command.OutOrStdout(), terminal.EscapeJSON(string(body)))
			return err
		},
	}
}

// fetchAnyTURN asks each relay in turn for a TURN credential. It returns
// the first that also names TURN over TCP, so that a citizen with no UDP,
// such as in a sandbox that lets only port 443 out, can still relay; else
// the first reply. A relay that does not answer, or serves no TURN, does
// not stop the others.
func fetchAnyTURN(ctx context.Context, dir string, relays []string) ([]byte, error) {
	if len(relays) == 0 {
		return nil, errors.New("no relays: add one with arc relay add <url>")
	}
	var problems []error
	var first []byte
	for _, relay := range relays {
		body, err := fetchTURN(ctx, dir, relay)
		if err == nil {
			if overTCP(body) {
				return body, nil
			}
			if first == nil {
				first = body
			}
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		problems = append(problems, err)
	}
	if first != nil {
		return first, nil
	}
	// One line: arc prints an error on one line, so a newline of
	// errors.Join would show as \n.
	texts := make([]string, len(problems))
	for i, problem := range problems {
		texts[i] = problem.Error()
	}
	return nil, errors.New(strings.Join(texts, "; "))
}

// fetchTURN asks a relay for a TURN credential, signed with the key of the
// citizen in dir, and returns the JSON reply.
func fetchTURN(ctx context.Context, dir, relay string) ([]byte, error) {
	endpoint, err := turnURL(relay)
	if err != nil {
		return nil, err
	}
	id, err := loadIdentity(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer id.done()
	authorization, err := turn.AuthHeader(ctx, id.signer, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", authorization)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s serves no TURN", relay)
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s refused: %s %s", relay, response.Status, strings.TrimSpace(string(body)))
	}
	var credential turn.Credential
	if json.Unmarshal(body, &credential) != nil || credential.Username == "" || len(credential.URLs) == 0 {
		return nil, fmt.Errorf("%s serves no TURN: its reply is not a credential", relay)
	}
	return body, nil
}

// turnURL is the HTTP URL of the TURN credential of a relay URL.
// overTCP says whether a credential names TURN over TCP or TLS.
func overTCP(body []byte) bool {
	var credential turn.Credential
	if json.Unmarshal(body, &credential) != nil {
		return false
	}
	return slices.ContainsFunc(credential.URLs, func(u string) bool {
		return strings.HasPrefix(u, "turns:") || strings.Contains(u, "transport=tcp")
	})
}

func turnURL(relay string) (string, error) { return relayHTTP(relay, turn.Path) }

// relayHTTP is the HTTP URL of a path on the listener of a relay.
func relayHTTP(relay, path string) (string, error) {
	u, err := url.Parse(relay)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("\"%s\" is not a relay URL", relay)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", errors.New("a relay is a ws:// or wss:// URL")
	}
	u.Path, u.RawQuery, u.Fragment = path, "", ""
	return u.String(), nil
}
