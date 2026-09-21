// Package frame holds the request and response frame of the ARC handler.
//
//	[1 byte]   version
//	[1 byte]   type
//	[2 bytes]  flags
//	[16 bytes] request id
//	[4 bytes]  meta length, big endian
//	[N bytes]  meta, JSON
//	[4 bytes]  body length, big endian
//	[N bytes]  body
package frame

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the frame version that this package writes.
const Version = 1

// RequestIDBytes is the length of a request id.
const RequestIDBytes = 16

// Type names one kind of frame.
type Type uint8

// The frame types.
const (
	Request      Type = 1
	Response     Type = 2
	Error        Type = 3
	StreamOpen   Type = 4
	StreamData   Type = 5
	StreamResize Type = 6
	StreamClose  Type = 7
	StreamExit   Type = 8
	StreamError  Type = 9
	Event        Type = 10
)

var names = map[Type]string{
	Request: "request", Response: "response", Error: "error",
	StreamOpen: "stream_open", StreamData: "stream_data", StreamResize: "stream_resize",
	StreamClose: "stream_close", StreamExit: "stream_exit", StreamError: "stream_error",
	Event: "event",
}

// String returns the name of the frame type.
func (t Type) String() string {
	if name, ok := names[t]; ok {
		return name
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Errors of this package.
var (
	ErrMalformed      = errors.New("frame: malformed frame")
	ErrUnknownVersion = errors.New("frame: unknown version")
	ErrUnknownType    = errors.New("frame: unknown type")
)

// Frame is one decoded frame.
type Frame struct {
	Version   int
	Type      Type
	Flags     uint16
	RequestID []byte
	Meta      map[string]any
	Body      []byte
}

// NewRequestID returns 16 random bytes.
func NewRequestID() []byte {
	id := make([]byte, RequestIDBytes)
	if _, err := rand.Read(id); err != nil {
		panic("frame: the system gave no random bytes: " + err.Error())
	}
	return id
}

// Encode writes one frame.
func Encode(frameType Type, requestID []byte, meta map[string]any, body []byte) ([]byte, error) {
	if _, ok := names[frameType]; !ok {
		return nil, ErrUnknownType
	}
	if len(requestID) != RequestIDBytes {
		return nil, fmt.Errorf("%w: the request id is %d bytes, want %d", ErrMalformed, len(requestID), RequestIDBytes)
	}
	if meta == nil {
		meta = map[string]any{}
	}

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 4+RequestIDBytes+4+len(metaBytes)+4+len(body))
	out = append(out, Version, uint8(frameType), 0, 0)
	out = append(out, requestID...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(metaBytes)))
	out = append(out, metaBytes...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...), nil
}

// EncodeError writes one error frame. The code and the message go in the meta.
func EncodeError(requestID []byte, code, message string, meta map[string]any, body []byte) ([]byte, error) {
	fields := map[string]any{"code": code, "message": message}
	for key, value := range meta {
		fields[key] = value
	}
	return Encode(Error, requestID, fields, body)
}

// EncodeEvent writes one event frame. A provider sends it without a request,
// and the topic names it.
func EncodeEvent(topic string, body []byte, meta map[string]any) ([]byte, error) {
	fields := map[string]any{}
	for key, value := range meta {
		fields[key] = value
	}
	fields["topic"] = topic
	return Encode(Event, NewRequestID(), fields, body)
}

// Decode reads one frame.
func Decode(raw []byte) (*Frame, error) {
	if len(raw) < 1 {
		return nil, ErrMalformed
	}
	if raw[0] != Version {
		return nil, ErrUnknownVersion
	}
	if len(raw) < 4+RequestIDBytes+4 {
		return nil, ErrMalformed
	}

	frameType := Type(raw[1])
	if _, ok := names[frameType]; !ok {
		return nil, ErrUnknownType
	}

	flags := binary.BigEndian.Uint16(raw[2:4])
	requestID := raw[4 : 4+RequestIDBytes]
	rest := raw[4+RequestIDBytes:]

	metaLen := int(binary.BigEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if len(rest) < metaLen+4 {
		return nil, ErrMalformed
	}

	metaBytes := rest[:metaLen]
	rest = rest[metaLen:]
	bodyLen := int(binary.BigEndian.Uint32(rest[:4]))
	body := rest[4:]
	if len(body) != bodyLen {
		return nil, ErrMalformed
	}

	meta, err := decodeMeta(metaBytes)
	if err != nil {
		return nil, err
	}

	return &Frame{
		Version:   Version,
		Type:      frameType,
		Flags:     flags,
		RequestID: requestID,
		Meta:      meta,
		Body:      body,
	}, nil
}

// Code returns the error code of an error frame, and the empty string for any
// other frame.
func (f *Frame) Code() string {
	code, _ := f.Meta["code"].(string)
	return code
}

// Message returns the error message of an error frame.
func (f *Frame) Message() string {
	message, _ := f.Meta["message"].(string)
	return message
}

// decodeMeta reads the meta of a frame that an unknown peer wrote. Anything
// but an object is an error.
func decodeMeta(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var meta map[string]any
	if err := decoder.Decode(&meta); err != nil || meta == nil {
		return nil, ErrMalformed
	}
	if decoder.More() {
		return nil, ErrMalformed
	}
	return meta, nil
}
