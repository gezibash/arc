// Command arc-storage runs the storage app service, and holds the commands
// of the owner:
//
//	arc-storage                   the service, under arc serve
//	arc-storage put <file>...     give blobs to the storage citizen
//	arc-storage list              list the changes after a cursor
//	arc-storage get <id>...       get blobs
//	arc-storage delete <id>...    delete blobs
//	arc-storage usage             show the bytes in use and the quota
//	arc-storage audit <file>...   check that the storage citizen holds files
//	arc-storage drain             refuse new puts; show what remains
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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/apps/storage/server"
	"github.com/gezibash/arc/sdk/stdio"
	"github.com/gezibash/arc/sdk/terminal"
)

const usage = `usage:
  arc-storage                          the service, under arc serve
  arc-storage put [flags] <file>...    give blobs to the storage citizen
  arc-storage list [flags]             list the changes after a cursor
  arc-storage get [flags] <id>...      get blobs
  arc-storage delete [flags] <id>...   delete blobs
  arc-storage usage [flags]            show the bytes in use and the quota
  arc-storage audit [flags] <file>...  check that the storage citizen holds files
  arc-storage drain [flags]            refuse new puts; show what remains

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
		"put": put, "list": list, "get": get, "delete": remove, "usage": showUsage, "audit": audit, "drain": drain,
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
		fmt.Fprintln(os.Stderr, errorLine(err))
		os.Exit(1)
	}
}

// errorLine is the line that arc-storage prints for an error. An error can
// hold text that a storage citizen chose, such as the message of a refusal,
// so the line is escaped and stays one line (docs/clients/SPEC.md, section
// 3.9).
func errorLine(err error) string { return "arc-storage: " + terminal.EscapeLine(err.Error()) }

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

// The lines below show what a storage citizen answered, or what a joining
// citizen wrote to the operator. Each field that the other party chose is
// escaped on one line (docs/clients/SPEC.md, section 3.9), so it cannot
// change the terminal or forge a line.
