// Command arc-storage runs the storage app service, and holds the commands
// of the owner:
//
//	arc-storage                   the service, under arc serve
//	arc-storage put <file>...     give blobs to the storage citizen
//	arc-storage list              list the changes after a cursor
//	arc-storage get <id>...       get blobs
//	arc-storage delete <id>...    delete blobs
//	arc-storage usage             show the bytes in use and the quota
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
	"syscall"

	"github.com/gezibash/arc/apps/storage/client"
	"github.com/gezibash/arc/apps/storage/server"
	"github.com/gezibash/arc/apps/storage/wire"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/stdio"
)

const usage = `usage:
  arc-storage                          the service, under arc serve
  arc-storage put [flags] <file>...    give blobs to the storage citizen
  arc-storage list [flags]             list the changes after a cursor
  arc-storage get [flags] <id>...      get blobs
  arc-storage delete [flags] <id>...   delete blobs
  arc-storage usage [flags]            show the bytes in use and the quota

arc-storage <command> -h shows the flags of a command.`

func main() {
	if len(os.Args) < 2 {
		stdio.Main("arc-storage", server.Run)
		return
	}
	commands := map[string]func(context.Context, []string) error{
		"put": put, "list": list, "get": get, "delete": remove, "usage": showUsage,
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
	for _, it := range items {
		status, err := s.Put(ctx, it.id, it.blob)
		if err != nil {
			return fmt.Errorf("%s: the storage citizen refused the put: %w", it.file, err)
		}
		fmt.Printf("%s\t%s\t%s\n", it.id, status, it.file)
	}
	return nil
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
	for _, id := range flags.Args() {
		blob, err := s.Get(ctx, id)
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
	}
	return nil
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
	for _, id := range flags.Args() {
		status, err := s.Delete(ctx, id)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Printf("%s\t%s\n", id, status)
	}
	return nil
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
	used, quota, blobs, err := s.Usage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d bytes of %d in use, %d blobs\n", used, quota, blobs)
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
func open(ctx context.Context, s *settings, need int64) (*client.Session, error) {
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
