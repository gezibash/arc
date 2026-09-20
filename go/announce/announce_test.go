package announce_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/go/announce"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/vectors"
)

// The Elixir implementation signed this record. The signature covers the
// canonical JSON of the record, so a verify that passes proves that both
// implementations write the same canonical bytes.
func TestVerifiesAnElixirAnnouncement(t *testing.T) {
	want := vectors.Load(t).Announcement

	signer, err := identity.FromSeedHex(want.SignerSeed)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := announce.Verify(want.Record, time.Unix(want.Now, 0))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	if hex.EncodeToString(entry.PublicKey) != signer.EncodePublicKey() {
		t.Error("the entry names another signer")
	}
	if entry.Name != signer.Name() {
		t.Errorf("name = %s, want %s", entry.Name, signer.Name())
	}
	if len(entry.Capabilities) != 2 {
		t.Fatalf("the entry holds %d capabilities, want 2", len(entry.Capabilities))
	}
	if entry.Capabilities[0].ID != "exec" || entry.Capabilities[0].Channel != "stable" {
		t.Errorf("the first capability is %+v", entry.Capabilities[0])
	}
	if entry.Federation != announce.Local {
		t.Errorf("federation = %s, want local", entry.Federation)
	}
}

// One changed byte of the record breaks the signature. This holds for the
// canonical encoder as well: a different encoder would fail the test above.
func TestRefusesAChangedAnnouncement(t *testing.T) {
	want := vectors.Load(t).Announcement
	at := time.Unix(want.Now, 0)

	changes := map[string]func(map[string]any){
		"another public key": func(r map[string]any) {
			r["public_key"] = strings.Repeat("ab", 32)
		},
		"a longer life": func(r map[string]any) { r["expires_at"] = int64(want.Now + 3600) },
		"another field": func(r map[string]any) { r["extra"] = "x" },
		"no signature":  func(r map[string]any) { delete(r, "signature") },
		"a changed capability": func(r map[string]any) {
			list := r["capabilities"].([]any)
			list[1].(map[string]any)["id"] = "other"
		},
	}

	for name, change := range changes {
		record := copyRecord(want.Record)
		change(record)

		if _, err := announce.Verify(record, at); err == nil {
			t.Errorf("%s: the record still verified", name)
		}
	}
}

func TestRefusesARecordOutOfItsLife(t *testing.T) {
	want := vectors.Load(t).Announcement

	if _, err := announce.Verify(want.Record, time.Unix(want.Now+181, 0)); err == nil {
		t.Error("an expired record verified")
	}
	if _, err := announce.Verify(want.Record, time.Unix(want.Now-31, 0)); err == nil {
		t.Error("a record from the future verified")
	}
	if _, err := announce.Verify(want.Record, time.Unix(want.Now-29, 0)); err != nil {
		t.Errorf("a record inside the skew did not verify: %v", err)
	}
}

func TestCreateAndVerify(t *testing.T) {
	me, _ := identity.Generate()
	now := time.Unix(1735689600, 0)

	record, err := announce.Create(me, []announce.Capability{{ID: "exec", Title: "Run a command"}}, announce.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}

	entry, err := announce.Verify(record, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if entry.ExpiresAt != now.Unix()+180 {
		t.Errorf("expires at %d, want %d", entry.ExpiresAt, now.Unix()+180)
	}
	if entry.Capabilities[0].Title != "Run a command" {
		t.Errorf("the capability is %+v", entry.Capabilities[0])
	}
}

func TestCreateRefusesBadInput(t *testing.T) {
	me, _ := identity.Generate()

	tooMany := make([]announce.Capability, announce.MaxCapabilities+1)
	for index := range tooMany {
		tooMany[index] = announce.Capability{ID: "x"}
	}

	cases := map[string]struct {
		capabilities []announce.Capability
		options      announce.Options
	}{
		"no id":                      {[]announce.Capability{{Title: "x"}}, announce.Options{}},
		"too many":                   {tooMany, announce.Options{}},
		"a long life":                {nil, announce.Options{TTL: 10 * time.Hour}},
		"a summary too long":         {[]announce.Capability{{ID: "x", Summary: strings.Repeat("y", 1025)}}, announce.Options{}},
		"federation without a relay": {nil, announce.Options{Federation: announce.Direct}},
	}

	for name, test := range cases {
		if _, err := announce.Create(me, test.capabilities, test.options); err == nil {
			t.Errorf("%s: the record was created", name)
		}
	}
}

func TestFederatedAnnouncement(t *testing.T) {
	me, _ := identity.Generate()
	relay, _ := identity.Generate()
	other, _ := identity.Generate()
	now := time.Unix(1735689600, 0)

	record, err := announce.Create(me, []announce.Capability{{ID: "exec"}}, announce.Options{
		Now:            now,
		Federation:     announce.Network,
		RelayPublicKey: relay.PublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	entry, err := announce.Verify(record, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if entry.Federation != announce.Network {
		t.Errorf("federation = %s, want network", entry.Federation)
	}
	if !entry.Federatable(relay.PublicKey) {
		t.Error("the record does not leave its home relay")
	}
	if entry.Federatable(other.PublicKey) {
		t.Error("the record leaves a relay that is not its home")
	}
}

func TestMatchesAndSearch(t *testing.T) {
	want := vectors.Load(t).Announcement
	entry, err := announce.Verify(want.Record, time.Unix(want.Now, 0))
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{entry.Name, entry.ShortName, strings.ToUpper(entry.ShortName), hex.EncodeToString(entry.PublicKey)[:8]} {
		if !entry.Matches(query) {
			t.Errorf("the query %q did not resolve", query)
		}
	}
	for _, query := range []string{"", "nobody", "zz"} {
		if entry.Matches(query) {
			t.Errorf("the query %q resolved", query)
		}
	}

	for _, query := range []string{"exec", "run a command", "", "stable exec"} {
		if !entry.SearchMatch(query) {
			t.Errorf("the search %q found nothing", query)
		}
	}
	if entry.SearchMatch("postgres") {
		t.Error("the search found a capability that is not there")
	}
}

func copyRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if list, ok := value.([]any); ok {
			copied := make([]any, len(list))
			for index, item := range list {
				if fields, ok := item.(map[string]any); ok {
					copied[index] = copyRecord(fields)
				} else {
					copied[index] = item
				}
			}
			out[key] = copied
			continue
		}
		out[key] = value
	}
	return out
}
