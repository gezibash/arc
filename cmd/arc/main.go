// Command arc is the command line tool of ARC, on Nostr events. See
// docs/delivery/SPEC.md.
//
//	arc keys gen | recover | add | list | use | remove | encrypt | bunker
//	arc whoami
//	arc relay add <url> | rm <url> | ls | turn | serve
//	arc message send | inbox | outbox | watch
//	arc serve | announce | discover | install | call | session
//	arc sync [--dir <path>]
//	arc apps list | info | remove
//	arc info | resolve | apps init | version
//	arc lists add | rm | ls
//	arc update [check | apply]
//	arc release sign
//	arc <app> <command...>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/relaylog"
	"github.com/gezibash/arc/adapters/relay/rings"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/relay/stun"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		// An app set the exit status from its reply, and wrote its
		// output already.
		if status, ok := errors.AsType[iface.ExitError](err); ok {
			os.Exit(status.Code)
		}
		fmt.Fprintln(os.Stderr, errorLine(err))
		os.Exit(1)
	}
}

// errorLine is the line that arc prints for an error. An error can carry
// text that another party chose, such as the refusal of a provider or of a
// relay, so the line is escaped and stays one line, as docs/clients/SPEC.md,
// section 3.9, says: a refusal cannot forge a second line.
func errorLine(err error) string {
	return "arc: " + terminal.EscapeLine(citizen.SignerOnly(err).Error())
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:   "arc",
		Short: "ARC on signed events, over any transport",
		Long: "ARC on signed events, over any transport.\n\n" +
			"An installed app adds its own commands: arc <name> <command>.\n" +
			"arc help <name> lists them.",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE:               dispatch,
		SilenceUsage:       true,
		SilenceErrors:      true,
	}
	command.PersistentFlags().String("home", "", "the directory of arc; arc takes --home, then ARC_HOME, then ~/.arc, then ~/.config/arc when an older arc made it; a ~/.arc with no identity gives way to a ~/.config/arc with one")
	command.PersistentFlags().String("key", "", "the identity to use, by petname (ARC_KEY)")
	command.AddCommand(keysCommand(), whoamiCommand(), relayCommand(), messageCommand(), contactsCommand(),
		serveCmd(), announceCmd(), discoverCmd(), installCmd(), callCmd(), sessionCmd(), syncCommand(),
		appsCommand(), infoCommand(), resolveCommand(), versionCommand(), listsCommand(), updateCommand(), releaseCommand(), pathCommand())
	command.SetHelpCommand(helpCommand(command))
	command.Version = version
	return command
}

// rootDir is the directory of arc. It holds one directory for each identity,
// and the files of a relay that this machine runs.
func rootDir(command *cobra.Command) (string, error) {
	dir, _, err := rootSource(command)
	return dir, err
}

// rootSource is rootDir, and the rule that chose it: --home, ARC_HOME,
// ~/.arc, or the home of an older arc.
func rootSource(command *cobra.Command) (string, string, error) {
	user, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	for _, given := range []struct{ dir, why string }{
		{flagString(command, "home"), "--home"},
		{os.Getenv("ARC_HOME"), "ARC_HOME"},
	} {
		if given.dir != "" {
			dir, err := absHome(user, given.dir)
			return dir, given.why, err
		}
	}
	dir, err := defaultRoot(user)
	if err != nil {
		return "", "", err
	}
	old := filepath.Join(user, ".config", "arc")
	if warning := twoHomes(user, dir); warning != "" {
		if _, warned := homeWarned.LoadOrStore(command.Root(), true); !warned {
			// A path can hold any character, such as a newline in HOME.
			fmt.Fprintln(command.ErrOrStderr(), "arc: warning: "+terminal.EscapeLine(warning))
		}
	}
	if dir == old {
		return dir, olderHome, nil
	}
	return dir, "~/.arc", nil
}

func flagString(command *cobra.Command, name string) string {
	value, _ := command.Flags().GetString(name)
	return value
}

// absHome is a home that --home or ARC_HOME gives, as a clean absolute
// path. A leading ~ is the user's home, as a shell expands it: bash does
// not expand it after =, as in --home=~/arc. A symlink keeps its own name.
func absHome(user, dir string) (string, error) {
	if dir == "~" {
		dir = user
	} else if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(user, rest)
	} else if strings.HasPrefix(dir, "~") {
		// A shell expands ~root to the home of a user, and ~+ to the working
		// directory, so a folder of that name here would be one rm -rf away
		// from the folder that it names there (#411).
		return "", fmt.Errorf("the home \"%s\" starts with a ~ that a shell expands to another folder: arc expands only ~ and ~/; give the whole path", dir)
	}
	return filepath.Abs(dir)
}

// twoHomes is the warning when both default homes exist, which says the
// home that arc uses and why, or "" when one home exists. arc writes
// nothing to the other home.
func twoHomes(user, dir string) string {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	switch {
	case dir == home && holdsIdentity(home) && holdsIdentity(old):
		return fmt.Sprintf("both %s and %s hold an identity. arc uses %s, because it holds an identity, and an older arc or arc-dm uses %s. Upgrade each arc and arc-dm on this machine, or set ARC_HOME.", home, old, home, old)
	case dir == old && holdsIdentity(old):
		// Anything at ~/.arc: entries of arc, or a link that leads to no
		// home, as a dangling link or a link to ".".
		if _, err := os.Lstat(home); err == nil {
			// A link that leads to no home is not a second home.
			if holds, _ := holdsHome(home, "~/.arc"); !holds {
				return fmt.Sprintf("%s is a link that leads to no home of arc. arc uses %s, and reads and writes nothing through %s. Remove the link, or point it at a home, or set ARC_HOME.", home, old, home)
			}
			return fmt.Sprintf("both %s and %s exist. arc uses %s, because %s holds no identity, and reads and writes nothing in %s. Set ARC_HOME to choose the home.", home, old, old, home, home)
		}
	}
	return ""
}

