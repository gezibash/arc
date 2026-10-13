// Package storage holds what the two ends of the storage app share: the ARC
// call that opens a pipe, the messages on the pipe, and the padding of a
// blob. The pipe itself is the package sdk/direct.
package storage

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

// Open is the body of the ARC call that opens one pipe for one session.
// The manifest command "open" makes it from the Session that the client
// gives. The author of the call names the account: the account key of the
// citizen for this storage citizen, or a device key that the newest grant
// set of an account holds.
type Open struct {
	Version int `json:"v"`
	// Account is the pseudonym of an older client. The storage app does not
	// use it.
	Account string  `json:"account,omitempty"`
	Session Session `json:"session"`
}

// MaxGrants is the largest number of device keys in one grant set. It is
// the largest device list (docs/devices/SPEC.md).
const MaxGrants = 64

// Grants is the body of the ARC call that sets the grant set of an
// account. The account key of the citizen for this storage citizen
// authors the call, so the call is the signed record. The set holds each
// device key that can use the account. A device key that a newer set does
// not hold is revoked.
type Grants struct {
	Version int `json:"v"`
	// Devices are the device keys, in hex.
	Devices []string `json:"grants"`
	// At is the time of the device list that the set follows, in Unix
	// seconds. The app refuses a set that is older than the set it holds.
	At int64 `json:"at"`
}

// Granted is the reply to Grants: the number of device keys in the set
// that the app holds.
type Granted struct {
	Version int `json:"v"`
	Devices int `json:"devices"`
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
	// OpHello asks the app for its version and its features. An older app
	// answers invalid_request.
	OpHello = "hello"
	// OpAudit asks the app to prove that it holds blobs: it answers the
	// AuditProof of each ID for the nonce of the request.
	OpAudit = "audit"
	// OpDrain sets the account to StateDraining, or back to StateOpen with
	// Off. The reply is the reply of a usage request.
	OpDrain = "drain"
)

const (
	// FeatureBatch says that the app takes requests in flight, numbered
	// with n, and a list of IDs in get and delete.
	FeatureBatch = "batch"
	// FeaturePieces says that the app takes a put with a tag, keeps the
	// tag, gives it back in each listing, and takes the sizes of pieces.
	// An older app ignores the tag, so a device asks hello first.
	FeaturePieces = "pieces"
	// Window is the largest number of requests that a client keeps in
	// flight on one pipe: sent, with no final reply yet.
	Window = 32
	// MaxBatch is the largest number of IDs in one get, delete or audit.
	MaxBatch = 256
	// MaxInFlight bounds the bytes of the puts that a client keeps in
	// flight. A put larger than it goes alone.
	MaxInFlight = 128 << 20
	// FeatureAudit says that the app answers OpAudit.
	FeatureAudit = "audit"
	// FeatureDrain says that the app answers OpDrain.
	FeatureDrain = "drain"
)

// Request is one text message from the client on the pipe. A client that
// did not get FeatureBatch from hello sends the next request only after
// the reply to this one. With FeatureBatch, it keeps at most Window
// requests in flight, each with its own N.
type Request struct {
	Op string `json:"op"`
	// N numbers a request in flight. Each reply to it carries the same N.
	// Zero is a client that sends one request at a time.
	N uint64 `json:"n,omitempty"`
	// IDs name the blobs of a get or a delete with FeatureBatch, at most
	// MaxBatch. A request has ID or IDs, not both.
	IDs []string `json:"ids,omitempty"`
	// ID names a blob: 64 hex digits.
	ID string `json:"id,omitempty"`
	// Size is the size of the blob of a put.
	Size int64 `json:"size,omitempty"`
	// Tag is the sealed tag of a piece, in standard base64, TagSize
	// bytes. Only a put of a piece has it. The app keeps it, and cannot
	// open it.
	Tag string `json:"tag,omitempty"`
	// After is the cursor of a list: the app lists the entries with a
	// greater sequence number.
	After uint64 `json:"after,omitempty"`
	// Limit is the largest number of entries of a list.
	Limit int `json:"limit,omitempty"`
	// Nonce is the challenge of an audit: NonceSize random bytes, in hex.
	Nonce string `json:"nonce,omitempty"`
	// Off ends the drain of an account.
	Off bool `json:"off,omitempty"`
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
	// ErrStaleGrants refuses a grant set that is older than the set that
	// the account holds.
	ErrStaleGrants = "stale_grants"
)

