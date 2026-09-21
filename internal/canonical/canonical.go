// Package canonical writes the deterministic JSON that ARC signs.
//
// The rules are short:
//
//   - An object writes its keys in byte order, with no space.
//   - A string writes the escapes of JSON, and nothing more. The bytes <, >,
//     &, /, U+007F, U+2028 and U+2029 stay as they are, and every other
//     implementation of ARC does the same.
//   - A number writes the digits that arrived, so a value survives a decode
//     and an encode without change.
//
// Two implementations that sign the same record must produce the same bytes.
// The encoder therefore refuses a float, which has no one spelling.
package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"
)

// ErrUnsupported reports a value that has no canonical form.
var ErrUnsupported = errors.New("canonical: unsupported value")

// Encode writes one value as canonical JSON.
func Encode(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := encode(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Decode reads JSON into the value tree that Encode accepts. Numbers keep
// their digits.
func Decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("canonical: trailing bytes")
	}
	return value, nil
}

func encode(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case string:
		return encodeString(out, value)
	case json.Number:
		return encodeNumber(out, value.String())
	case int:
		out.WriteString(strconv.FormatInt(int64(value), 10))
	case int64:
		out.WriteString(strconv.FormatInt(value, 10))
	case uint64:
		out.WriteString(strconv.FormatUint(value, 10))
	case []any:
		return encodeList(out, value)
	case map[string]any:
		return encodeObject(out, value)
	case map[string]string:
		return encodeStringObject(out, value)
	default:
		return fmt.Errorf("%w: %T", ErrUnsupported, value)
	}
	return nil
}

func encodeList(out *bytes.Buffer, values []any) error {
	out.WriteByte('[')
	for index, item := range values {
		if index > 0 {
			out.WriteByte(',')
		}
		if err := encode(out, item); err != nil {
			return err
		}
	}
	out.WriteByte(']')
	return nil
}

func encodeObject(out *bytes.Buffer, fields map[string]any) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			out.WriteByte(',')
		}
		if err := encodeString(out, key); err != nil {
			return err
		}
		out.WriteByte(':')
		if err := encode(out, fields[key]); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

func encodeStringObject(out *bytes.Buffer, fields map[string]string) error {
	wider := make(map[string]any, len(fields))
	for key, value := range fields {
		wider[key] = value
	}
	return encodeObject(out, wider)
}

// encodeNumber writes the digits that arrived. A number with a fraction or an
// exponent has no one spelling across languages, so it is an error.
func encodeNumber(out *bytes.Buffer, digits string) error {
	if _, err := strconv.ParseInt(digits, 10, 64); err != nil {
		return fmt.Errorf("%w: the number %s is not a whole number", ErrUnsupported, digits)
	}
	out.WriteString(digits)
	return nil
}

func encodeString(out *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: the string is not UTF-8", ErrUnsupported)
	}

	out.WriteByte('"')
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char == '"':
			out.WriteString(`\"`)
		case char == '\\':
			out.WriteString(`\\`)
		case char == '\b':
			out.WriteString(`\b`)
		case char == '\f':
			out.WriteString(`\f`)
		case char == '\n':
			out.WriteString(`\n`)
		case char == '\r':
			out.WriteString(`\r`)
		case char == '\t':
			out.WriteString(`\t`)
		case char < 0x20:
			fmt.Fprintf(out, `\u%04x`, char)
		default:
			out.WriteByte(char)
		}
	}
	out.WriteByte('"')
	return nil
}