// homeWarned holds each run of arc, by its root command, that warned of
// two homes already. arc finds its home more than once in one command.
var homeWarned sync.Map

// homeNames are the entries that arc keeps at the top of its home. An app
// that keeps data in ~/.arc/<app> never uses one of these names, and
// arc install refuses them.
var homeNames = []string{"citizens", "default", "device", "relay.db", "wake", "wake.toml"}

// defaultRoot is ~/.arc. A home that an older arc made in ~/.config/arc
// stays in use, until ~/.arc holds an entry of arc. arc moves no file, and
// it never looks for a home in the working directory: a cloned repository
// can hold a .arc with the key of someone else. A home that arc cannot
// read stops arc: arc never uses the next home in its place.
func defaultRoot(user string) (string, error) {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	holds, err := holdsHome(home, "~/.arc")
	if err != nil {
		return home, err
	}
	if holds {
		// A ~/.arc with entries but no identity, such as a lone wake.toml,
		// does not hide the identity of an older home (#408). An older home
		// that arc cannot read stops arc: else keys gen would make a second
		// identity in ~/.arc, which then hides the first.
		if mine, err := identityIn(home, "~/.arc"); err != nil || mine {
			return home, err
		}
		older, err := identityIn(old, olderHome)
		if err != nil {
			return old, err
		}
		if older {
			return old, nil
		}
		return home, nil
	}
	if holds, err = holdsHome(old, olderHome); err != nil || holds {
		return old, err
	}
	return home, nil
}

// olderHome is the rule that chose ~/.config/arc.
const olderHome = "~/.config/arc, the home of an older arc"

// holdsHome says whether dir holds an entry of arc. Only an entry that is
// not there is missing: a folder that arc cannot read is an error. why is
// the rule that would choose dir.
func holdsHome(dir, why string) (bool, error) {
	for _, name := range homeNames {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, unreadableHome(dir, why, err)
		}
	}
	return false, nil
}

// The three errors of a home that arc cannot read. Each case has one
// wording, which names the folder and the rule that chose it, and says
// what fixes it (#408).

func unreadableHome(dir, why string, err error) error {
	fix := "Fix its permissions"
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		fix = "It is not a folder: move it away"
	case errors.Is(err, syscall.ELOOP):
		fix = "It is a loop of symbolic links: remove the link"
	}
	return fmt.Errorf("cannot read the home %s, chosen by %s, and arc uses no other home in its place: %w. %s, or give --home or ARC_HOME", dir, why, err, fix)
}

// unreadableDefault is the error of a default file that arc cannot read,
// or of its home, when the home is what arc cannot read.
func unreadableDefault(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return unreadableHome(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the default identity in %s, chosen by %s: %w. Fix the permissions of %s, or give --key or ARC_KEY", root, why, err, defaultPath(root))
}

func unreadableIdentities(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return unreadableHome(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the identities in %s, chosen by %s: %w. Fix the permissions of that folder", citizensDir(root), why, err)
}

// identityIn says whether dir holds an identity of arc. Only a folder of
// identities that is not there is missing: one that arc cannot read is an
// error. why is the rule that would choose dir.
func identityIn(dir, why string) (bool, error) {
	entries, err := os.ReadDir(citizensDir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, unreadableIdentities(dir, why, err)
	}
	return len(entries) > 0, nil
}

// unwritableIdentities is the error of a folder of identities that arc
// can read but cannot add to.
func unwritableIdentities(root, why string, err error) error {
	return fmt.Errorf("cannot add an identity to %s, chosen by %s: %w. Fix the permissions of that folder", citizensDir(root), why, err)
}

// holdsIdentity says whether dir holds an identity of arc.
func holdsIdentity(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "citizens"))
	return err == nil && len(entries) > 0
}

// home is the directory of the identity that the command uses. It holds the
// key, the relay list, the installs and the store.
func home(command *cobra.Command) (string, error) {
	dir, _, err := chosen(command)
	return dir, err
}

func keyPath(dir string) string      { return filepath.Join(dir, "key") }
func relaysPath(dir string) string   { return filepath.Join(dir, "relays") }
func indexersPath(dir string) string { return filepath.Join(dir, "indexers") }

// readRelays returns the relays that arc uses. A list that an older arc
// wrote can hold a line that is not a relay URL, such as the second half
// of a URL that held a newline (#441). arc skips such a line; arc relay ls
// shows it, and arc relay rm removes it.
func readRelays(dir string) ([]string, error) { return relayURLs(readURLs(relaysPath(dir))) }

func writeRelays(dir string, urls []string) error { return writeURLs(dir, relaysPath(dir), urls) }

// An indexer relay holds only relay lists. A citizen publishes its NIP-65
// and NIP-17 lists there, and looks up the lists of others there.
func readIndexers(dir string) ([]string, error) { return relayURLs(readURLs(indexersPath(dir))) }

