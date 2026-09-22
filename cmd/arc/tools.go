package main

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/bundle"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/iface"
	"github.com/spf13/cobra"
)

// version is the release of this build. A build sets it with
// -ldflags "-X main.version=0.9.0".
var version = "dev"

func toolCommand() *cobra.Command {
	command := &cobra.Command{Use: "tool", Short: "List, show and remove the capabilities that you installed"}

	command.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List the capabilities that you installed", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				installs, err := installsOf(command)
				if err != nil {
					return err
				}
				list, err := installs.List()
				if err != nil {
					return err
				}
				if len(list) == 0 {
					fmt.Println("no capabilities: install one with arc install <provider>")
					return nil
				}
				for _, e := range list {
					fmt.Printf("%s -> %s/%s\n  %s\n", runName(e), e.Name, e.ID, e.Provider)
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "info <name>", Short: "Show one installed capability", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				installs, err := installsOf(command)
				if err != nil {
					return err
				}
				install, err := installed(installs, args[0])
				if err != nil {
					return err
				}
				provider, err := nostr.PubKeyFromHex(install.Provider)
				if err != nil {
					return err
				}
				sess, err := open(command)
				if err != nil {
					return err
				}
				defer sess.close()

				offer, err := findOffer(command.Context(), sess, provider, install.ID)
				if err != nil {
					return err
				}
				fmt.Printf("%s -> %s/%s\n", runName(install), install.Name, install.ID)
				showOffer(offer)
				if install.Consent != nil && offer.Manifest != nil {
					if changes := install.Consent.Changes(offer.Manifest); len(changes) > 0 {
						fmt.Printf("the author changed what it can do; install it again to agree:\n  %s\n", strings.Join(changes, "\n  "))
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "remove <name>", Short: "Remove one installed capability", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				installs, err := installsOf(command)
				if err != nil {
					return err
				}
				gone, err := installs.Remove(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("removed %s/%s\n", gone.Name, gone.ID)
				return nil
			},
		},
	)
	return command
}

// runName is how the citizen runs an install.
func runName(e catalog.Install) string {
	if e.As != "" {
		return "arc " + e.As
	}
	return "arc call " + e.Name
}

// installed finds one install by the name that runs it, or by the petname of
// its provider.
func installed(installs catalog.Installs, name string) (catalog.Install, error) {
	list, err := installs.List()
	if err != nil {
		return catalog.Install{}, err
	}
	for _, e := range list {
		if e.As == name || (e.As == "" && e.Name == name) {
			return e, nil
		}
	}
	return catalog.Install{}, fmt.Errorf("nothing installed as %q: see arc tool list", name)
}

func showOffer(o catalog.Offer) {
	fmt.Printf("%s offers %s (%s)\n  %s\n  %s\n", o.Name(), o.Title, o.ID, o.Summary, o.Provider.Hex())
	if o.Manifest != nil {
		fmt.Print(iface.Describe(o.Manifest))
	}
}

func infoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info <provider> [capability]",
		Short: "Show the capabilities that a provider announces",
		Long:  "The provider is 64 characters of hex, or the name of an install.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			installs, err := installsOf(command)
			if err != nil {
				return err
			}
			provider, _, err := installs.Resolve(args[0])
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			if len(args) == 2 {
				offer, err := findOffer(command.Context(), sess, provider, args[1])
				if err != nil {
					return err
				}
				showOffer(offer)
				return nil
			}

			mine := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}
			reports, errs := sess.node.Pull(command.Context(), mine, sess.relays)
			var offers []catalog.Offer
			for _, event := range sess.node.Store.Query(mine) {
				if offer, err := catalog.Read(event); err == nil {
					offers = append(offers, offer)
				}
			}
			if len(offers) == 0 {
				if unreached := node.Unreached(reports, errs); unreached != nil {
					return fmt.Errorf("this machine holds no announcement from that provider, and %w", unreached)
				}
				return errors.New("that provider announces nothing")
			}
			for i, offer := range offers {
				if i > 0 {
					fmt.Println()
				}
				showOffer(offer)
			}
			return nil
		},
	}
}

func resolveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <name or key>",
		Short: "Find citizens by petname, or by the start of their key",
		Long: "arc looks in the identities of this machine, in its installs, and in\n" +
			"the announcements that its relays hold.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			query := strings.ToLower(strings.TrimSpace(args[0]))
			if len(query) < 4 {
				return errors.New("give at least 4 characters of a petname or a key")
			}

			type found struct{ key, name, where string }
			var hits []found
			seen := map[string]bool{}
			consider := func(key, where string) {
				pk, err := nostr.PubKeyFromHex(key)
				if err != nil || seen[key] {
					return
				}
				name := keys.Name(pk[:])
				if name == query || strings.HasPrefix(key, query) {
					seen[key] = true
					hits = append(hits, found{key, name, where})
				}
			}

			root, err := rootDir(command)
			if err != nil {
				return err
			}
			mine, err := listCitizens(root)
			if err != nil {
				return err
			}
			for _, c := range mine {
				consider(c.public, "an identity of this machine")
			}

			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			installs, err := installsOf(command)
			if err != nil {
				return err
			}
			list, err := installs.List()
			if err != nil {
				return err
			}
			for _, e := range list {
				consider(e.Provider, "installed as "+runName(e))
			}

			reports, errs := sess.node.Pull(command.Context(), announcements, sess.relays)
			for _, offer := range catalog.Search(sess.node.Store, "") {
				consider(offer.Provider.Hex(), "announces "+offer.ID)
			}

			if len(hits) == 0 {
				if unreached := node.Unreached(reports, errs); unreached != nil {
					return fmt.Errorf("no citizen answers to %s on this machine, and %w", args[0], unreached)
				}
				return fmt.Errorf("no citizen answers to %s", args[0])
			}
			for _, h := range hits {
				fmt.Printf("%s\n  %s\n  %s\n", h.name, h.key, h.where)
			}
			return nil
		},
	}
}

func appsCommand() *cobra.Command {
	command := &cobra.Command{Use: "apps", Short: "Work with provider bundles"}
	command.AddCommand(&cobra.Command{
		Use:   "init [directory]",
		Short: "Write a new provider bundle",
		Long: "The bundle holds an Arcfile, a manifest, and a runtime that\n" +
			"answers one message. Serve it with arc serve <directory>.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			files, err := bundle.Init(path)
			if err != nil {
				return err
			}
			fmt.Printf("wrote a bundle in %s\n", files.Root)
			fmt.Printf("  %s\n  %s\n  %s\n", files.Arcfile, files.Manifest, files.Runtime)
			fmt.Printf("\nserve it with: arc serve %s\n", path)
			return nil
		},
	})
	return command
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and the build commit",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("arc %s\n", version)
			fmt.Printf("commit %s\n", commit())
			fmt.Printf("built with %s for %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}

// commit reads the revision that the Go build wrote into the binary. A build
// outside a checkout writes none.
func commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "unknown", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if dirty && revision != "unknown" {
		return revision + " (modified)"
	}
	return revision
}
