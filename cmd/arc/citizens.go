package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/core/keys"
	"github.com/spf13/cobra"
)

func citizensDir(root string) string { return filepath.Join(root, "citizens") }

func defaultPath(root string) string { return filepath.Join(root, "default") }

func publicPath(dir string) string { return filepath.Join(dir, "public") }

// chosen returns the directory of the identity that the command uses, and
// what chose it: --key, ARC_KEY, or the default.
func chosen(command *cobra.Command) (string, string, error) {
	root, why, err := rootSource(command)
	if err != nil {
		return "", "", err
	}
	name, source := "", ""
	if flag, _ := command.Flags().GetString("key"); flag != "" {
		name, source = flag, "--key"
	} else if env := os.Getenv("ARC_KEY"); env != "" {
		name, source = env, "ARC_KEY"
	} else if body, err := os.ReadFile(defaultPath(root)); err == nil {
		name, source = strings.TrimSpace(string(body)), "the default"
	} else if !errors.Is(err, os.ErrNotExist) {
		// A home that arc cannot read is not a home with no identity.
		return "", "", unreadableDefault(root, why, err)
	}
	if name == "" {
		return "", "", fmt.Errorf("no identity in %s, chosen by %s: make one with arc keys gen, or add one with arc keys add", root, why)
	}
	dir := filepath.Join(citizensDir(root), name)
	if !validName(name) {
		return "", "", fmt.Errorf("no identity \"%s\" on this machine: see arc keys list", name)
	}
	if _, err := os.Stat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", unreadableIdentities(root, why, err)
		}
		return "", "", fmt.Errorf("no identity \"%s\" in %s, chosen by %s: see arc keys list", name, root, why)
	}
	return dir, source, nil
}

// validName refuses a name that would leave the directory of identities.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

// localID is one identity of this machine.
type localID struct {
	name   string
	public string
}

// listCitizens reads the identities of this machine, by name.
func listCitizens(root string) ([]localID, error) {
	entries, err := os.ReadDir(citizensDir(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []localID
	for _, entry := range entries {
		if !entry.IsDir() || !validName(entry.Name()) {
			continue
		}
		public, _ := os.ReadFile(publicPath(filepath.Join(citizensDir(root), entry.Name())))
		out = append(out, localID{name: entry.Name(), public: strings.TrimSpace(string(public))})
	}
	return out, nil
}

func defaultName(root string) string {
	body, _ := os.ReadFile(defaultPath(root))
	return strings.TrimSpace(string(body))
}

// addCitizen keeps a key file as a new identity. It reads the key in a
// directory of its own first, so that a key that does not open, or a signer
// that does not answer, leaves nothing behind. The first identity becomes the
// default. A caller that made the key passes it as known, so a sealed key does
// not ask for its passphrase again.
func addCitizen(ctx context.Context, root, why, text string, known *keys.Key) (_ keys.Key, err error) {
	// A default that arc cannot read stops the add before it writes a key:
	// keys gen shows the seed only after the add.
	body, err := os.ReadFile(defaultPath(root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return keys.Key{}, unreadableDefault(root, why, err)
	}
	first := strings.TrimSpace(string(body)) == ""
	if err := os.MkdirAll(citizensDir(root), 0o700); err != nil {
		return keys.Key{}, unwritableIdentities(root, why, err)
	}
	staging, err := os.MkdirTemp(citizensDir(root), ".adding-")
	if err != nil {
		return keys.Key{}, unwritableIdentities(root, why, err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(staging)) }()

	if err := keyfile.Write(keyPath(staging), text); err != nil {
		return keys.Key{}, err
	}
	// An identity that signs through a bunker uses the device key of this
	// machine, so the key that the signer lists stays when an add fails.
	var device keys.Key
	if strings.HasPrefix(text, "bunker://") {
		if device, err = deviceKey(root); err != nil {
			return keys.Key{}, err
		}
		if err := keyfile.Save(filepath.Join(staging, "bunker-client"), device); err != nil {
			return keys.Key{}, err
		}
	}
	var k keys.Key
	if known != nil {
		k = *known
	} else {
		id, err := loadIdentity(ctx, staging)
		if err != nil {
			if strings.Contains(err.Error(), "not on the device list") {
				return keys.Key{}, fmt.Errorf("%w\non the signer, run: arc keys devices add %s --name <name>", err, device.Public.Hex())
			}
			return keys.Key{}, err
		}
		k = id.key
	}
	if err := os.WriteFile(publicPath(staging), []byte(k.Public.Hex()+"\n"), 0o600); err != nil {
		return keys.Key{}, err
	}
	name := k.Name()
	if err := os.Rename(staging, filepath.Join(citizensDir(root), name)); err != nil {
		if _, statErr := os.Stat(filepath.Join(citizensDir(root), name)); statErr == nil {
			return keys.Key{}, fmt.Errorf("%s is already on this machine", name)
		}
		return keys.Key{}, err
	}
	if first {
		if err := os.WriteFile(defaultPath(root), []byte(name+"\n"), 0o600); err != nil {
			return keys.Key{}, err
		}
	}
	return k, nil
}
