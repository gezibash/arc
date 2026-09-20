package canonical_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/internal/canonical"
)

func TestEncodesTheFormThatArcSigns(t *testing.T) {
	cases := map[string]struct {
		value any
		want  string
	}{
		"keys in byte order": {
			map[string]any{"b": 1, "a": 2, "A": 3},
			`{"A":3,"a":2,"b":1}`,
		},
		"a nested object": {
			map[string]any{"x": map[string]any{"z": "1", "y": "2"}},
			`{"x":{"y":"2","z":"1"}}`,
		},
		"a list keeps its order": {
			map[string]any{"l": []any{"b", "a", 1}},
			`{"l":["b","a",1]}`,
		},
		"the escapes of JSON, and no more": {
			"\x01\b\t\n\f\r\"\\<>&/\u007f",
			`"\u0001\b\t\n\f\r\"\\<>&/` + "\u007f" + `"`,
		},
		"text stays as it is": {"日本 é", `"日本 é"`},
		"an empty object":     {map[string]any{}, `{}`},
		"null":                {nil, `null`},
		"true":                {true, `true`},
	}

	for name, test := range cases {
		got, err := canonical.Encode(test.value)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != test.want {
			t.Errorf("%s: %s, want %s", name, got, test.want)
		}
	}
}

func TestANumberKeepsItsDigits(t *testing.T) {
	value, err := canonical.Decode([]byte(`{"big":1735689600,"neg":-1,"zero":0}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err := canonical.Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"big":1735689600,"neg":-1,"zero":0}` {
		t.Errorf("got %s", got)
	}
}

func TestRefusesWhatHasNoOneSpelling(t *testing.T) {
	cases := map[string]any{
		"a fraction":               json.Number("1.5"),
		"an exponent":              json.Number("1e3"),
		"a float":                  1.5,
		"a channel":                make(chan int),
		"bytes that are not UTF-8": string([]byte{0xff, 0xfe}),
	}

	for name, value := range cases {
		if _, err := canonical.Encode(value); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

func TestDecodeRefusesTrailingBytes(t *testing.T) {
	if _, err := canonical.Decode([]byte(`{} {}`)); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Errorf("error = %v, want the trailing error", err)
	}
}
