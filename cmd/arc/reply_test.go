package main

import (
	"strings"
	"testing"
)

// Only a build with the tag arctest has --arctest-tag. A release build
// refuses it, so no release can send the tags that the tests of the apps
// send. Run with -tags arctest, the test has nothing to prove.
func TestAReleaseBuildHasNoTestTags(t *testing.T) {
	if testTagsBuild {
		t.Skip("a build with the tag arctest has the flag")
	}
	key := strings.Repeat("7a", 32)
	out, err := run(t, t.TempDir(), "", "message", "send", "--arctest-tag", "e,"+key, key, "hi")
	if err == nil || !strings.Contains(err.Error()+out, "unknown flag: --arctest-tag") {
		t.Errorf("a release build took --arctest-tag: %v\n%s", err, out)
	}
}
