// Command arc-storage runs the storage app service, and holds the commands
// of the owner:
//
//	arc-storage                   the service, under arc serve
//	arc-storage put <file>...     give blobs to the storage citizen
//	arc-storage list              list the changes after a cursor
//	arc-storage get <id>...       get blobs
//	arc-storage delete <id>...    delete blobs
//	arc-storage usage             show the bytes in use and the quota
//
// and the commands of the operator, which work on STORAGE_DIR:
//
//	arc-storage invite            make an invite link
//	arc-storage requests          list the requests that wait
//	arc-storage accept <account>  admit an account
//	arc-storage refuse <account>  refuse the request of an account
//	arc-storage account set|close change or close an account
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/apps/storage/client"
	"github.com/gezibash/arc/apps/storage/server"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/stdio"
	wire "github.com/gezibash/arc/sdk/storage"
)

const usage = `usage:
  arc-storage                          the service, under arc serve
  arc-storage put [flags] <file>...    give blobs to the storage citizen
  arc-storage list [flags]             list the changes after a cursor
  arc-storage get [flags] <id>...      get blobs
  arc-storage delete [flags] <id>...   delete blobs
  arc-storage usage [flags]            show the bytes in use and the quota

The commands of the operator work on the disk of the storage citizen:
  arc-storage invite [flags]                 make an invite link
  arc-storage requests [flags]               list the requests that wait
  arc-storage accept <account> [flags]       admit an account
  arc-storage refuse <account> [flags]       refuse the request of an account
  arc-storage account set <account> [flags]  change the quota, plan or end
  arc-storage account close <account>        refuse puts; reads end in 30 days

arc-storage <command> -h shows the flags of a command.`

func main() {
	if len(os.Args) < 2 {
		stdio.Main("arc-storage", server.Run)
		return
	}
	commands := map[string]func(context.Context, []string) error{
		"put": put, "list": list, "get": get, "delete": remove, "usage": showUsage,
		"invite": makeInvite, "requests": requests, "accept": accept, "refuse": refuse, "account": changeAccount,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[2:])
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "arc-storage:", err)
		os.Exit(1)
	}
}

// put pads each file to a blob, and gives it. The ID of a blob is the
// SHA-256 of the blob, so a second put of a file sends no bytes. It prints
// one line for each file: the ID, the status and the file.
func put(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage put", flag.ContinueOnError)
	c := clientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("usage: arc-storage put [flags] <files>")
	}
	type item struct {
		file, id string
		blob     []byte
	}
	var items []item
	var need int64
	for _, file := range flags.Args() {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		blob, err := wire.Pad(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		sum := sha256.Sum256(blob)
		items = append(items, item{file, hex.EncodeToString(sum[:]), blob})
		need += int64(len(blob))
	}
	s, err := open(ctx, c, need)
	if err != nil {
		return err
	}
	defer s.Close()
	blobs := make([]wire.Blob, len(items))
	for i, it := range items {
		blobs[i] = wire.Blob{ID: it.id, Data: it.blob}
	}
	results, err := s.PutMany(ctx, blobs)
	for i, r := range results {
		if r.Err != nil {
			return fmt.Errorf("%s: the storage citizen refused the put: %w", items[i].file, r.Err)
		}
		if r.Status != "" {
			fmt.Printf("%s\t%s\t%s\n", items[i].id, r.Status, items[i].file)
		}
	}
	return err
}

// list prints the last change of each blob after the cursor, one line for
// each: the sequence number, the ID, and the size or "deleted". The last
// sequence number is the cursor of the next list.
func list(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage list", flag.ContinueOnError)
	c := clientFlags(flags)
	after := flags.Uint64("after", 0, "the cursor: list the changes after this sequence number")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: arc-storage list [flags]")
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	entries, next, err := s.List(ctx, *after)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Deleted {
			fmt.Printf("%d\t%s\tdeleted\n", e.Seq, e.ID)
		} else {
			fmt.Printf("%d\t%s\t%d\n", e.Seq, e.ID, e.Size)
		}
	}
	fmt.Fprintf(os.Stderr, "cursor %d\n", next)
	return nil
}

