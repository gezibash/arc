package main

import (
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/spf13/cobra"
)

func storageKeysCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "storage",
		Short: "Ask storage citizens for an account",
		Args:  cobra.NoArgs,
	}
	add := &cobra.Command{
		Use:   "add <invite link or key of the storage citizen>",
		Short: "Ask a storage citizen for an account, and install it when it admits",
		Long: "arc derives the account key of this citizen for the storage citizen,\n" +
			"and sends one join call that the account key signs. The link\n" +
			"storage+arc://<key>?invite=<token> of the operator carries an invite.\n" +
			"If the storage citizen admits the account, arc installs its storage\n" +
			"app and sends the grant set of the device list. If the operator\n" +
			"decides by hand, the account is pending: run the command again later.\n" +
			"Run this on the signer.",
		Args: cobra.ExactArgs(1),
		RunE: addStorage,
	}
	add.Flags().String("want", "1GiB", "the quota to ask for")
	add.Flags().String("note", "", "a note for the operator, at most 140 bytes")
	add.Flags().Bool("show-key", false, "show the citizen key to the operator, signed over the account key")
	add.Flags().String("as", citizen.StorageApp, "the installed name of the storage citizen")
	command.AddCommand(add)
	return command
}

func addStorage(command *cobra.Command, args []string) error {
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
	as, _ := command.Flags().GetString("as")

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
		fmt.Printf("%s: pending: %s\nrun arc keys storage add again later; the operator lists the request with arc-storage requests\n", as, account.Message)
		return nil
	}
	fmt.Printf("%s: admitted: %s of %s in use, plan %s, %s\n", as, storage.HumanSize(account.Used), storage.HumanSize(account.Quota), account.Plan, account.State)

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	if err := installStorage(command, sess, installs, provider, as); err != nil {
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
		fmt.Printf("grants: sent to the storage citizen %s\n", install.As)
	}
	if err != nil {
		return fmt.Errorf("%w; run arc sync to try again", err)
	}
	return nil
}

// installStorage installs the storage app of the storage citizen as name,
// unless it is installed.
func installStorage(command *cobra.Command, sess *citizen.Session, installs catalog.Installs, provider nostr.PubKey, as string) error {
	all, err := installs.List()
	if err != nil {
		return err
	}
	for _, install := range all {
		if install.Provider == provider.Hex() && install.ID == citizen.StorageApp {
			return nil
		}
		if install.As == as {
			return fmt.Errorf("the name %s is installed for another app; choose another name with --as", as)
		}
	}
	if builtinName(command.Root(), as) {
		return fmt.Errorf("%s is a command of arc; choose another name with --as", as)
	}
	offer, err := sess.FindOffer(command.Context(), provider, citizen.StorageApp)
	if err != nil {
		return err
	}
	if err := installs.Add(offer, as); err != nil {
		return err
	}
	fmt.Printf("installed %s\n", as)
	return nil
}
