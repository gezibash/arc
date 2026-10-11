package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/spf13/cobra"
)

// pathFile holds the default path of the bulk bytes of a citizen: auto,
// direct, turn or session. See docs/direct/SPEC.md, section 3.7.
func pathFile(dir string) string { return filepath.Join(dir, "path") }

// pathCommand shows or sets the default path of this citizen.
func pathCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "path [auto|direct|turn|session]",
		Short: "Show or set the default path of large bodies",
		Long: "Show or set the default path of large bodies. auto tries a direct path,\n" +
			"then the TURN server of the relay. direct tries only a direct path. turn\n" +
			"uses only the TURN server, so the provider does not see your address.\n" +
			"session sends the bytes in session frames over the relay.\n" +
			"The flag --path of a command overrides it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, err := home(command)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				policy, err := citizenPath(dir)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(command.OutOrStdout(), policy)
				return err
			}
			policy, err := direct.ParsePolicy(args[0])
			if err != nil {
				return err
			}
			return os.WriteFile(pathFile(dir), []byte(string(policy)+"\n"), 0o600)
		},
	}
}

// citizenPath is the default path of the citizen in dir.
func citizenPath(dir string) (direct.Policy, error) {
	data, err := os.ReadFile(pathFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return direct.Auto, nil
	}
	if err != nil {
		return "", err
	}
	policy, err := direct.ParsePolicy(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("%s: %w", pathFile(dir), err)
	}
	return policy, nil
}

// dialConfig is the config of a pipe for a command: the flag --path, or
// the default of the citizen.
func dialConfig(command *cobra.Command) (direct.DialConfig, error) {
	dir, err := home(command)
	if err != nil {
		return direct.DialConfig{}, err
	}
	flag, _ := command.Flags().GetString("path")
	policy, err := direct.ParsePolicy(flag)
	if err == nil && flag == "" {
		policy, err = citizenPath(dir)
	}
	if err != nil {
		return direct.DialConfig{}, err
	}
	relays, err := readRelays(dir)
	if err != nil {
		return direct.DialConfig{}, err
	}
	return direct.DialConfig{
		Policy:  policy,
		Options: direct.EnvOptions(relays),
		Notes:   command.ErrOrStderr(),
		TURN: func(ctx context.Context) (direct.Credential, error) {
			if len(relays) == 0 {
				return direct.Credential{}, errors.New("arc relay turn: no relays")
			}
			body, err := fetchTURN(ctx, dir, relays[0])
			if err != nil {
				return direct.Credential{}, fmt.Errorf("arc relay turn: %w", err)
			}
			var credential direct.Credential
			err = json.Unmarshal(body, &credential)
			return credential, err
		},
	}, nil
}