// isRelayURL says whether a line of a relay list is a ws:// or wss:// URL.
func isRelayURL(line string) bool {
	return strings.HasPrefix(line, "ws://") || strings.HasPrefix(line, "wss://")
}

// relayURLs keeps the lines that are relay URLs.
func relayURLs(lines []string, err error) ([]string, error) {
	return slices.DeleteFunc(lines, func(line string) bool { return !isRelayURL(line) }), err
}

// checkRelayURL refuses a relay URL that is not ws:// or wss://, or that
// holds a space, a control character or a format character. The list keeps
// one relay on each line: a newline would add a second relay. A control
// character would reach the terminal in each line that names the relay,
// and a bidi or invisible character could make it look like another relay
// (#441).
func checkRelayURL(url string) error {
	if !isRelayURL(url) {
		return errors.New("a relay is a ws:// or wss:// URL")
	}
	if strings.ContainsFunc(url, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) }) {
		return errors.New("a relay URL holds no space, no control character and no format character")
	}
	return nil
}

func writeIndexers(dir string, urls []string) error {
	return writeURLs(dir, indexersPath(dir), urls)
}

func readURLs(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func writeURLs(dir, path string, urls []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := strings.Join(urls, "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func relayCommand() *cobra.Command {
	command := &cobra.Command{Use: "relay", Short: "Manage the relays of this citizen, or run one"}

	add := &cobra.Command{
		Use: "add <url>", Short: "Send to and sync with a relay", Args: cobra.ExactArgs(1),
		Long: "With --index, the relay is an indexer: arc publishes only its relay\n" +
			"lists there, and looks up the relay lists of other citizens there.",
		RunE: func(command *cobra.Command, args []string) error {
			url := args[0]
			if err := checkRelayURL(url); err != nil {
				return err
			}
			dir, err := home(command)
			if err != nil {
				return err
			}
			// add, rm and ls read each line, so that a write keeps the
			// lines that arc skips.
			relays, err := readURLs(relaysPath(dir))
			if err != nil {
				return err
			}
			indexers, err := readURLs(indexersPath(dir))
			if err != nil {
				return err
			}
			index, _ := command.Flags().GetBool("index")
			if index {
				if slices.Contains(relays, url) {
					return fmt.Errorf("%s is a relay of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(indexers, url) {
					indexers = append(indexers, url)
				}
				if err := writeIndexers(dir, indexers); err != nil {
					return err
				}
			} else {
				if slices.Contains(indexers, url) {
					return fmt.Errorf("%s is an indexer of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(relays, url) {
					relays = append(relays, url)
				}
				if err := writeRelays(dir, relays); err != nil {
					return err
				}
			}
			announceRelays(command)
			return nil
		},
	}
	add.Flags().Bool("index", false, "use the relay only to publish and look up relay lists")

	command.AddCommand(
		add,
		&cobra.Command{
			Use: "rm <url>", Short: "Stop using a relay", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				relays, err := readURLs(relaysPath(dir))
				if err != nil {
					return err
				}
				indexers, err := readURLs(indexersPath(dir))
				if err != nil {
					return err
				}
				other := func(u string) bool { return u == args[0] }
				if err := writeRelays(dir, slices.DeleteFunc(relays, other)); err != nil {
					return err
				}
				if err := writeIndexers(dir, slices.DeleteFunc(indexers, other)); err != nil {
					return err
				}
				announceRelays(command)
				return nil
			},
		},
		&cobra.Command{
			Use: "ls", Short: "List the relays", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				urls, err := readURLs(relaysPath(dir))
				if err != nil {
					return err
				}
				indexers, err := readURLs(indexersPath(dir))
				if err != nil {
					return err
				}
				if len(urls) == 0 {
					fmt.Fprintln(command.OutOrStdout(), "no relays: add one with arc relay add <url>")
				}
				// A list of an older arc can hold any character, and a
				// line that is not a relay URL, which arc skips.
				skipped := false
				show := func(url, suffix string) {
					if !isRelayURL(url) {
						fmt.Fprintf(command.OutOrStdout(), "not a relay URL, skipped: \"%s\"%s\n", terminal.EscapeLine(url), suffix)
						skipped = true
						return
					}
					fmt.Fprintln(command.OutOrStdout(), terminal.EscapeLine(url)+suffix)
				}
				for _, url := range urls {
					show(url, "")
				}
				for _, url := range indexers {
					show(url, "  (index)")
				}
				if skipped {
					fmt.Fprintln(command.ErrOrStderr(), "arc: the relay list holds a line that is not a relay URL, which arc skips; remove it with arc relay rm and the line as the list holds it, quoted for the shell")
				}
				return nil
			},
		},
		turnCommand(),
		ringCommand(),
		serveCommand(),
	)
	return command
}

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

// announceRelays publishes the relay list of this citizen, when it has a key
// and at least one relay.
func announceRelays(command *cobra.Command) {
	sess, err := open(command)
	if err != nil {
		return
	}
	defer sess.Close()
	if len(sess.Relays) > 0 {
		publishRelayList(command.Context(), sess)
	}
}

// newRelay makes the relay of arc relay serve, with its store in the file at
// path. The relay logs its connections to logger. If ring is not nil, the
// relay offers rings. Call the returned function to close the store.
func newRelay(path string, p limits.Policy, ring *rings.Config, logger *slog.Logger) (*khatru.Relay, *relaylog.Log, *rings.Rings, func(), error) {
	db := &boltdb.BoltBackend{Path: path}
	if err := db.Init(); err != nil {
		return nil, nil, nil, nil, err
	}
	connections := relaylog.New(logger)
	p.Refused = connections.Refused
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	rl.Negentropy = true
	// khatru copies Info for each NIP-11 request and appends to the copy;
	// with no spare room, each append copies, and requests do not race (#425).
	rl.Info.SupportedNIPs = slices.Clip(append(rl.Info.SupportedNIPs, 77))
	readers := sealed.Protect(rl)
	limits.Apply(rl, db.DB, p)
	connections.Watch(rl)
	var ringer *rings.Rings
	if ring != nil {
		config := *ring
		config.Readers = readers
		config.Logger = logger
		var err error
		if ringer, err = rings.Apply(rl, db.DB, config); err != nil {
			db.Close()
			return nil, nil, nil, nil, err
		}
	}
	return rl, connections, ringer, db.Close, nil
}

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "serve", Short: "Run a relay on this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			listen, _ := command.Flags().GetString("listen")
			path, _ := command.Flags().GetString("db")
			if path == "" {
				dir, err := rootDir(command)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				path = filepath.Join(dir, "relay.db")
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			ring, err := ringSetting(command)
			if err != nil {
				return err
			}
			rl, connections, ringer, closeDB, err := newRelay(path, writePolicy(command), ring, logger)
			if err != nil {
				return err
			}
			defer closeDB()

			listener, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			listener = connections.Listen(listener)
			fmt.Fprintf(command.OutOrStdout(), "relay listens on ws://%s\n", listener.Addr())
			turnConfig, err := turnSetting(command)
			if err != nil {
				return err
			}
			var turnServer *turn.Server
			if stunAddress := stunListen(command, listen); stunAddress != "" {
				// A second relay on one machine cannot take the default
				// port. It runs with no STUN server. A port that the
				// operator sets must open.
				conn, err := net.ListenPacket("udp", stunAddress)
				switch {
				case err != nil && (command.Flags().Changed("stun") || turnConfig != nil):
					return fmt.Errorf("STUN: %w", err)
				case err != nil:
					fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("relay answers no STUN: %v (set --stun)", err)))
				case turnConfig != nil:
					// The TURN server answers STUN binding requests too.
					host, _, _ := net.SplitHostPort(stunAddress)
					turnConfig.RelayHost = host
					turnConfig.Logger = logger
					if address, _ := command.Flags().GetString("turn-tcp"); address != "" {
						if turnConfig.TCP, err = net.Listen("tcp", address); err != nil {
							_ = conn.Close()
							return fmt.Errorf("TURN over TCP: %w", err)
						}
					}
					if turnServer, err = turn.Serve(conn, *turnConfig); err != nil {
						_ = conn.Close()
						if turnConfig.TCP != nil {
							_ = turnConfig.TCP.Close()
						}
						return fmt.Errorf("TURN: %w", err)
					}
					defer func() { _ = turnServer.Close() }()
					fmt.Fprintf(command.OutOrStdout(), "relay answers STUN and TURN on udp://%s, relay ports %d-%d on %s\n", conn.LocalAddr(), turnConfig.MinPort, turnConfig.MaxPort, turnConfig.PublicIP)
					if turnConfig.TCP != nil {
						fmt.Fprintf(command.OutOrStdout(), "relay answers TURN on tcp://%s\n", turnConfig.TCP.Addr())
					}
				default:
					defer func() { _ = conn.Close() }()
					go func() {
						if err := stun.Serve(conn); err != nil {
							fmt.Fprintln(os.Stderr, "the STUN server stopped:", terminal.EscapeLine(err.Error()))
						}
					}()
					fmt.Fprintf(command.OutOrStdout(), "relay answers STUN on udp://%s\n", conn.LocalAddr())
				}
			} else if turnConfig != nil {
				return errors.New("TURN needs the STUN port; do not set --stun none with --turn-ports")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if ringer != nil {
				go ringer.Run(ctx)
				fmt.Fprintf(command.OutOrStdout(), "relay rings citizens, at most %d rings each minute\n", ring.PerMinute)
			}
			server := &http.Server{Handler: relayHandler(rl, turnServer, ringer), ConnContext: connections.ConnContext}
			go func() { <-ctx.Done(); _ = server.Close() }()
			go connections.Run(ctx, time.Minute)
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	command.Flags().String("listen", "127.0.0.1:7447", "the address to listen on")
	command.Flags().String("db", "", "the file that holds the events (default <home>/relay.db)")
	command.Flags().Int("max-event-bytes", 0, "refuse an event larger than this, as JSON (0: no cap on ephemeral events; a kept event has at most the protocol limit of 65,536 bytes, and a gift wrap the mail limit)")
	command.Flags().Bool("wrap-auth", false, "take a gift wrap only after NIP-42 authentication, or with proof of work")
	command.Flags().Int("wrap-pow", 0, "the NIP-13 difficulty that lets a gift wrap in without authentication (0: none)")
	command.Flags().Int("rate", 0, "the events that one IP address can write each minute (0: no limit)")
	command.Flags().Int("burst", 0, "the events that one IP address can write at once (default the rate)")
	command.Flags().String("ip-header", "", "the HTTP header that holds the client IP address, when a proxy sets it")
	command.Flags().Int64("max-store-mb", 0, "refuse new stored events when the store uses this many MiB (0: no cap)")
	command.Flags().String("stun", "", "the UDP address that answers STUN, or none (default: the host of --listen, port 3478)")
	command.Flags().String("turn-ports", "", "serve TURN on the STUN port, with relay sockets on this UDP port range, such as 49152-49251 (needs TURN_SECRET)")
	command.Flags().String("turn-ip", "", "the public IP address of the TURN relay sockets (default: the host of --listen)")
	command.Flags().Int64("turn-quota", 1024, "the MiB that one citizen can relay through TURN in 24 hours (0: refuse every allocation)")
	command.Flags().String("turn-tcp", "", "also serve TURN over TCP on this address, such as 127.0.0.1:3479 behind a TLS proxy on port 443 (needs --turn-ports)")
	command.Flags().StringArray("turn-url", nil, "another address that TURN credentials name, such as turns:relay.example:443?transport=tcp (repeatable; needs --turn-ports)")
	command.Flags().Bool("rings", false, "ring a registered citizen at its URL when mail waits unread (needs RINGS_SECRET)")
	command.Flags().Int("rings-daily-cap", rings.DefaultDailyCap, "the most rings that a citizen can ask for in one UTC day")
	command.Flags().Int("rings-per-minute", rings.DefaultPerMinute, "the most rings of the whole relay in one minute")
	command.Flags().IntSlice("rings-ports", []int{443}, "the ports that a ring URL can name")
	return command
}

// ringSetting reads the ring flags of a relay. It returns nil if the relay
// offers no rings. The secret that encrypts the ring keys comes from the
// environment variable RINGS_SECRET, so it is not in the process list.
func ringSetting(command *cobra.Command) (*rings.Config, error) {
	flags := command.Flags()
	if on, _ := flags.GetBool("rings"); !on {
		return nil, nil
	}
	secret := os.Getenv("RINGS_SECRET")
	if secret == "" {
		return nil, errors.New("--rings needs the environment variable RINGS_SECRET")
	}
	var config rings.Config
	config.Secret = secret
	config.DailyCap, _ = flags.GetInt("rings-daily-cap")
	config.PerMinute, _ = flags.GetInt("rings-per-minute")
	config.Ports, _ = flags.GetIntSlice("rings-ports")
	if config.DailyCap < 1 || config.PerMinute < 1 {
		return nil, errors.New("--rings-daily-cap and --rings-per-minute must be at least 1")
	}
	if address, _ := flags.GetString("turn-ip"); address != "" {
		if ip, err := netip.ParseAddr(address); err == nil {
			config.Own = append(config.Own, ip)
		}
	}
	return &config, nil
}

// relayHandler serves the relay, and the HTTP paths of TURN and rings when
// the relay offers them. Every other path is the relay's: khatru answers
// 404 on a path that it does not know.
func relayHandler(rl *khatru.Relay, turnServer *turn.Server, ringer *rings.Rings) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", rl)
	if turnServer != nil {
		mux.Handle(turn.Path, turnServer)
	}
	if ringer != nil {
		mux.Handle(rings.Path, ringer)
		mux.Handle(rings.TestPath, ringer)
	}
	return mux
}

