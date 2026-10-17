//go:build !arctest

package main

import (
	"fiatjaf.com/nostr"
	"github.com/spf13/cobra"
)

// A release build has no --arctest-tag. See testtags_arctest.go.
func addTestTags(*cobra.Command) {}

func testTags(*cobra.Command, []nostr.PubKey) nostr.Tags { return nil }

// testTagsBuild says whether this build has --arctest-tag.
const testTagsBuild = false
