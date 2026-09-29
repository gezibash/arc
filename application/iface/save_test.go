package iface

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

const saveManifest = `{"interface":1,"id":"download","shape":"service","title":"Download",
"service":{"method":"GET","path":"/"},"commands":[{"path":[],"summary":"Download",
"args":[{"name":"output","kind":"positional","type":"path","required":true}],
"action":{"call":{"class":"live","body":""}},
"output":{"open":{"parse":"json"},"save":{"to":"{{output}}","field":"payload"}}}]}`

func TestSaveUsesOnlyTheChosenPath(t *testing.T) {
	m, err := Parse([]byte(saveManifest))
	if err != nil {
		t.Fatal(err)
	}
	chosen := filepath.Join(t.TempDir(), "chosen")
	untrusted := filepath.Join(t.TempDir(), "from-reply")
	body, _ := json.Marshal(map[string]string{"output": untrusted, "payload": "downloaded content"})
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: string(body)}}
	var out bytes.Buffer
	err = Run(context.Background(), env, Installed{Manifest: m, Name: "download"}, []string{chosen}, Stdio{In: strings.NewReader(""), Out: &out, Err: &out})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(chosen); err != nil || string(got) != "downloaded content" {
		t.Fatalf("chosen file = %q, %v", got, err)
	}
	if _, err := os.Stat(untrusted); !os.IsNotExist(err) {
		t.Fatalf("reply selected a destination: %v", err)
	}
}

func TestSaveRequiresAnExplicitPathArgument(t *testing.T) {
	for _, to := range []string{"/tmp/fixed", "{{payload}}", "{{output}}.suffix", "{{output|default:/tmp/fixed}}"} {
		text := strings.Replace(saveManifest, `"to":"{{output}}"`, `"to":"`+to+`"`, 1)
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("accepted destination %q", to)
		}
	}
	for _, arg := range []string{`"type":"text"`, `"type":"path","default":"/tmp/fixed"`} {
		if _, err := Parse([]byte(strings.Replace(saveManifest, `"type":"path"`, arg, 1))); err == nil {
			t.Errorf("accepted %s", arg)
		}
	}
}
