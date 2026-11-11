// Command arc-dm is the direct messages app of a person. It drives
// arc message, so arc signs, seals and sends each message:
//
//	arc-dm [ls]                  the conversations, newest first, with unread counts
//	arc-dm open <peer>           one conversation; it marks its messages read
//	arc-dm send <peer> [text...] send a message; peers separated by commas make a group
//	arc-dm reply <id> [text...]  reply to a message, to each member of its conversation
//	arc-dm read [peer]           mark a conversation read, or all of them
//	arc-dm watch                 print and notify each message as it arrives
//
// The read state stays on this device.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/terminal"
)

const usage = `usage:
  arc-dm [ls] [flags]                  the conversations, newest first
  arc-dm open [flags] <peer>           one conversation; marks it read
  arc-dm send [flags] <peer> [text...] send a message (no text: read standard input)
  arc-dm reply [flags] <id> [text...]  reply to a message, to its conversation
  arc-dm read [flags] [peer]           mark a conversation read, or all of them
  arc-dm watch [flags]                 print and notify each message as it arrives

A peer is a public key or a petname, the start of either, or the end of a
petname: the eight hex digits at the end of a petname come from a hash of
the key, not from its start. Several peers, separated by commas, name a
group: arc-dm send ada,cy hello sends one message that both see, and a
reply goes to each member. The command is ls when none is given. A peer
with no direct conversation that shares one group with you opens that
group. An id is the start of the id of a message, as arc-dm open shows
it beside each message: at least 4 hex digits. Flags can come before or
after the command. After the peer of send, only -reply counts; each other
word is text, and after -- each word is.
arc-dm <command> -h shows the flags of a command.`

func main() {
	commands := map[string]func(context.Context, []string) error{"ls": list, "open": open, "send": send, "reply": reply, "read": read, "watch": watch}
	name, args := "ls", os.Args[1:]
	if len(args) > 0 && slices.Contains([]string{"help", "-h", "-help", "--help"}, args[0]) {
		fmt.Println(usage)
		return
	}
	if i := commandAt(args); i >= 0 {
		name, args = args[i], slices.Concat(args[:i], args[i+1:])
	}
	run, ok := commands[name]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	// arc-dm ends on an interrupt, SIGTERM and SIGHUP, as when its terminal
	// closes. The end of the context ends each arc that it runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	second, interruptAgain := context.WithCancel(context.Background())
	// A second signal ends the work that the first one began, such as the
	// last read of a watch on a hung arc. A third one ends arc-dm as with
	// no handler. The channel listens from the start. A second signal of
	// the same kind that comes at the same instant as the first can still
	// be lost: Go keeps one pending bit for each signal (runtime/sigqueue.go)
	// until its handler takes it, and the kernel keeps one pending standard
	// signal of each kind. Then arc-dm ends when the last read ends, within
	// closeRead.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-signals
		<-signals
		signal.Stop(signals)
		stop()
		interruptAgain()
	}()
	err := run(context.WithValue(ctx, secondSignal{}, second), args)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// An error can hold what arc wrote, which can hold text that a
		// relay chose. arc escaped its own lines: escape the rest, and
		// keep the escapes of arc as they are.
		fmt.Fprintln(os.Stderr, "arc-dm:", terminal.EscapeLineData(err.Error()))
		os.Exit(1)
	}
}

// secondSignal is the key of the context that a second signal ends.
type secondSignal struct{}

// commandAt is the index of the command in args, or -1 for none. The flags
// can come before the command; each flag but -h takes a value, in the next
// argument unless it holds "=".
func commandAt(args []string) int {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			return -1
		case slices.Contains([]string{"-h", "-help", "--help"}, arg):
		case strings.HasPrefix(arg, "-"):
			if !strings.Contains(arg, "=") {
				i++
			}
		default:
			return i
		}
	}
	return -1
}

// parse parses the flags of a command before its first n arguments, and
// returns the arguments. The flag package stops at the first argument that
// is not a flag, so without this arc-dm send ada -reply 3f9a hi would send
// "-reply 3f9a hi" as text.
//
// After the n arguments, only the flags named in after count, and the
// first other word starts the text: arc-dm send ada -home is where the
// heart is sends that text, and changes no home. After "--", each word is
// text.
func parse(flags *flag.FlagSet, args []string, n int, after ...string) ([]string, error) {
	out, _, err := parseText(flags, args, n, after...)
	return out, err
}

// parseText is parse, and also reports whether the text came after "--".
func parseText(flags *flag.FlagSet, args []string, n int, after ...string) (out []string, literal bool, err error) {
	for n == 0 || len(out) < n {
		if err := flags.Parse(args); err != nil {
			return nil, false, err
		}
		rest := flags.Args()
		ended := len(rest) < len(args) && args[len(args)-len(rest)-1] == "--"
		if n == 0 || ended || len(rest) == 0 {
			return append(out, rest...), ended, nil
		}
		out = append(out, rest[0])
		args = rest[1:]
	}
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			return append(out, args[1:]...), true, nil
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if !strings.HasPrefix(arg, "-") || !slices.Contains(after, name) {
			break
		}
		if !inline {
			if len(args) < 2 {
				return nil, false, fmt.Errorf("flag needs an argument: %s", arg)
			}
			value, args = args[1], args[1:]
		}
		if err := flags.Set(name, value); err != nil {
			return nil, false, fmt.Errorf("invalid value %q for flag %s: %w", value, arg, err)
		}
		args = args[1:]
	}
	return append(out, args...), false, nil
}

// refuseHelp refuses a text of only -h, -H, --h, --H, -help or --help: a person who
// asks for help must not send the request as a message. After -- it is
// text.
func refuseHelp(words []string, literal bool, command string) error {
	if len(words) != 1 || literal || !slices.Contains([]string{"-h", "-H", "--h", "--H", "-help", "--help"}, words[0]) {
		return nil
	}
	name, _, _ := strings.Cut(command, " ")
	return fmt.Errorf("the text is only %s: for help, run arc-dm %s -h; to send it, write arc-dm %s -- %s", words[0], name, command, words[0])
}

// given reports whether a flag was set, even to an empty value.
func given(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// settings are the flags that each command takes.
type settings struct {
	arc   arcprogram.Arc
	state string
}

func newFlags(name string) (*flag.FlagSet, *settings) {
	flags := flag.NewFlagSet("arc-dm "+name, flag.ContinueOnError)
	s := &settings{arc: arcprogram.Arc{Warnings: os.Stderr}}
	flags.StringVar(&s.arc.Program, "arc", "arc", "the arc program")
	flags.StringVar(&s.arc.Home, "home", "", "the arc home (default: the home that arc picks)")
	flags.StringVar(&s.state, "state", "", "the state directory (default DM_STATE, or ~/.local/state/arc-dm)")
	return flags, s
}

func stamp(at time.Time) string { return at.Local().Format("2006-01-02 15:04") }

// indent puts two spaces before each line of text.
func indent(text string) string { return "  " + strings.ReplaceAll(text, "\n", "\n  ") }

// preview is the first line of text, at most limit characters.
func preview(text string, limit int) string {
	line, _, more := strings.Cut(text, "\n")
	if runes := []rune(line); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	if more {
		return line + " …"
	}
	return line
}
