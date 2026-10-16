package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gezibash/arc/adapters/relay/rings"
	"github.com/gezibash/arc/adapters/relay/turn"
	relaytransport "github.com/gezibash/arc/adapters/transport/relay"
	"github.com/spf13/cobra"
)

// ringCommand registers a ring URL with the relays of this citizen, as
// docs/rings/SPEC.md defines.
func ringCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "ring",
		Short: "Let the relays call a URL when mail waits unread",
		Long: "A relay that offers rings makes one HTTPS POST to the URL when mail\n" +
			"for this citizen waits unread: at once, then not again until the next\n" +
			"read, within the limits that you set. The call carries no mail, no\n" +
			"author and no count. Use it to start an agent that does not run all\n" +
			"the time. A registration lasts 30 days; set it again to renew it.",
	}

	set := &cobra.Command{
		Use: "set <https url>", Short: "Register the URL with each relay that offers rings", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			name, _ := command.Flags().GetString("key-env")
			key := os.Getenv(name)
			if key == "" {
				return fmt.Errorf("the environment variable %s is empty: it must hold the key that the URL asks for", name)
			}
			var request rings.Request
			request.URL, request.Key = args[0], key
			request.MinInterval, _ = command.Flags().GetInt("min-interval")
			request.Burst, _ = command.Flags().GetInt("burst")
			request.DailyCap, _ = command.Flags().GetInt("daily-cap")
			body, err := json.Marshal(request)
			if err != nil {
				return err
			}
			return eachRingRelay(command, func(ctx context.Context, relay string, id signerIdentity) (string, error) {
				var status rings.Status
				if err := ringCall(ctx, id, http.MethodPut, relay, rings.Path, body, &status); err != nil {
					return "", err
				}
				return "rings " + describeRing(status), nil
			})
		},
	}
	set.Flags().String("key-env", "", "the environment variable that holds the key that the URL asks for, as a Bearer token (required)")
	set.Flags().Int("min-interval", 0, "the seconds in which one ring comes back after it is used, 60 to 86400 (default 300)")
	set.Flags().Int("burst", 0, "the rings that can go one after the other after a quiet time, 1 to 10 (default 3)")
	set.Flags().Int("daily-cap", 0, "the most rings in one UTC day (default 48, or the cap of the relay)")
	_ = set.MarkFlagRequired("key-env")

	show := &cobra.Command{
		Use: "show", Short: "Show the registration on each relay", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return eachRingRelay(command, func(ctx context.Context, relay string, id signerIdentity) (string, error) {
				var status rings.Status
				if err := ringCall(ctx, id, http.MethodGet, relay, rings.Path, nil, &status); err != nil {
					return "", err
				}
				line := "rings " + describeRing(status)
				if status.LastRing != 0 {
					line += fmt.Sprintf("; last ring %s, %s", time.Unix(status.LastRing, 0).Format(time.DateTime), describeAnswer(status.LastStatus))
				}
				return line, nil
			})
		},
	}

	test := &cobra.Command{
		Use: "test", Short: "Ask each relay for one ring now; it counts against the cap of the day", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return eachRingRelay(command, func(ctx context.Context, relay string, id signerIdentity) (string, error) {
				var answer rings.TestAnswer
				if err := ringCall(ctx, id, http.MethodPost, relay, rings.TestPath, nil, &answer); err != nil {
					return "", err
				}
				if answer.Status == 0 {
					return "", fmt.Errorf("the URL gave no answer: %s", answer.Error)
				}
				if answer.Status < 200 || answer.Status > 299 {
					return "", fmt.Errorf("the URL %s", describeAnswer(answer.Status))
				}
				return "the URL " + describeAnswer(answer.Status), nil
			})
		},
	}

	clear := &cobra.Command{
		Use: "clear", Short: "Remove the registration from each relay", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return eachRingRelay(command, func(ctx context.Context, relay string, id signerIdentity) (string, error) {
				if err := ringCall(ctx, id, http.MethodDelete, relay, rings.Path, nil, nil); err != nil {
					return "", err
				}
				return "rings no more", nil
			})
		},
	}

	command.AddCommand(set, show, test, clear)
	return command
}

// eachRingRelay runs do on each relay of the citizen that offers rings, and
// prints one line for each relay. A relay without rings is skipped.
func eachRingRelay(command *cobra.Command, do func(ctx context.Context, relay string, id signerIdentity) (string, error)) error {
	ctx := command.Context()
	dir, err := home(command)
	if err != nil {
		return err
	}
	relays, err := readRelays(dir)
	if err != nil {
		return err
	}
	if len(relays) == 0 {
		return errors.New("no relays: add one with arc relay add <url>")
	}
	id, err := loadIdentity(ctx, dir)
	if err != nil {
		return err
	}
	out := command.OutOrStdout()
	offered, failed := 0, 0
	for _, relay := range relays {
		tags, err := relaytransport.Relay{URL: relay}.Tags(ctx)
		if err != nil {
			fmt.Fprintf(out, "%s: %v\n", relay, err)
			failed++
			continue
		}
		if !slices.Contains(tags, rings.Tag) {
			fmt.Fprintf(out, "%s: offers no rings, skipped\n", relay)
			continue
		}
		offered++
		line, err := do(ctx, relay, id)
		if err != nil {
			fmt.Fprintf(out, "%s: %v\n", relay, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "%s: %s\n", relay, line)
	}
	switch {
	case offered == 0 && failed == 0:
		return errors.New("no relay of this citizen offers rings")
	case failed > 0:
		return fmt.Errorf("%d of %d relays failed", failed, len(relays))
	}
	return nil
}

// ringCall sends one signed request to the rings of a relay, and reads the
// JSON answer into result.
func ringCall(ctx context.Context, id signerIdentity, method, relay, path string, body []byte, result any) error {
	endpoint, err := relayHTTP(relay, path)
	if err != nil {
		return err
	}
	authorization, err := turn.AuthHeaderBody(ctx, id.signer, method, endpoint, body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", authorization)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// A test ring waits for the URL, for at most 10 seconds.
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &refusal) == nil && refusal.Error != "" {
			return fmt.Errorf("refused (%d): %s", response.StatusCode, refusal.Error)
		}
		return fmt.Errorf("refused: %s %s", response.Status, strings.TrimSpace(string(data)))
	}
	if result == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, result)
}

// describeRing says where and how often a relay rings, and until when. It
// names only the host of the URL, because a path can hold a secret.
func describeRing(s rings.Status) string {
	host := s.URL
	if u, err := url.Parse(s.URL); err == nil {
		host = u.Host
	}
	line := fmt.Sprintf("%s, %d at once then one each %d s, %d of %d left today", host, s.Burst, s.MinInterval, s.Left, s.DailyCap)
	// A relay of v0.33.0 sends no expiry.
	if s.Expires != 0 {
		line += ", until " + time.Unix(s.Expires, 0).Format(time.DateOnly)
	}
	return line
}

func describeAnswer(status int) string {
	if status == 0 {
		return "no answer"
	}
	return fmt.Sprintf("answered %d %s", status, http.StatusText(status))
}
