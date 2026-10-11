// Package wire holds what the two ends of the storage app share: the ARC
// call that opens a pipe, the messages on the pipe, and the padding of a
// blob. The pipe itself is the package sdk/direct.
package wire

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"regexp"

	"github.com/pion/webrtc/v4"
)

const (
	// MinBlob is the size of the smallest blob. Each blob has a size that
	// is a power of two, so the storage citizen sees no exact size.
	MinBlob = 4 << 10
	// MaxBlob is the size of the largest blob.
	MaxBlob = 64 << 20
	// MaxList is the largest number of entries in one list reply.
	MaxList = 1000
)

// Open is the body of the ARC call to the app. It opens one pipe for one
// session. The manifest command "open" makes it from the Session that the
// client gives, and adds the account.
type Open struct {
	Version int `json:"v"`
	// Account is the pseudonym of the owner for this storage citizen: a
	// keyed value of the citizen (interface spec, section 3.7.1). The key
	// of the storage citizen goes into the keyed value, so another storage
	// citizen sees another pseudonym.
	Account string  `json:"account"`
	Session Session `json:"session"`
}

// Session holds the WebRTC offer of the client.
type Session struct {
	// HoldMS is the time that the app waits before it sends its first
	// packet, in milliseconds. The caller then sends first.
	HoldMS int `json:"hold_ms,omitempty"`
	// STUN is the STUN server of the TURN server of a relay, in a relay
	// attempt. See docs/direct/SPEC.md, section 3.6.
	STUN string                    `json:"stun,omitempty"`
	SDP  webrtc.SessionDescription `json:"sdp"`
}

// Answer is the reply to Open.
type Answer struct {
	Version int                       `json:"v"`
	SDP     webrtc.SessionDescription `json:"sdp"`
}

// The operations of a request on the pipe.
const (
	OpPut    = "put"
	OpList   = "list"
	OpGet    = "get"
	OpDelete = "delete"
	OpUsage  = "usage"
)

// Request is one text message from the client on the pipe. The client
// sends the next request only after the reply to this one.
type Request struct {
	Op string `json:"op"`
	// ID names a blob: 64 hex digits.
	ID string `json:"id,omitempty"`
	// Size is the size of the blob of a put.
	Size int64 `json:"size,omitempty"`
	// After is the cursor of a list: the app lists the entries with a
	// greater sequence number.
	After uint64 `json:"after,omitempty"`
	// Limit is the largest number of entries of a list.
	Limit int `json:"limit,omitempty"`
}

// The statuses of a reply.
const (
	// StatusSend asks the client to send the bytes of a put.
	StatusSend = "send"
	// StatusStored says that the app holds the bytes of a put.
	StatusStored = "stored"
	// StatusHeld says that the app holds a blob with this ID already. No
	// bytes go.
	StatusHeld = "held"
	// StatusTombstoned says that the owner deleted this ID in the last 90
	// days. No bytes go.
	StatusTombstoned = "tombstoned"
	// StatusDeleted says that the app removed the blob.
	StatusDeleted = "deleted"
	// StatusAbsent says that the app holds no blob with this ID.
	StatusAbsent = "absent"
)

// The errors of a reply.
const (
	ErrInvalidRequest = "invalid_request"
	ErrBadSize        = "bad_size"
	ErrOverQuota      = "over_quota"
	ErrNotFound       = "not_found"
	ErrFailed         = "storage_failed"
)

// Reply is one text message from the app on the pipe.
type Reply struct {
	Status string `json:"status,omitempty"`
	// Error is a short code. Message says the same for a person.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	// Seq is the sequence number of the change that a put or a delete made.
	Seq uint64 `json:"seq,omitempty"`
	// Size is the size of the blob of a get. Its bytes follow the reply.
	Size int64 `json:"size,omitempty"`
	// Entries and Next are the result of a list. Next is the cursor for the
	// next list.
	Entries []Entry `json:"entries,omitempty"`
	Next    uint64  `json:"next,omitempty"`
	// Used, Quota and Blobs are the result of a usage request.
	Used  int64 `json:"used,omitempty"`
	Quota int64 `json:"quota,omitempty"`
	Blobs int   `json:"blobs,omitempty"`
}

// Entry is the last change of one blob ID.
type Entry struct {
	Seq     uint64 `json:"seq"`
	ID      string `json:"id"`
	Size    int64  `json:"size,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

var (
	idPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	accountPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

// IsID says if id names a blob: 64 lower-case hex digits.
func IsID(id string) bool { return idPattern.MatchString(id) }

// IsAccount says if account is a pseudonym: a keyed value of 22 characters
// of base64url.
func IsAccount(account string) bool { return accountPattern.MatchString(account) }

// IsHex64 says if key is a public key in hex.
func IsHex64(key string) bool {
	b, err := hex.DecodeString(key)
	return err == nil && len(b) == 32
}

// GoodSize says if size is the size of a blob: a power of two from MinBlob
// to MaxBlob.
func GoodSize(size int64) bool {
	return size >= MinBlob && size <= MaxBlob && size&(size-1) == 0
}

// PaddedSize is the size of the blob that Pad makes from n bytes.
func PaddedSize(n int64) int64 {
	need := uint64(n) + 8
	if need <= MinBlob {
		return MinBlob
	}
	return int64(1) << bits.Len64(need-1)
}

// Pad makes a blob from data: the length of data in 8 bytes, big endian,
// then data, then zeros up to the next power of two, at least MinBlob.
// The client encrypts data before Pad. A later client encrypts the padded
// record instead, as docs/proposals/storage.md, section 2.4, says.
func Pad(data []byte) ([]byte, error) {
	size := PaddedSize(int64(len(data)))
	if size > MaxBlob {
		return nil, fmt.Errorf("%d bytes make a blob of %d bytes, and the largest blob is %d bytes", len(data), size, MaxBlob)
	}
	blob := make([]byte, size)
	binary.BigEndian.PutUint64(blob, uint64(len(data)))
	copy(blob[8:], data)
	return blob, nil
}

// Unpad returns the data of a blob that Pad made.
func Unpad(blob []byte) ([]byte, error) {
	if len(blob) < 8 {
		return nil, errors.New("the blob is shorter than its length")
	}
	n := binary.BigEndian.Uint64(blob)
	if n > uint64(len(blob)-8) {
		return nil, errors.New("the length in the blob is larger than the blob")
	}
	return blob[8 : 8+n], nil
}
