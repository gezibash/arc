package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The provider reads its limits from the environment.
type config struct {
	// Root is the directory of the mailboxes. DM_ROOT, default ~/.arc/dm.
	Root string
	// MailboxBudget is what one mailbox may hold, messages and attachments
	// together. DM_MAILBOX_BUDGET, default 512 MiB.
	MailboxBudget int
	// RetractWindow is how many seconds a sender may retract a message.
	// DM_RETRACT_WINDOW, default 600.
	RetractWindow int
	// MaxAttach is the largest sealed attachment token. DM_MAX_ATTACH,
	// default 6 MiB, which holds a file of 4 MiB.
	MaxAttach int
	// MaxBody is the largest sealed body token. DM_MAX_BODY, default 96 KiB.
	MaxBody int
}

func loadConfig() (*config, error) {
	root := os.Getenv("DM_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".arc", "dm")
	}
	if strings.HasPrefix(root, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "mailboxes"), 0o700); err != nil {
		return nil, err
	}

	return &config{
		Root:          root,
		MailboxBudget: number("DM_MAILBOX_BUDGET", 512*1024*1024),
		RetractWindow: number("DM_RETRACT_WINDOW", 600),
		MaxAttach:     number("DM_MAX_ATTACH", 6*1024*1024),
		MaxBody:       number("DM_MAX_BODY", 98_304),
	}, nil
}

func number(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