// turnSetting reads the TURN flags of a relay. It returns nil if the relay
// serves no TURN. The secret comes from the environment variable
// TURN_SECRET, so it is not in the process list.
func turnSetting(command *cobra.Command) (*turn.Config, error) {
	flags := command.Flags()
	ports, _ := flags.GetString("turn-ports")
	if ports == "" {
		if flags.Changed("turn-tcp") || flags.Changed("turn-url") {
			return nil, errors.New("--turn-tcp and --turn-url need --turn-ports")
		}
		return nil, nil
	}
	low, high, ok := strings.Cut(ports, "-")
	minPort, err1 := strconv.ParseUint(low, 10, 16)
	maxPort, err2 := strconv.ParseUint(high, 10, 16)
	if !ok || err1 != nil || err2 != nil || minPort == 0 || maxPort < minPort {
		return nil, fmt.Errorf("--turn-ports \"%s\" is not a range such as 49152-49251", ports)
	}
	secret := os.Getenv("TURN_SECRET")
	if secret == "" {
		return nil, errors.New("--turn-ports needs the environment variable TURN_SECRET")
	}
	address, _ := flags.GetString("turn-ip")
	if address == "" {
		listen, _ := flags.GetString("listen")
		address, _, _ = net.SplitHostPort(listen)
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("TURN needs the public IP address of this machine: set --turn-ip (not \"%s\")", address)
	}
	quota, _ := flags.GetInt64("turn-quota")
	urls, _ := flags.GetStringArray("turn-url")
	for _, u := range urls {
		if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
			return nil, fmt.Errorf("--turn-url \"%s\" is not a turn: or turns: URL", u)
		}
	}
	return &turn.Config{Secret: secret, PublicIP: ip, MinPort: uint16(minPort), MaxPort: uint16(maxPort), Quota: quota << 20, URLs: urls}, nil
}

