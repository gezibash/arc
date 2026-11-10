package host

import (
	"context"
	"fmt"

	"github.com/gezibash/arc/runtime/bundle"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
)

// allowList resolves allow of the Arcfile to public keys in hex. An entry
// takes any form of a key that a command takes. Nil allows every caller. An
// entry that does not resolve stops the start, so the list fails closed.
func allowList(ctx context.Context, installs catalog.Installs, entries []string) (map[string]bool, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	env := &citizen.Environment{Installs: installs}
	allowed := map[string]bool{}
	for _, entry := range entries {
		pk, err := env.ResolveKey(ctx, entry)
		if err != nil {
			return nil, fmt.Errorf("allow in the Arcfile: %q is not a key: %w", entry, err)
		}
		allowed[pk.Hex()] = true
	}
	return allowed, nil
}

// useList resolves [uses] of the Arcfile to the installed apps. It returns
// the apps as "<id> <key>", and the environment that names each address.
// Nil allows calls to every installed app.
func useList(installs catalog.Installs, uses []bundle.Use) (map[string]string, []string, error) {
	if uses == nil {
		return nil, nil, nil
	}
	apps := map[string]string{}
	var env []string
	for _, use := range uses {
		app, ok, err := installs.Named(use.Installed)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("[uses] in the Arcfile: %s names %q, which this citizen has not installed: run arc install <key> --as %s", use.Name, use.Installed, use.Installed)
		}
		apps[app.ID+" "+app.Provider] = use.Name
		env = append(env, use.Env()+"="+app.ID+"+arc://"+app.Provider+"/")
	}
	return apps, env, nil
}

// inUses refuses a call of the program to an app that [uses] does not name.
func inUses(ctx context.Context, installs catalog.Installs, uses map[string]string, address string) error {
	if uses == nil {
		return nil
	}
	parsed, err := catalog.ParseAddress(address)
	if err != nil {
		return err
	}
	pk, _, err := installs.Resolve(ctx, parsed.Provider)
	if err != nil {
		return err
	}
	if _, ok := uses[parsed.Scheme+" "+pk.Hex()]; !ok {
		return fmt.Errorf("not_in_uses: %s is not in [uses] of the Arcfile", address)
	}
	return nil
}
