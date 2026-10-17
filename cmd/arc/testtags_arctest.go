//go:build arctest

package main

import (
	"strings"

	"fiatjaf.com/nostr"
	"github.com/spf13/cobra"
)

// addTestTags gives arc message send the flag --arctest-tag in a build
// with the tag arctest. The tests of the apps build arc with it, to send a
// message with tags that an honest arc never writes, such as a reply to a
// message of another conversation, or two e tags. The send then skips the
// checks of --reply. A release build has no such flag.
func addTestTags(send *cobra.Command) {
	send.Flags().StringArray("arctest-tag", nil, "add a tag to the rumor, its elements separated by commas (test builds only)")
}

// testTags are the p tags of the recipients and each --arctest-tag, or nil
// when the send has none.
func testTags(command *cobra.Command, to []nostr.PubKey) nostr.Tags {
	extra, _ := command.Flags().GetStringArray("arctest-tag")
	if len(extra) == 0 {
		return nil
	}
	tags := nostr.Tags{}
	for _, pk := range to {
		tags = append(tags, nostr.Tag{"p", pk.Hex()})
	}
	for _, tag := range extra {
		tags = append(tags, strings.Split(tag, ","))
	}
	return tags
}

// testTagsBuild says whether this build has --arctest-tag.
const testTagsBuild = true
