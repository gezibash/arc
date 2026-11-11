package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func storageKeysCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "storage",
		Short: "Ask storage citizens for an account, and keep the storage list",
		Long: "The storage list names the storage citizens of this citizen. It is an\n" +
			"event of kind 10273 on the relays of the citizen, sealed to the citizen\n" +
			"key. Only the signer changes it. Each device reads it at each sync, and\n" +
			"installs each storage citizen that it names.",
		Args: cobra.NoArgs,
	}
	add := &cobra.Command{
		Use:   "add <invite link or key of the storage citizen>",
		Short: "Ask a storage citizen for an account, and install it when it admits",
		Long: "arc derives the account key of this citizen for the storage citizen,\n" +
			"and sends one join call that the account key signs. The link\n" +
			"storage+arc://<key>?invite=<token> of the operator carries an invite.\n" +
			"If the storage citizen admits the account, arc installs its storage\n" +
			"app, adds it to the storage list as active, and sends the grant set\n" +
			"of the device list. If the operator\n" +
			"decides by hand, the account is pending: run the command again later.\n" +
			"Run this on the signer.",
		Args: cobra.ExactArgs(1),
		RunE: addStorage,
	}
	add.Flags().String("want", "1GiB", "the quota to ask for")
	add.Flags().String("note", "", "a note for the operator, at most 140 bytes")
	add.Flags().Bool("show-key", false, "show the citizen key to the operator, signed over the account key")
	add.Flags().String("as", citizen.StorageApp, "the installed name of the storage citizen")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Print the storage list",
		Args:  cobra.NoArgs,
		RunE:  listStorage,
	}
	rm := &cobra.Command{
		Use:   "rm <name or key>",
		Short: "Mark a storage citizen draining, or gone with --gone",
		Long: "rm sets the state draining in the storage list. Each device sends the\n" +
			"storage citizen a drain request, so that it takes no put, and repair\n" +
			"moves its pieces. When it holds no piece, the sync of the signer sets\n" +
			"the state gone. With --gone, rm sets the state gone now: each device\n" +
			"removes its install at the next sync. The signer removes a gone entry\n" +
			"after 30 days. Run this on the signer.",
		Args: cobra.ExactArgs(1),
		RunE: removeStorage,
	}
	rm.Flags().Bool("gone", false, "the storage citizen is gone: each device removes its install")
	command.AddCommand(add, ls, rm)
	return command
}

func listStorage(command *cobra.Command, _ []string) error {
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	list, ok, err := sess.StorageList(command.Context())
	if err != nil {
		return err
	}
	if !ok || len(list.Entries) == 0 {
		fmt.Fprintln(command.OutOrStdout(), "the storage list is empty; add a storage citizen with arc keys storage add on the signer")
		return nil
	}
	for _, entry := range list.Entries {
		since := time.Unix(int64(entry.Since), 0).UTC().Format("2006-01-02 15:04")
		fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\tsince %s\t%s\n", terminal.EscapeLine(entry.Name), terminal.EscapeLine(string(entry.State)), entry.Key.Hex(), since, terminal.EscapeLine(strings.Join(entry.Relays, " ")))
	}
	return nil
}

func removeStorage(command *cobra.Command, args []string) error {
	gone, _ := command.Flags().GetBool("gone")
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	if err := sess.RequireSigner(); err != nil {
		return err
	}
	ctx := command.Context()
	list, ok, err := sess.StorageList(ctx)
	if err != nil {
		return err
	}
	entry, found := list.Find(args[0])
	if !ok || !found {
		return fmt.Errorf("the storage list names no storage citizen %s; see arc keys storage ls", args[0])
	}
	state := storage.StateDraining
	if gone {
		state = storage.StateGone
	}
	return putStorage(ctx, command.OutOrStdout(), sess, list, entry.Key, entry.Name, entry.Relays, state)
}

