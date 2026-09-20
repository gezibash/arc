package main

import (
	"fmt"
	"strings"

	"github.com/gezibash/arc/toolbox"
	"github.com/spf13/cobra"
)

func cacheCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "cache",
		Short: "Keep the answers of a command on this machine",
		Long: "A command that names the cache filter keeps a copy of every\n" +
			"record that it shows you, once you turn its cache on. Each record\n" +
			"is sealed to your own key, so the files tell another reader\n" +
			"nothing. Each command holds its own cache.",
	}

	command.AddCommand(
		&cobra.Command{
			Use: "on <command>", Short: "Keep the records of this command from now on",
			Args: cobra.ExactArgs(1), RunE: cacheOn,
		},
		&cobra.Command{
			Use: "off <command>", Short: "Stop keeping the records of this command",
			Args: cobra.ExactArgs(1), RunE: cacheOff,
		},
		&cobra.Command{
			Use: "status [command]", Short: "Say what the cache holds",
			Args: cobra.MaximumNArgs(1), RunE: cacheStatus,
		},
		&cobra.Command{
			Use: "clear [command]", Short: "Remove the records of a command, or of every command",
			Args: cobra.MaximumNArgs(1), RunE: cacheClear,
		},
		&cobra.Command{
			Use: "search [command] <text>", Short: "Read the records that hold this text",
			Args: cobra.MinimumNArgs(1), RunE: cacheSearch,
		},
	)
	return command
}

func openCache(command *cobra.Command) (*settings, *toolbox.Cache, error) {
	held, err := open(command, false)
	if err != nil {
		return nil, nil, err
	}
	return held, &toolbox.Cache{Dir: held.keys.Dir}, nil
}

func cacheOn(command *cobra.Command, args []string) error {
	_, cache, err := openCache(command)
	if err != nil {
		return err
	}
	if err := cache.Enable(args[0]); err != nil {
		return err
	}
	fmt.Printf("the cache of %s is on\n", args[0])
	return nil
}

func cacheOff(command *cobra.Command, args []string) error {
	_, cache, err := openCache(command)
	if err != nil {
		return err
	}
	if err := cache.Disable(args[0]); err != nil {
		return err
	}
	fmt.Printf("the cache of %s is off. The records stay until you clear them\n", args[0])
	return nil
}

func cacheStatus(command *cobra.Command, args []string) error {
	held, cache, err := openCache(command)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		count, err := cache.Count(args[0], held.me.PublicKey)
		if err != nil {
			return err
		}
		fmt.Printf("the cache of %s is %s and holds %d records\n", args[0], state(cache.On(args[0])), count)
		return nil
	}

	total, err := cache.Count("", held.me.PublicKey)
	if err != nil {
		return err
	}
	fmt.Printf("the cache holds %d records for %s\n", total, held.me.Name())

	for _, name := range cache.Commands(held.me.PublicKey) {
		count, err := cache.Count(name, held.me.PublicKey)
		if err != nil {
			return err
		}
		fmt.Printf("  %-12s %-3s %d\n", name, state(cache.On(name)), count)
	}
	return nil
}

func state(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func cacheClear(command *cobra.Command, args []string) error {
	held, cache, err := openCache(command)
	if err != nil {
		return err
	}

	name := ""
	if len(args) == 1 {
		name = args[0]
	}

	count, err := cache.Clear(name, held.me.PublicKey)
	if err != nil {
		return err
	}

	where := "every command"
	if name != "" {
		where = name
	}
	fmt.Printf("removed %d records of %s\n", count, where)
	return nil
}

// cacheSearch reads "arc cache search <command> <text>". Without a command
// of that name, every word is the text, and the search reads every command.
func cacheSearch(command *cobra.Command, args []string) error {
	held, cache, err := openCache(command)
	if err != nil {
		return err
	}

	name, words := "", args
	if len(args) > 1 {
		for _, held := range cache.Commands(held.me.PublicKey) {
			if held == toolbox.NormalizeCommand(args[0]) {
				name, words = args[0], args[1:]
				break
			}
		}
	}

	found, err := cache.Search(name, held.me, strings.Join(words, " "))
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Println("no records hold that text")
		return nil
	}

	for _, record := range found {
		fmt.Println(record.Line())
	}
	return nil
}
