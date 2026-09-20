package main

import (
	"fmt"

	"github.com/gezibash/arc/go/bundle"
	"github.com/spf13/cobra"
)

func appsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "apps",
		Short: "Work with provider bundles",
	}

	command.AddCommand(&cobra.Command{
		Use:   "init [directory]",
		Short: "Write a new provider bundle",
		Long: "The bundle holds an Arcfile, a manifest, and a runtime that\n" +
			"answers one message. Serve it with arc serve <directory>.",
		Args: cobra.MaximumNArgs(1),
		RunE: appsInit,
	})
	return command
}

func appsInit(_ *cobra.Command, args []string) error {
	path := "."
	if len(args) == 1 {
		path = args[0]
	}

	files, err := bundle.Init(path)
	if err != nil {
		return err
	}

	fmt.Printf("wrote a bundle in %s\n", files.Root)
	fmt.Printf("  %s\n  %s\n  %s\n", files.Arcfile, files.Manifest, files.Runtime)
	fmt.Printf("\nserve it with: arc serve %s\n", path)
	return nil
}
