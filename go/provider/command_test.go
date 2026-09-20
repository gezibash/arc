package provider_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/provider"
)

func TestSplitMessage(t *testing.T) {
	line, body, hasBody := provider.SplitMessage("send abc\ntoken\nmore")
	if line != "send abc" || body != "token\nmore" || !hasBody {
		t.Errorf("split gave %q %q %v", line, body, hasBody)
	}

	line, body, hasBody = provider.SplitMessage("whoami")
	if line != "whoami" || body != "" || hasBody {
		t.Errorf("split gave %q %q %v", line, body, hasBody)
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		line    string
		args    []string
		options map[string]any
	}{
		{"whoami", []string{"whoami"}, map[string]any{}},
		{"", nil, map[string]any{}},
		{"thread abc --limit 5", []string{"thread", "abc"}, map[string]any{"limit": "5"}},
		{"inbox --unread", []string{"inbox"}, map[string]any{"unread": true}},
		{"inbox --unread true", []string{"inbox"}, map[string]any{"unread": true}},
		{"inbox --unread false", []string{"inbox"}, map[string]any{}},
		{"inbox --unread null", []string{"inbox"}, map[string]any{}},
		{`inbox --unread ""`, []string{"inbox"}, map[string]any{}},
		{`write a --title "A title"`, []string{"write", "a"}, map[string]any{"title": "A title"}},
		{`write a --reply-to abc`, []string{"write", "a"}, map[string]any{"reply_to": "abc"}},
		{`append a 'single quoted'`, []string{"append", "a", "single quoted"}, map[string]any{}},
		{`send "a --word inside"`, []string{"send", "a --word inside"}, map[string]any{}},
		// A JSON string, as the CLI writes one, keeps its newlines and flags.
		{"write a --body \"line one\\nline two --not-a-flag\"", []string{"write", "a"},
			map[string]any{"body": "line one\nline two --not-a-flag"}},
	}

	for _, test := range cases {
		args, options := provider.ParseLine(test.line)

		if !reflect.DeepEqual(args, test.args) {
			t.Errorf("%q gave args %v, want %v", test.line, args, test.args)
		}
		if !reflect.DeepEqual(options, test.options) {
			t.Errorf("%q gave options %v, want %v", test.line, options, test.options)
		}
	}
}

// A body of several megabytes parses in one pass.
func TestParseLineTakesALongValue(t *testing.T) {
	long := strings.Repeat("a", 4*1024*1024)
	_, options := provider.ParseLine(`write a --body "` + long + `"`)

	if got, ok := provider.Option(options, "body"); !ok || len(got) != len(long) {
		t.Errorf("the value is %d characters", len(got))
	}
}

func TestOptionAndFlag(t *testing.T) {
	_, options := provider.ParseLine("inbox --unread --limit 5")

	if got, ok := provider.Option(options, "limit"); !ok || got != "5" {
		t.Errorf("limit = %q", got)
	}
	if _, ok := provider.Option(options, "unread"); ok {
		t.Error("a bare flag read as a string")
	}
	if !provider.Flag(options, "unread") {
		t.Error("the bare flag does not stand")
	}
	if provider.Flag(options, "missing") {
		t.Error("a flag that is not there stands")
	}
}
