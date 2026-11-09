package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gezibash/arc/apps/storage/client"
	"github.com/gezibash/arc/apps/storage/server"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

func storageDir(flags *flag.FlagSet) *string {
	return flags.String("dir", os.Getenv("STORAGE_DIR"), "the storage directory (default STORAGE_DIR, or ~/.local/state/arc-storage)")
}

func operator(dir string) (*server.Operator, error) {
	dir, err := server.Dir(dir)
	if err != nil {
		return nil, err
	}
	return server.NewOperator(dir), nil
}

func sizeFlag(flags *flag.FlagSet, name, usage string) *string {
	return flags.String(name, "1GiB", usage)
}

func quotaOf(text string) (int64, error) {
	quota, err := wire.ParseSize(text)
	if err == nil && quota <= 0 {
		err = errors.New("the quota must be more than 0 bytes")
	}
	return quota, err
}

func accountArg(args []string, usage string) (string, error) {
	if len(args) != 1 || !wire.IsHex64(strings.ToLower(args[0])) {
		return "", errors.New(usage)
	}
	return strings.ToLower(args[0]), nil
}

// makeInvite makes an invite token, and prints the link
// storage+arc://<key of the storage citizen>?invite=<token>. arc
// whoami gives the key, or -key.
func makeInvite(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage invite", flag.ContinueOnError)
	dir := storageDir(flags)
	quota := sizeFlag(flags, "quota", "the quota of the account")
	plan := flags.String("plan", server.DefaultPlan, "the plan of the account")
	key := flags.String("key", "", "the key of the storage citizen (default: arc whoami in -home)")
	program := flags.String("arc", "arc", "the arc program")
	home := flags.String("home", "", "the arc home of the storage citizen")
	positional, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: arc-storage invite [-quota 1GiB] [-plan free]")
	}
	q, err := quotaOf(*quota)
	if err != nil {
		return fmt.Errorf("-quota: %w", err)
	}
	self := strings.ToLower(*key)
	if self == "" {
		out, err := client.Arc{Program: *program, Home: *home}.Command(ctx, "whoami").Output()
		if err != nil {
			return fmt.Errorf("arc whoami: %w", err)
		}
		if lines := strings.Split(string(out), "\n"); len(lines) > 1 {
			self = strings.TrimSpace(lines[1])
		}
	}
	if !wire.IsHex64(self) {
		return fmt.Errorf("%q is not the key of the storage citizen; give it with -key", self)
	}
	o, err := operator(*dir)
	if err != nil {
		return err
	}
	token, err := o.Invite(q, *plan)
	if err != nil {
		return err
	}
	fmt.Println(wire.InviteLink(self, token))
	fmt.Fprintf(os.Stderr, "the link admits one account at %d bytes, plan %s, until %s\n", q, *plan, time.Now().Add(server.InviteAge).UTC().Format(time.DateOnly))
	return nil
}

// requests prints each request that waits: the account key, the bytes
// that it wants, the time, the citizen key or -, and the note.
func requests(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage requests", flag.ContinueOnError)
	dir := storageDir(flags)
	if positional, err := parse(flags, args); err != nil || len(positional) != 0 {
		if err == nil {
			err = errors.New("usage: arc-storage requests")
		}
		return err
	}
	o, err := operator(*dir)
	if err != nil {
		return err
	}
	all, err := o.Requests()
	if err != nil {
		return err
	}
	for _, r := range all {
		citizen := r.Citizen
		if citizen == "" {
			citizen = "-"
		}
		fmt.Println(requestLine(r, citizen))
	}
	return nil
}