// get writes the data of each blob to a file named by its ID, in the
// directory of -dir, or to the file of -o.
func get(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage get", flag.ContinueOnError)
	c := clientFlags(flags)
	dir := flags.String("dir", ".", "the directory of the files")
	output := flags.String("o", "", "the file to write, for one ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 || (*output != "" && flags.NArg() != 1) {
		return errors.New("usage: arc-storage get [flags] <ids>")
	}
	for _, id := range flags.Args() {
		if !wire.IsID(id) {
			return fmt.Errorf("%q is not a blob ID of 64 hex digits", id)
		}
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.GetMany(ctx, flags.Args(), func(id string, blob []byte, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if sum := sha256.Sum256(blob); hex.EncodeToString(sum[:]) != id {
			return fmt.Errorf("%s: the bytes do not have the SHA-256 of the ID", id)
		}
		data, err := wire.Unpad(blob)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		file := *output
		if file == "" {
			file = filepath.Join(*dir, id)
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return err
		}
		fmt.Println(file)
		return nil
	})
}

// remove deletes each blob, and prints the ID and the status.
func remove(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage delete", flag.ContinueOnError)
	c := clientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("usage: arc-storage delete [flags] <ids>")
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	items, err := s.DeleteMany(ctx, flags.Args())
	for _, item := range items {
		if item.Error != "" {
			return fmt.Errorf("%s: the storage citizen refused the delete: %s", item.ID, item.Error)
		}
		fmt.Printf("%s\t%s\n", item.ID, item.Status)
	}
	return err
}

// showUsage prints the bytes in use, the quota and the number of blobs.
func showUsage(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage usage", flag.ContinueOnError)
	c := clientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	u, err := s.Usage(ctx)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("%d bytes of %d in use, %d blobs", u.Used, u.Quota, u.Blobs)
	if u.Plan != "" {
		line += fmt.Sprintf(", plan %s, %s", u.Plan, u.State)
	}
	if u.Expires > 0 {
		line += ", ends " + time.Unix(u.Expires, 0).UTC().Format(time.DateOnly)
	}
	fmt.Println(line)
	return nil
}

// parse parses flags that come before or after the positional arguments,
// as in arc-storage accept <account> -quota 1GiB.
func parse(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
}

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
		fmt.Printf("%s\t%d\t%s\t%s\t%s\n", r.Account, r.Want, r.Asked.UTC().Format(time.DateTime), citizen, r.Note)
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

// settings are the flags that each command shares.
type settings struct {
	client client.Client
	stun   string
}

func clientFlags(flags *flag.FlagSet) *settings {
	s := &settings{}
	flags.StringVar(&s.client.Arc.Program, "arc", "arc", "the arc program")
	flags.StringVar(&s.client.Arc.Home, "home", "", "the arc home (default: the home that arc picks)")
	flags.StringVar(&s.client.App, "app", "storage", "the installed name of the storage app")
	flags.StringVar(&s.stun, "stun", os.Getenv("STORAGE_STUN"), "the STUN server, or none (default STORAGE_STUN, or the first relay on port "+direct.RelaySTUNPort+")")
	flags.BoolVar(&s.client.HoldFirst, "hold", false, "ask the storage citizen to wait in the first attempt, not in the second")
	flags.BoolVar(&s.client.NoRelay, "no-relay", false, "fail if no direct path opens; do not send the bytes through the TURN server of the relay")
	return s
}

// open opens a pipe to the storage citizen, and says the path on standard
// error.
func open(ctx context.Context, s *settings, need int64) (*wire.Conn, error) {
	stun := direct.STUNURL(s.stun, nil)
	if s.stun == "" {
		relays, err := s.client.Arc.Relays(ctx)
		if err != nil {
			return nil, err
		}
		stun = direct.STUNURL("", relays)
	}
	c := s.client
	c.Options = direct.Options{STUN: stun, Loopback: os.Getenv("STORAGE_LOOPBACK") != ""}
	c.Notes = os.Stderr
	session, err := c.Open(ctx, need)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "path %s\n", session.Path)
	return session, nil
}
