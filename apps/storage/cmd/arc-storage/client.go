package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gezibash/arc/apps/storage/client"
	"github.com/gezibash/arc/sdk/direct"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

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
			fmt.Println(putLine(items[i].id, r.Status, items[i].file))
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
			fmt.Printf("%d\t%s\tdeleted\n", e.Seq, e.ID) //arc:raw-ok List refuses an ID that is not 64 hex digits
		} else {
			fmt.Printf("%d\t%s\t%d\n", e.Seq, e.ID, e.Size) //arc:raw-ok List refuses an ID that is not 64 hex digits
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
		fmt.Println(deleteLine(item.ID, item.Status))
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
	fmt.Println(usageLine(u))
	return nil
}

// audit checks that the storage citizen holds the blob of each file,
// without a get. It sends a new nonce, and compares each answer with the
// proof of the local bytes. It prints one line for each file: the ID,
// pass, fail or lost, and the file. A fail or a lost blob is an error.
func audit(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage audit", flag.ContinueOnError)
	c := clientFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("usage: arc-storage audit [flags] <files>")
	}
	nonce := wire.NewNonce()
	files := flags.Args()
	ids := make([]string, len(files))
	proofs := make([]string, len(files))
	for i, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		blob, err := wire.Pad(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		sum := sha256.Sum256(blob)
		ids[i] = hex.EncodeToString(sum[:])
		proofs[i] = wire.AuditProof(nonce, ids[i], blob)
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	items, err := s.Audit(ctx, ids, nonce)
	if err != nil {
		return err
	}
	failed := 0
	for i, item := range items {
		result := "pass"
		switch {
		case item.Error == wire.ErrNotFound:
			result = "lost"
		case item.Error != "":
			result = item.Error
		case item.Proof != proofs[i]:
			result = "fail"
		}
		if result != "pass" {
			failed++
		}
		fmt.Println(auditLine(ids[i], result, files[i]))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d blobs failed the audit", failed, len(items))
	}
	return nil
}

// drain sets the account to draining, or back to open with -off, and
// prints what remains.
func drain(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-storage drain", flag.ContinueOnError)
	c := clientFlags(flags)
	off := flags.Bool("off", false, "end the drain: the account takes puts again")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: arc-storage drain [-off]")
	}
	s, err := open(ctx, c, 0)
	if err != nil {
		return err
	}
	defer s.Close()
	u, err := s.Drain(ctx, *off)
	if err != nil {
		return err
	}
	fmt.Println(drainLine(u))
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
	flags.BoolVar(&s.client.AnswererFirst, "answerer-first", false, "let the storage citizen send first in the first attempt, and ask it to wait in the second")
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

func putLine(id, status, file string) string {
	return fmt.Sprintf("%s\t%s\t%s", id, terminal.EscapeLine(status), file)
}

func deleteLine(id, status string) string {
	return fmt.Sprintf("%s\t%s", id, terminal.EscapeLine(status))
}

func usageLine(u wire.Usage) string {
	line := fmt.Sprintf("%d bytes of %d in use, %d blobs", u.Used, u.Quota, u.Blobs)
	if u.Plan != "" {
		line += fmt.Sprintf(", plan %s, %s", terminal.EscapeLine(u.Plan), terminal.EscapeLine(u.State))
	}
	if u.Expires > 0 {
		line += ", ends " + time.Unix(u.Expires, 0).UTC().Format(time.DateOnly)
	}
	return line
}

func auditLine(id, result, file string) string {
	return fmt.Sprintf("%s\t%s\t%s", id, terminal.EscapeLine(result), file)
}

func drainLine(u wire.Usage) string {
	return fmt.Sprintf("%s: %d blobs of %d bytes remain", terminal.EscapeLine(u.State), u.Blobs, u.Used)
}
