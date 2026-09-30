package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeAcceptsOneValue(t *testing.T) {
	for _, input := range []string{`{"a":1}`, "{\"a\":1}\n", ` {"a":1}  `} {
		var v struct{ A int }
		if err := Decode(strings.NewReader(input), &v); err != nil || v.A != 1 {
			t.Errorf("Decode(%q) = %v, a=%d; want nil, a=1", input, err, v.A)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"b":2}`, // unknown field
		`{"a":1} ]`,     // stray close bracket
		`{"a":1} }`,     // stray close brace
		`{"a":1} {}`,    // second value
		`{"a":1} x`,     // junk
		``,              // no value
	} {
		var v struct{ A int }
		if err := Decode(strings.NewReader(input), &v); err == nil {
			t.Errorf("Decode(%q) = nil; want an error", input)
		}
	}
}
