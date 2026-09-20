package direct_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/direct"
)

const peerKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func policy(body string) string {
	return strings.ReplaceAll(body, "PEER", peerKey)
}

func TestReadsAPolicy(t *testing.T) {
	rules, err := direct.DecodePolicy([]byte(policy(`{"version":1,"rules":[
	  {"peer":"PEER","capability":"primary","scheme":"exec","path":"/",
	   "lease_ms":30000,"dial":["203.0.113.4"],
	   "listen":{"bind":"0.0.0.0","address":"203.0.113.9","port":7400}}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("the policy holds %d rules", len(rules))
	}

	rule := rules[0]
	if hex.EncodeToString(rule.Peer) != peerKey || rule.Capability != "primary" {
		t.Errorf("rule = %+v", rule)
	}
	if rule.LeaseMS != 30000 || len(rule.Dial) != 1 || rule.Listen.Port != 7400 {
		t.Errorf("rule = %+v", rule)
	}

	// The lease takes its default when the rule names none.
	rules, err = direct.DecodePolicy([]byte(policy(`{"version":1,"rules":[
	  {"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.4"]}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].LeaseMS != direct.DefaultLeaseMS {
		t.Errorf("lease = %d", rules[0].LeaseMS)
	}
}

func TestRefusesAPolicyThatDoesNotHold(t *testing.T) {
	cases := map[string]string{
		"another version":                 `{"version":2,"rules":[]}`,
		"an unknown field":                `{"version":1,"rules":[],"colour":"red"}`,
		"a key that is short":             `{"version":1,"rules":[{"peer":"abc","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.4"]}]}`,
		"no way to meet":                  `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/"}]}`,
		"a path without a slash":          `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"run","dial":["203.0.113.4"]}]}`,
		"a lease too long":                `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","lease_ms":999999,"dial":["203.0.113.4"]}]}`,
		"an address that is a name":       `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["example.com"]}]}`,
		"a multicast address":             `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["239.0.0.1"]}]}`,
		"a link local address":            `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["169.254.1.1"]}]}`,
		"a hole punch without an address": `{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","hole_punch":true,"listen":{"bind":"0.0.0.0","address":"203.0.113.9","port":0}}]}`,
		"the same rule twice": `{"version":1,"rules":[
		  {"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.4"]},
		  {"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.5"]}]}`,
	}

	for name, body := range cases {
		if _, err := direct.DecodePolicy([]byte(policy(body))); err == nil {
			t.Errorf("%s: the policy passed", name)
		}
	}
}

func TestOnlyANamedAddressIsDialled(t *testing.T) {
	rules, err := direct.DecodePolicy([]byte(policy(`{"version":1,"rules":[
	  {"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.4"]}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	rule := rules[0]

	address, err := rule.Candidate(map[string]any{"host": "203.0.113.4", "port": 7400})
	if err != nil || address != "203.0.113.4:7400" {
		t.Errorf("address = %q, %v", address, err)
	}

	for name, candidate := range map[string]map[string]any{
		"another address": {"host": "203.0.113.9", "port": 7400},
		"a port of zero":  {"host": "203.0.113.4", "port": 0},
		"a name":          {"host": "example.com", "port": 7400},
		"an extra field":  {"host": "203.0.113.4", "port": 7400, "colour": "red"},
	} {
		if _, err := rule.Candidate(candidate); err == nil {
			t.Errorf("%s: the address passed", name)
		}
	}
}

func TestLoadPolicyReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.json")
	body := policy(`{"version":1,"rules":[{"peer":"PEER","capability":"primary","scheme":"exec","path":"/","dial":["203.0.113.4"]}]}`)

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.LoadPolicy(path); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.LoadPolicy(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a file that is not there passed")
	}
}
