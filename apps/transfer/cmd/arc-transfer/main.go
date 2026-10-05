// Command arc-transfer runs the transfer app service, and holds the two
// commands of the app that a person runs:
//
//	arc-transfer                 the service, under arc serve
//	arc-transfer send <key> <file>   give a file to a citizen in one command
//	arc-transfer offer <file>        record a file to give, and print its link
//	arc-transfer get <link>      get the file of a link
//	arc-transfer put <key> <file>    give a file to the app of a citizen
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gezibash/arc/apps/transfer/client"
	"github.com/gezibash/arc/apps/transfer/direct"
	"github.com/gezibash/arc/apps/transfer/server"
	"github.com/gezibash/arc/sdk/stdio"
)

const usage = `usage:
  arc-transfer                        the service, under arc serve
  arc-transfer send [flags] <key> <file>...   give files to a citizen in one command
  arc-transfer offer [flags] <file>   record a file to give, and print its link
  arc-transfer get [flags] <link>     get the file of a link
  arc-transfer put [flags] <key> <file>   give a file to the app of a citizen

arc-transfer <command> -h shows the flags of a command.`

func main() {
	if len(os.Args) < 2 {
		stdio.Main("arc-transfer", server.Run)
		return
	}
	commands := map[string]func(context.Context, []string) error{"send": send, "offer": offer, "get": get, "put": put}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[2:])
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "arc-transfer:", err)
		os.Exit(1)
	}
}

// offer records a file as an offer, and prints its link.
func offer(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-transfer offer", flag.ContinueOnError)
	arc := arcFlags(flags)
	state := flags.String("state", "", "the state directory (default TRANSFER_STATE, or ~/.local/state/arc-transfer)")
	var to keyList
	flags.Var(&to, "to", "a public key that can get the file (repeatable; default: each caller)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: arc-transfer offer [flags] <file>")
	}
	sender, err := arc.PublicKey(ctx)
	if err != nil {
		return err
	}
	recorded, err := server.Record(server.StateDir(*state), flags.Arg(0), to)
	if err != nil {
		return err
	}
	fmt.Println(direct.Link(sender, recorded))
	return nil
}

// get gets the file of a link. It prints the path of the file on standard
// output, and the speed on standard error.
func get(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-transfer get", flag.ContinueOnError)
	arc := arcFlags(flags)
	output := flags.String("o", "", "the file to write (default: the name in the link, in this directory)")
	stun := flags.String("stun", os.Getenv("TRANSFER_STUN"), "the STUN server, or none (default TRANSFER_STUN, or "+direct.DefaultSTUN+")")
	hold := flags.Bool("hold", false, "ask the sender to wait in the first attempt, not in the second")
	state := flags.String("state", "", "the state directory, for a file that the sender gave with put (default TRANSFER_STATE, or ~/.local/state/arc-transfer)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: arc-transfer get [flags] <link>")
	}
	result, err := client.Get{
		Arc: *arc, Link: flags.Arg(0), Output: *output, HoldFirst: *hold, Notes: os.Stderr,
		State:   server.StateDir(*state),
		Options: direct.Options{STUN: direct.STUNURL(*stun), Loopback: os.Getenv("TRANSFER_LOOPBACK") != ""},
	}.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Println(result.Output)
	if result.Path == "put" {
		fmt.Fprintln(os.Stderr, "the sender gave the file before, with put")
		return nil
	}
	seconds := result.Elapsed.Seconds()
	fmt.Fprintf(os.Stderr, "%d bytes in %.1f s, %.2f MiB/s, path %s\n", result.Bytes, seconds, float64(result.Bytes)/(1<<20)/seconds, result.Path)
	return nil
}

// put gives a file to the app of a citizen. It prints the link of the file
// on standard output, for a message to the citizen.
func put(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-transfer put", flag.ContinueOnError)
	arc := arcFlags(flags)
	stun := flags.String("stun", os.Getenv("TRANSFER_STUN"), "the STUN server, or none (default TRANSFER_STUN, or "+direct.DefaultSTUN+")")
	hold := flags.Bool("hold", false, "ask the receiver to wait in the first attempt, not in the second")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: arc-transfer put [flags] <key> <file>")
	}
	result, err := client.Put{
		Arc: *arc, To: flags.Arg(0), File: flags.Arg(1), HoldFirst: *hold, Notes: os.Stderr,
		Options: direct.Options{STUN: direct.STUNURL(*stun), Loopback: os.Getenv("TRANSFER_LOOPBACK") != ""},
	}.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Println(result.Link)
	if result.Bytes > 0 {
		seconds := result.Elapsed.Seconds()
		fmt.Fprintf(os.Stderr, "%d bytes in %.1f s, %.2f MiB/s, path %s\n", result.Bytes, seconds, float64(result.Bytes)/(1<<20)/seconds, result.Path)
	}
	return nil
}

// arcFlags adds the flags that name the arc program and the arc home.
func arcFlags(flags *flag.FlagSet) *client.Arc {
	arc := &client.Arc{}
	flags.StringVar(&arc.Program, "arc", "arc", "the arc program")
	flags.StringVar(&arc.Home, "home", "", "the arc home (default: the home that arc picks)")
	return arc
}

// keyList is a flag that takes one public key each time.
type keyList []string

func (k *keyList) String() string { return strings.Join(*k, ",") }

func (k *keyList) Set(value string) error {
	if !direct.IsHex64(value) {
		return errors.New("a public key is 64 hex digits")
	}
	*k = append(*k, value)
	return nil
}
