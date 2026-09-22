package keys

import (
	"encoding/hex"
	"testing"
)

// The Elixir implementation named this key. A change to the words or the
// hash renames every citizen, so the name must stay.
func TestAKeyKeepsItsPetname(t *testing.T) {
	public, _ := hex.DecodeString("b5076a8474a832daee4dd5b4040983b6623b5f344aca57d4d6ee4baf3f259e6e")
	if got := Name(public); got != "hale-helmholtz-a7bdc925" {
		t.Errorf("Name = %q, want hale-helmholtz-a7bdc925", got)
	}
	if got := ShortName(public); got != "hale-helmholtz" {
		t.Errorf("ShortName = %q, want hale-helmholtz", got)
	}
}
