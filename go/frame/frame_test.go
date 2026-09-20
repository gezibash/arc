package frame_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gezibash/arc/go/frame"
)

func TestRoundTrip(t *testing.T) {
	id := frame.NewRequestID()
	meta := map[string]any{"method": "POST", "path": "/run"}

	raw, err := frame.Encode(frame.Request, id, meta, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != frame.Request || got.Type.String() != "request" {
		t.Errorf("type = %s", got.Type)
	}
	if !bytes.Equal(got.RequestID, id) {
		t.Error("the request id changed")
	}
	if got.Meta["path"] != "/run" || string(got.Body) != "body" {
		t.Errorf("frame = %+v", got)
	}
}

func TestEmptyMetaAndBody(t *testing.T) {
	raw, err := frame.Encode(frame.Response, frame.NewRequestID(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Meta) != 0 || len(got.Body) != 0 {
		t.Errorf("frame = %+v", got)
	}
}

func TestErrorFrame(t *testing.T) {
	raw, err := frame.EncodeError(frame.NewRequestID(), "denied", "the grant is missing", map[string]any{"path": "/run"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != frame.Error || got.Code() != "denied" || got.Message() != "the grant is missing" {
		t.Errorf("frame = %+v", got)
	}
	if got.Meta["path"] != "/run" {
		t.Error("the extra meta is missing")
	}
}

func TestEventFrame(t *testing.T) {
	raw, err := frame.EncodeEvent("arc.direct.v1", []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != frame.Event || got.Meta["topic"] != "arc.direct.v1" {
		t.Errorf("frame = %+v", got)
	}
}

func TestEncodeRefusesBadInput(t *testing.T) {
	if _, err := frame.Encode(frame.Request, []byte{1, 2}, nil, nil); err == nil {
		t.Error("a short request id encoded")
	}
	if _, err := frame.Encode(frame.Type(99), frame.NewRequestID(), nil, nil); err != frame.ErrUnknownType {
		t.Error("an unknown type encoded")
	}
}

func TestDecodeRefusesBadInput(t *testing.T) {
	good, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"a": "b"}, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}

	version := append([]byte(nil), good...)
	version[0] = 2

	unknownType := append([]byte(nil), good...)
	unknownType[1] = 99

	longMeta := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(longMeta[20:24], 0xFFFF)

	badMeta, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"a": "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copy(badMeta[24:], []byte(`["x"`))

	cases := map[string]struct {
		raw  []byte
		want error
	}{
		"empty":                      {nil, frame.ErrMalformed},
		"another version":            {version, frame.ErrUnknownVersion},
		"an unknown type":            {unknownType, frame.ErrUnknownType},
		"a short header":             {good[:10], frame.ErrMalformed},
		"a long meta":                {longMeta, frame.ErrMalformed},
		"a cut body":                 {good[:len(good)-1], frame.ErrMalformed},
		"meta that is not an object": {badMeta, frame.ErrMalformed},
	}

	for name, test := range cases {
		if _, err := frame.Decode(test.raw); err != test.want {
			t.Errorf("%s: gave %v, want %v", name, err, test.want)
		}
	}
}