// Reply is one text message from the app on the pipe.
type Reply struct {
	// N is the N of the request.
	N      uint64 `json:"n,omitempty"`
	Status string `json:"status,omitempty"`
	// Error is a short code. Message says the same for a person.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	// Seq is the sequence number of the change that a put or a delete made.
	Seq uint64 `json:"seq,omitempty"`
	// Size is the size of the blob of a get, whose bytes follow the reply,
	// or of an audit, which no bytes follow.
	Size int64 `json:"size,omitempty"`
	// Entries and Next are the result of a list. Next is the cursor for the
	// next list.
	Entries []Entry `json:"entries,omitempty"`
	Next    uint64  `json:"next,omitempty"`
	// Used, Quota and Blobs are the result of a usage request. Quota is
	// the quota of the account.
	Used  int64 `json:"used,omitempty"`
	Quota int64 `json:"quota,omitempty"`
	Blobs int   `json:"blobs,omitempty"`
	// State, Plan and Expires are the record of the account in a usage
	// reply. An older storage app sends none of them.
	State   string `json:"state,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	// Version, Features and Window are the reply to hello.
	Version  int      `json:"v,omitempty"`
	Features []string `json:"features,omitempty"`
	Window   int      `json:"window,omitempty"`
	// Proof is the answer to an audit with ID: the AuditProof, in hex.
	Proof string `json:"proof,omitempty"`
	// Items are the results of a get, a delete or an audit with IDs, one for each
	// ID, in order. The bytes of each found blob of a get follow the
	// reply, in the same order.
	Items []Item `json:"items,omitempty"`
}

// Item is the result for one ID of a get, a delete or an audit with IDs.
// A get gives Size, or Error. A delete gives Status and Seq, or Error. An
// audit gives Size and Proof, or Error.
type Item struct {
	ID     string `json:"id"`
	Size   int64  `json:"size,omitempty"`
	Status string `json:"status,omitempty"`
	Seq    uint64 `json:"seq,omitempty"`
	Proof  string `json:"proof,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Entry is the last change of one blob ID.
type Entry struct {
	Seq     uint64 `json:"seq"`
	ID      string `json:"id"`
	Size    int64  `json:"size,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	// Tag is the tag of the put of a piece, in standard base64. A blob of
	// format 1 has none.
	Tag string `json:"tag,omitempty"`
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsID says if id names a blob: 64 lower-case hex digits.
func IsID(id string) bool { return idPattern.MatchString(id) }

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

const (
	// TagSize is the size of the tag of a piece: the generation (4), a
	// nonce (24), and the sealed header of 43 bytes with its AEAD tag (16).
	TagSize = 4 + 24 + 43 + 16
	// PieceOverhead is the bytes of a piece around its shard: the version
	// (1), the generation (4), a nonce (24) and the AEAD tag (16).
	PieceOverhead = 1 + 4 + 24 + 16
	// MaxK is the largest count of pieces that rebuild a blob. A storage
	// list holds at most 16 storage citizens, and k is half of them.
	MaxK = 8
)

// PieceSize is the size of a piece of a blob of size blob, for a code
// that k pieces rebuild: ceil(blob / k) + PieceOverhead.
func PieceSize(blob int64, k int) int64 {
	return (blob+int64(k)-1)/int64(k) + PieceOverhead
}

// GoodPieceSize says if size is the size of a piece: PieceSize of a blob
// size and a k from 1 to MaxK.
func GoodPieceSize(size int64) bool {
	for blob := int64(MinBlob); blob <= MaxBlob; blob <<= 1 {
		for k := 1; k <= MaxK; k++ {
			if PieceSize(blob, k) == size {
				return true
			}
		}
	}
	return false
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

// Blob is one blob or piece of a batch put. A piece has a tag; a blob of
// format 1 has none.
type Blob struct {
	ID   string
	Data []byte
	Tag  []byte
}