// putStorage sets the state of one storage citizen in the storage list,
// and publishes the list. Since changes only with the state.
func putStorage(ctx context.Context, out io.Writer, sess *citizen.Session, list storage.List, key nostr.PubKey, name string, relays []string, state string) error {
	entry := storage.ListEntry{Key: key, Name: name, Relays: relays, State: state, Since: nostr.Now()}
	if have, ok := list.Find(key.Hex()); ok && have.State == state {
		entry.Since = have.Since
	}
	list, removed := list.Prune(time.Now())
	list, err := list.Put(entry)
	if err != nil {
		return err
	}
	if _, err := sess.PublishStorageList(ctx, list); err != nil {
		return fmt.Errorf("the storage list did not change: %w", err)
	}
	for _, e := range removed {
		fmt.Fprintf(out, "storage list: removed %s, gone since %s\n", terminal.EscapeLine(e.Name), e.Since.Time().UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(out, "storage list: %s is %s\n", terminal.EscapeLine(name), state)
	if state == storage.StateDraining {
		fmt.Fprintf(out, "each sync sends %s a drain request, and repair moves its pieces; the signer marks it gone when it holds none\n", terminal.EscapeLine(name))
	}
	return nil
}

func addStorage(command *cobra.Command, args []string) error {
	as, _ := command.Flags().GetString("as")
	if err := checkStorageName(as); err != nil {
		return err
	}
	key, token, err := storage.ParseInvite(args[0])
	if err != nil {
		return err
	}
	provider, err := nostr.PubKeyFromHex(key)
	if err != nil {
		return err
	}
	wantText, _ := command.Flags().GetString("want")
	want, err := storage.ParseSize(wantText)
	if err != nil {
		return fmt.Errorf("--want: %w", err)
	}
	note, _ := command.Flags().GetString("note")
	if len(note) > storage.MaxNote {
		return fmt.Errorf("--note has %d bytes; the most is %d", len(note), storage.MaxNote)
	}
	show, _ := command.Flags().GetBool("show-key")

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	ctx := command.Context()
	reply, err := sess.JoinStorage(ctx, provider, storage.Join{Invite: token, Want: want, Note: note}, show)
	if errors.Is(err, citizen.ErrOlderStorage) {
		return fmt.Errorf("%s %w", as, err)
	}
	if err != nil {
		return fmt.Errorf("%s: the join failed: %w", as, err)
	}
	if reply.Error != "" {
		return fmt.Errorf("%s refused the account: %s", as, reply.Message)
	}
	account := reply.Account
	if account.Status == storage.StatusPending {
		fmt.Fprint(command.OutOrStdout(), accountPending(as, account))
		return nil
	}
	fmt.Fprint(command.OutOrStdout(), accountAdmitted(as, account))

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	name, err := installStorage(command, sess, installs, provider, as)
	if err != nil {
		return err
	}
	storageList, _, err := sess.StorageList(ctx)
	if err != nil {
		return fmt.Errorf("the storage list did not come, so it does not name %s: %w; run arc keys storage add again", name, err)
	}
	if err := putStorage(ctx, command.OutOrStdout(), sess, storageList, provider, name, sess.RelayURLs(), storage.StateActive); err != nil {
		return err
	}
	list, ok, err := sess.DeviceList(ctx)
	if err != nil {
		return fmt.Errorf("the device list did not come, so no grants went: %w; run arc sync to try again", err)
	}
	if !ok {
		return nil
	}
	targets, err := sess.PushGrants(ctx, installs, list)
	for _, install := range targets {
		fmt.Fprintf(command.OutOrStdout(), "grants: sent to the storage citizen %s\n", terminal.EscapeLine(install.As))
	}
	if err != nil {
		return fmt.Errorf("%w; run arc sync to try again", err)
	}
	return nil
}

// installStorage installs the storage app of the storage citizen as name,
// unless it is installed. It returns the installed name.
func installStorage(command *cobra.Command, sess *citizen.Session, installs catalog.Installs, provider nostr.PubKey, as string) (string, error) {
	all, err := installs.List()
	if err != nil {
		return "", err
	}
	for _, install := range all {
		if install.Provider == provider.Hex() && install.ID == citizen.StorageApp {
			return install.As, nil
		}
	}
	for _, install := range all {
		if install.As == as {
			return "", fmt.Errorf("the name %s is installed for another app; choose another name with --as", as)
		}
	}
	if builtinName(command.Root(), as) {
		return "", fmt.Errorf("%s is a name of arc itself; choose another name with --as", as)
	}
	offer, err := sess.FindOffer(command.Context(), provider, citizen.StorageApp)
	if err != nil {
		return "", err
	}
	if err := installs.Add(offer, as); err != nil {
		return "", err
	}
	fmt.Fprintf(command.OutOrStdout(), "installed %s\n", terminal.EscapeLine(as))
	return as, nil
}

// checkStorageName refuses an installed name that the storage list cannot
// hold, before arc asks the storage citizen: else arc would install the
// storage citizen, and the list would not name it. A name holds no control
// character and no format character, such as a bidi mark, so that it shows
// as itself in each line that names it (#441).
func checkStorageName(as string) error {
	if as == "" || len(as) > storage.MaxEntryName || strings.ContainsFunc(as, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return fmt.Errorf("--as: a storage name has 1 to %d bytes, and no control character and no format character", storage.MaxEntryName)
	}
	return nil
}

// accountPending and accountAdmitted are the lines that arc keys storage add
// prints for the answer of a storage citizen. The message, the plan and the
// state come from the storage citizen, so each is escaped on one line
// (docs/clients/SPEC.md, section 3.9).
func accountPending(as string, account *storage.Account) string {
	return fmt.Sprintf("%s: pending: %s\nrun arc keys storage add again later; the operator lists the request with arc-storage requests\n", as, terminal.EscapeLine(account.Message))
}

func accountAdmitted(as string, account *storage.Account) string {
	return fmt.Sprintf("%s: admitted: %s of %s in use, plan %s, %s\n", as, storage.HumanSize(account.Used), storage.HumanSize(account.Quota),
		terminal.EscapeLine(account.Plan), terminal.EscapeLine(account.State))
}