// stunListen is the UDP address of the STUN server of a relay, or empty for
// none. A citizen asks the host of its relay on port 3478, so the default
// keeps the host of the relay.
func stunListen(command *cobra.Command, listen string) string {
	value, _ := command.Flags().GetString("stun")
	switch value {
	case "none":
		return ""
	case "":
		host, _, _ := net.SplitHostPort(listen)
		return net.JoinHostPort(host, strconv.Itoa(stun.Port))
	}
	return value
}

// writePolicy reads the write limits of a relay from its flags.
func writePolicy(command *cobra.Command) limits.Policy {
	flags := command.Flags()
	var p limits.Policy
	p.MaxEventBytes, _ = flags.GetInt("max-event-bytes")
	p.WrapAuth, _ = flags.GetBool("wrap-auth")
	p.WrapPoW, _ = flags.GetInt("wrap-pow")
	p.Rate, _ = flags.GetInt("rate")
	p.Burst, _ = flags.GetInt("burst")
	p.IPHeader, _ = flags.GetString("ip-header")
	mb, _ := flags.GetInt64("max-store-mb")
	p.MaxStoreBytes = mb << 20
	return p
}

// session is one open store, with the key and the relays of this citizen.
func open(command *cobra.Command) (*citizen.Session, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	return openDir(command, dir)
}