// accept admits an account at a quota.
func accept(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage accept", flag.ContinueOnError)
	dir := storageDir(flags)
	quota := sizeFlag(flags, "quota", "the quota of the account")
	plan := flags.String("plan", server.DefaultPlan, "the plan of the account")
	positional, err := parse(flags, args)
	if err != nil {
		return err
	}
	account, err := accountArg(positional, "usage: arc-storage accept <account key> [-quota 1GiB] [-plan free]")
	if err != nil {
		return err
	}
	q, err := quotaOf(*quota)
	if err != nil {
		return fmt.Errorf("-quota: %w", err)
	}
	o, err := operator(*dir)
	if err != nil {
		return err
	}
	if err := o.Accept(account, q, *plan); err != nil {
		return err
	}
	fmt.Printf("admitted %s at %d bytes, plan %s; its next join gets admitted\n", account, q, *plan)
	return nil
}

// refuse refuses the request of an account.
func refuse(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage refuse", flag.ContinueOnError)
	dir := storageDir(flags)
	positional, err := parse(flags, args)
	if err != nil {
		return err
	}
	account, err := accountArg(positional, "usage: arc-storage refuse <account key>")
	if err != nil {
		return err
	}
	o, err := operator(*dir)
	if err != nil {
		return err
	}
	if err := o.Refuse(account); err != nil {
		return err
	}
	fmt.Printf("refused %s; its next join gets refused\n", account)
	return nil
}

// changeAccount runs account set and account close. An account key of
// STORAGE_OWNERS gets a record at STORAGE_QUOTA first.
func changeAccount(_ context.Context, args []string) error {
	const usage = "usage: arc-storage account set <account key> [-quota 2GiB] [-plan name] [-expires 2027-01-31|none]\n       arc-storage account close <account key>"
	if len(args) == 0 || (args[0] != "set" && args[0] != "close") {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("arc-storage account "+args[0], flag.ContinueOnError)
	dir := storageDir(flags)
	var quota, plan, expires *string
	if args[0] == "set" {
		quota = flags.String("quota", "", "the new quota")
		plan = flags.String("plan", "", "the new plan")
		expires = flags.String("expires", "", "the day the account closes, as 2027-01-31, or none")
	}
	positional, err := parse(flags, args[1:])
	if err != nil {
		return err
	}
	account, err := accountArg(positional, usage)
	if err != nil {
		return err
	}
	owners := strings.FieldsFunc(strings.ToLower(os.Getenv("STORAGE_OWNERS")), func(r rune) bool { return r == ',' || r == ' ' })
	ownerQuota, err := wire.ParseSize(os.Getenv("STORAGE_QUOTA"))
	if err != nil {
		return fmt.Errorf("STORAGE_QUOTA: %w", err)
	}
	if ownerQuota == 0 {
		ownerQuota = server.DefaultQuota
	}
	o, err := operator(*dir)
	if err != nil {
		return err
	}
	if args[0] == "close" {
		if err := o.Close(account, owners, ownerQuota); err != nil {
			return err
		}
		fmt.Printf("closing %s: it takes no put, and its reads end in 30 days\n", account)
		return nil
	}
	var c server.Change
	if *quota != "" {
		q, err := wire.ParseSize(*quota)
		if err != nil {
			return fmt.Errorf("-quota: %w", err)
		}
		c.Quota = &q
	}
	if *plan != "" {
		c.Plan = plan
	}
	switch *expires {
	case "":
	case "none":
		var zero int64
		c.Expires = &zero
	default:
		day, err := time.Parse(time.DateOnly, *expires)
		if err != nil {
			return fmt.Errorf("-expires: %q is not a day such as 2027-01-31, or none", *expires)
		}
		end := day.Unix()
		c.Expires = &end
	}
	if c == (server.Change{}) {
		return errors.New(usage)
	}
	if err := o.Set(account, owners, ownerQuota, c); err != nil {
		return err
	}
	fmt.Printf("changed %s\n", account)
	return nil
}

func requestLine(r server.Request, citizen string) string {
	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s", r.Account, r.Want, r.Asked.UTC().Format(time.DateTime), citizen, terminal.EscapeLine(r.Note))
}
