// Package strictjson decodes input that must hold exactly one JSON value.
package strictjson

import (
	"encoding/json"
	"errors"
	"io"
)

// ErrTrailing reports data after the JSON value.
var ErrTrailing = errors.New("strictjson: data after the JSON value")

// Decode reads one JSON value into v. An unknown field is an error. Data
// after the value is an error, but white space is not. json.Decoder.More
// does not find a trailing "]" or "}", so this function reads to the end.
func Decode(r io.Reader, v any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrTrailing
	}
	return nil
}