// openDir opens the session of the identity in dir.
func openDir(command *cobra.Command, dir string) (*citizen.Session, error) {
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return nil, err
	}
	urls, err := readRelays(dir)
	if err != nil {
		return nil, err
	}
	indexers, err := readIndexers(dir)
	if err != nil {
		return nil, err
	}
	root, err := rootDir(command)
	if err != nil {
		return nil, err
	}
	var device keys.Key
	if id.remote {
		if device, err = keyfile.Load(filepath.Join(dir, "bunker-client")); err != nil {
			return nil, err
		}
	}
	return citizen.Open(citizen.Config{Home: dir, Root: root, Key: id.key, Signer: id.signer, Remote: id.remote, Device: device, URLs: urls, IndexerURLs: indexers, Errors: command.ErrOrStderr()})
}

func errRemote(what string) error {
	return fmt.Errorf("%s needs the secret key on this machine; a remote signer cannot do it yet", what)
}

func messageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "message",
		Short: "Private messages, carried by relays or by hand",
		Long: "A message waits in the outbox until its recipient acknowledges it.\n" +
			"arc sync moves it: through relays, or through a directory that\n" +
			"someone carries. A machine that carries a directory also carries\n" +
			"other citizens' mail, which it cannot read.",
	}

	send := &cobra.Command{
		Use: "send <public key>[,<public key>...] [text...]", Short: "Send a message, to one citizen or to a group",
		Long: "With several public keys, separated by commas, arc sends one group\n" +
			"message: each member sees the others, and a reply goes to all of them.\n" +
			"A group has at most 10 participants, with you.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			to, err := recipients(args[0])
			if err != nil {
				return err
			}
			// A reply is checked before the text, so a wrong id does not
			// wait for standard input.
			parent, err := replyParent(command)
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			if parent != (nostr.ID{}) && testTags(command, to) == nil {
				if err := sess.Mail.CheckReply(command.Context(), to, parent); err != nil {
					return err
				}
			}

			text := strings.Join(args[1:], " ")
			if text == "" {
				body, err := io.ReadAll(io.LimitReader(os.Stdin, private.MaxMailContent+1))
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(body), "\n")
			}
			if terminal.Blank(text) {
				return errors.New("a message holds at least one character that a person can see")
			}
			if len(text) > private.MaxMailContent {
				return fmt.Errorf("the message is too large for mail, which holds at most %d bytes: send it over a direct path, such as the transfer app", private.MaxMailContent)
			}

			out, err := sendMessage(command, sess, to, text)
			if err != nil {
				return err
			}
			inboxNotes(sess, out)
			if asJSON, _ := command.Flags().GetBool("json"); asJSON {
				if len(sess.Relays) == 0 {
					fmt.Fprintln(os.Stderr, "no relays: run arc sync --dir <path> to hand it to a courier")
				}
				reply, _ := command.Flags().GetString("reply")
				return json.NewEncoder(command.OutOrStdout()).Encode(queuedLine{
					ID: out[0].Rumor, To: out[0].To, State: out[0].State(time.Now()),
					Members: hexKeys(append([]nostr.PubKey{sess.Signer.PublicKey()}, to...)),
					ReplyTo: reply,
				})
			}
			fmt.Fprintf(command.OutOrStdout(), "queued for %s until %s\n", petnames(to), out[0].Expires.Local().Format("2006-01-02 15:04"))
			if len(sess.Relays) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no relays: run arc sync --dir <path> to hand it to a courier")
			}
			return nil
		},
	}

	inbox := &cobra.Command{
		Use: "inbox", Short: "List the messages you received", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			msgs, err := sess.Mail.Inbox(command.Context())
			if err != nil {
				return err
			}
			if err := printHolding(command, sess); err != nil {
				return err
			}
			asJSON, _ := command.Flags().GetBool("json")
			printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
			if asJSON {
				for _, m := range msgs {
					if err := printer.print(m); err != nil {
						return err
					}
				}
				return nil
			}
			if len(msgs) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no messages: run arc sync")
			}
			for _, m := range msgs {
				if err := printer.print(m); err != nil {
					return err
				}
			}
			n, err := sess.Mail.Carrying()
			if err != nil {
				return err
			}
			if n > 0 {
				fmt.Fprintf(command.OutOrStdout(), "\ncarrying %d sealed messages for other citizens\n", n)
			}
			return nil
		},
	}

	held := &cobra.Command{
		Use: "held", Short: "List the messages from strangers that arc held apart",
		Long: "A message from a key that is not a contact, without proof of work,\n" +
			"waits here, with no acknowledgement to its sender. It goes when its\n" +
			"wrap expires, after 7 days. arc contacts add <key> moves the messages\n" +
			"of that key to the inbox, and acknowledges them.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			msgs, err := sess.Mail.Held(command.Context())
			if err != nil {
				return err
			}
			asJSON, _ := command.Flags().GetBool("json")
			if len(msgs) == 0 && !asJSON {
				fmt.Fprintln(command.OutOrStdout(), "no held messages")
			}
			printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
			for _, m := range msgs {
				if err := printer.print(m); err != nil {
					return err
				}
			}
			return nil
		},
	}

	outbox := &cobra.Command{
		Use: "outbox", Short: "List the messages you sent, and whether they arrived", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			out, err := sess.Mail.Outbox(command.Context())
			if err != nil {
				return err
			}
			if asJSON, _ := command.Flags().GetBool("json"); asJSON {
				return printSent(command, sess.Mail, out)
			}
			if len(out) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no messages sent")
			}
			printOutbox(command.OutOrStdout(), out, time.Now())
			return nil
		},
	}

	send.Flags().Bool("json", false, "print the queued message as one JSON object")
	send.Flags().String("reply", "", "reply to the message with this id, 64 hex digits; the recipients are its members")
	addTestTags(send)
	inbox.Flags().Bool("json", false, "print one JSON object for each message")
	held.Flags().Bool("json", false, "print one JSON object for each message")
	outbox.Flags().Bool("json", false, "print one JSON object for each message, and no requests")
	command.AddCommand(send, inbox, held, outbox, messageWatchCommand())
	return command
}

func syncCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile this machine with its relays and storage citizens, or with a directory",
		Long: "Without --dir, arc syncs mail and announcements with every relay, and\n" +
			"self data, such as the journal, with each storage citizen. Self data\n" +
			"never goes to a relay. The first sync imports the drafts that the\n" +
			"relays still hold; --import runs that again. With --dir, arc syncs\n" +
			"everything with that directory: a USB stick, a shared folder, or a\n" +
			"disk you carry.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			sealed := sealedFilters(sess.Key.Public)
			repair, _ := command.Flags().GetBool("repair")
			turn, _ := command.Flags().GetBool("turn")
			if turn && !repair {
				return errors.New("--turn allows TURN for repair: give it with --repair")
			}

			targets := sess.Relays
			dir, _ := command.Flags().GetString("dir")
			if dir != "" {
				targets = []transport.Transport{file.Dir{Path: dir}}
			}
			if len(targets) == 0 {
				return errors.New("nothing to sync with: add a relay, or give --dir")
			}

			if dir != "" {
				refreshDevices(command.Context(), sess, command.ErrOrStderr())
			}
			failed := false
			for _, t := range targets {
				// Self data goes to a directory that the citizen carries,
				// but never to a relay.
				if dir != "" {
					report, err := syncSealed(command.Context(), sess.Node, sealed, noContacts{t})
					if err != nil {
						fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: %v", t.Name(), err)))
						failed = true
						continue
					}
					fmt.Fprintf(command.OutOrStdout(), "%s: sealed data received %d, sent %d, already held %d\n",
						terminal.EscapeLine(report.Transport), report.Received, report.Sent, report.Duplicate+report.Superseded)
					for _, reason := range report.Refused {
						fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
					}
					if err := dropDeleted(command.Context(), command.OutOrStdout(), sess.Node, sealed, file.Dir{Path: dir}, report); err != nil {
						fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: the events of deleted pages stay: %v", t.Name(), err)))
						failed = true
					}
					if report.Unreadable > 0 {
						fmt.Fprintf(os.Stderr, "  %d items did not parse\n", report.Unreadable)
					}
					for _, err := range report.SendFailed {
						fmt.Fprintf(os.Stderr, "  not sent: %s\n", terminal.EscapeLine(err.Error()))
					}
					bodies, err := sess.SyncBodies(dir)
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: bodies: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
						failed = true
					} else if bodies.Sent+bodies.Received+len(bodies.Missing) > 0 {
						fmt.Fprintf(command.OutOrStdout(), "%s: bodies received %d, sent %d\n", terminal.EscapeLine(report.Transport), bodies.Received, bodies.Sent)
						for _, sum := range bodies.Missing {
							fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("  the body %s is on neither side; the device that wrote it holds it", sum)))
						}
					}
				}

				if offers, err := sess.Node.Sync(command.Context(), announcements, t); err == nil && offers.Received > 0 {
					fmt.Fprintf(command.OutOrStdout(), "%s: announcements received %d\n", terminal.EscapeLine(t.Name()), offers.Received)
				}

				mails, err := sess.Mail.Sync(command.Context(), t)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: mail: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
					failed = true
					continue
				}
				fmt.Fprintf(command.OutOrStdout(), "%s: mail received %d, delivered %d, carried %d, sent %d\n",
					terminal.EscapeLine(mails.Transport), mails.Received, mails.Delivered, mails.Carried, mails.Sent)
				for _, reason := range mails.Refused {
					fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
				}
			}
			if dir == "" && len(sess.Relays) > 0 {
				if sent, err := sess.Mail.Forward(command.Context()); err != nil {
					fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("mail: the waiting messages were not sent to the relays of their recipients: %v", err)))
					failed = true
				} else if sent > 0 {
					fmt.Fprintf(command.OutOrStdout(), "mail: sent %d waiting %s to the relays that %s recipients read\n", sent, plural(sent, "message", "messages"), plural(sent, "its", "their"))
				}
			}
			if dir == "" {
				cfg, err := selfSyncConfig(command, sess)
				if err != nil {
					return err
				}
				again, _ := command.Flags().GetBool("import")
				cfg.Repair, cfg.RepairTURN = repair, turn
				if syncSelf(command.Context(), sess, cfg, again) != nil {
					failed = true
				}
			}
			if failed {
				return errors.New("some transports failed")
			}
			return nil
		},
	}
	command.Flags().String("dir", "", "sync with this directory instead of the relays")
	command.Flags().Bool("import", false, "import the drafts that the relays hold again")
	command.Flags().Bool("repair", false, "audit the pieces on the storage citizens, and rebuild, move or delete pieces; arc serve does this on each tick")
	command.Flags().Bool("turn", false, "with --repair, let repair use a pipe through TURN; it waits for a direct path by default")
	return command
}

// sealedFilters match the self data of a citizen: drafts, their checkpoints
// and parts, and deletion requests. The first matches what the citizen key
// signed. The second matches what a device key signed, by the p tag that
// names the citizen.
func sealedFilters(me nostr.PubKey) []nostr.Filter {
	kinds := []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion}
	return []nostr.Filter{
		{Kinds: kinds, Authors: []nostr.PubKey{me}},
		{Kinds: kinds, Tags: nostr.TagMap{"p": {me.Hex()}}},
	}
}

// syncSealed syncs each filter of self data with one transport, and adds
// the reports.
func syncSealed(ctx context.Context, n *node.Node, filters []nostr.Filter, t transport.Transport) (node.Report, error) {
	var sum node.Report
	// An event can match two filters. It counts once.
	deleted := map[nostr.ID]bool{}
	for _, filter := range filters {
		report, err := n.Sync(ctx, filter, t)
		if err != nil {
			return sum, err
		}
		sum.Transport = report.Transport
		sum.Received += report.Received
		sum.Duplicate += report.Duplicate
		sum.Superseded += report.Superseded
		sum.Refused = append(sum.Refused, report.Refused...)
		for _, id := range report.Deleted {
			if !deleted[id] {
				deleted[id] = true
				sum.Deleted = append(sum.Deleted, id)
			}
		}
		sum.Unreadable += report.Unreadable
		sum.Sent += report.Sent
		for _, err := range report.SendFailed {
			if !errors.Is(err, errContactDraft) {
				sum.SendFailed = append(sum.SendFailed, err)
			}
		}
	}
	return sum, nil
}

// dropDeleted removes from a directory each event that the store refused
// because its author deleted it, and says how many. Another device learns
// of the delete from the directory, so it removes nothing unless the
// directory holds a valid copy of each deletion request of the store.
func dropDeleted(ctx context.Context, out io.Writer, n *node.Node, filters []nostr.Filter, d file.Dir, report node.Report) error {
	if len(report.Deleted) == 0 {
		return nil
	}
	held, err := holdsDeletions(ctx, n, filters, d)
	if err != nil {
		return err
	}
	if !held {
		fmt.Fprintf(out, "%s: %d events of deleted pages left out; they stay in the directory, because it lacks a valid copy of a deletion request\n", terminal.EscapeLine(report.Transport), len(report.Deleted))
		return nil
	}
	removed, err := d.Remove(report.Deleted)
	fmt.Fprintf(out, "%s: %d events of deleted pages left out, %d removed from the directory\n", terminal.EscapeLine(report.Transport), len(report.Deleted), removed)
	return err
}

// holdsDeletions says whether the directory holds each deletion request
// of the store that the filters match, and whether each copy verifies. A
// changed copy keeps the ID of the request, so the sync sent no valid one.
func holdsDeletions(ctx context.Context, n *node.Node, filters []nostr.Filter, d file.Dir) (bool, error) {
	want := map[nostr.ID]bool{}
	for _, filter := range filters {
		filter.Kinds = []nostr.Kind{nostr.KindDeletion}
		found, err := n.Store.Query(filter)
		if err != nil {
			return false, err
		}
		for _, e := range found {
			want[e.ID] = true
		}
	}
	if len(want) == 0 {
		return false, nil
	}
	batch, err := d.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}})
	if err != nil {
		return false, err
	}
	for _, e := range batch.Events {
		if want[e.ID] && store.Verify(e) == nil {
			delete(want, e.ID)
		}
	}
	return len(want) == 0, nil
}

// errContactDraft says that a directory gets no draft of the contact list.
var errContactDraft = errors.New("a draft of the contact list stays off a directory")

// noContacts gives a directory each event but the drafts of the contact
// list. A stick can reach a courier, and such a draft names the citizen.
type noContacts struct{ transport.Transport }

func (t noContacts) Send(ctx context.Context, event nostr.Event) error {
	if citizen.ContactDraft(event) {
		return errContactDraft
	}
	return t.Transport.Send(ctx, event)
}
