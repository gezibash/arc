package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// MaxNote is the largest note of a join, in bytes.
const MaxNote = 140

// The statuses of an account in the reply to a join.
const (
	// StatusAdmitted says that the storage citizen holds an account for the
	// account key.
	StatusAdmitted = "admitted"
	// StatusPending says that the operator did not decide yet. The signer
	// sends the join again later.
	StatusPending = "pending"
)

// The states of an account.
const (
	// StateOpen takes each request.
	StateOpen = "open"
	// StateClosing refuses each put, and keeps reads and deletes until the
	// reads end.
	StateClosing = "closing"
	// StateClosed takes no session.
	StateClosed = "closed"
)

// ErrRefused is the error of a reply to a join that the storage citizen
// refused. It is also the error of a put on a closing account.
const ErrRefused = "refused"

// JoinCall is the body of the ARC call that asks for an account. The
// account key of the citizen for this storage citizen authors the call.
type JoinCall struct {
	Version int  `json:"v"`
	Join    Join `json:"join"`
}

// Join asks for an account.
type Join struct {
	// Invite is the token of an invite link, or empty.
	Invite string `json:"invite,omitempty"`
	// Want is the quota that the citizen asks for, in bytes.
	Want int64 `json:"want,omitempty"`
	// Note is a text for the operator, at most MaxNote bytes.
	Note string `json:"note,omitempty"`
	// Citizen and Sig show the citizen key, by choice: Sig is the BIP-340
	// signature of the citizen key over JoinDigest. Both are empty, or
	// both are set.
	Citizen string `json:"citizen,omitempty"`
	Sig     string `json:"sig,omitempty"`
}

// JoinReply is the reply to a join: an account, or an error.
type JoinReply struct {
	Version int      `json:"v"`
	Account *Account `json:"account,omitempty"`
	// Error is ErrRefused. Message says why, for a person.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// Account is the state of an account in the reply to a join.
type Account struct {
	// Status is StatusAdmitted or StatusPending.
	Status string `json:"status"`
	// Message says more about a pending account.
	Message string `json:"message,omitempty"`
	// The values below are set for an admitted account.
	Quota   int64  `json:"quota,omitempty"`
	Used    int64  `json:"used,omitempty"`
	Plan    string `json:"plan,omitempty"`
	State   string `json:"state,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	// TombstoneDays is how long the storage citizen refuses a put of a
	// deleted ID.
	TombstoneDays int `json:"tombstone_days,omitempty"`
	// ReadDaysAfterClose is how long a closing account keeps its reads.
	ReadDaysAfterClose int `json:"read_days_after_close,omitempty"`
}

// JoinDigest is the message that the citizen key signs to show itself in
// a join: SHA-256("arc-storage-join-v1" || 0x00 || account key || storage
// key), with each key as its 32 bytes.
func JoinDigest(account, storage [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("arc-storage-join-v1"))
	h.Write([]byte{0})
	h.Write(account[:])
	h.Write(storage[:])
	var digest [32]byte
	h.Sum(digest[:0])
	return digest
}

// SignJoin signs JoinDigest with the secret citizen key, and returns the
// signature in hex.
func SignJoin(citizen [32]byte, account, storage [32]byte) (string, error) {
	priv, _ := btcec.PrivKeyFromBytes(citizen[:])
	digest := JoinDigest(account, storage)
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// VerifyJoin checks the citizen key and the signature of a join. The keys
// are in hex.
func VerifyJoin(citizen, sig, account, storage string) error {
	var c, a, s [32]byte
	for _, k := range []struct {
		text string
		out  *[32]byte
	}{{citizen, &c}, {account, &a}, {storage, &s}} {
		raw, err := hex.DecodeString(k.text)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("%q is not a key of 64 hex digits", k.text)
		}
		copy(k.out[:], raw)
	}
	raw, err := hex.DecodeString(sig)
	if err != nil {
		return errors.New("the signature of the citizen key is not hex")
	}
	parsed, err := schnorr.ParseSignature(raw)
	if err != nil {
		return errors.New("the signature of the citizen key is not valid")
	}
	pub, err := schnorr.ParsePubKey(c[:])
	if err != nil {
		return errors.New("the citizen key is not valid")
	}
	digest := JoinDigest(a, s)
	if !parsed.Verify(digest[:], pub) {
		return errors.New("the signature of the citizen key does not match the account key and the storage key")
	}
	return nil
}

// InviteScheme starts an invite link.
const InviteScheme = "storage+arc://"

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsToken says if token is an invite token: 32 lower-case hex digits, 128
// bits.
func IsToken(token string) bool { return tokenPattern.MatchString(token) }

// InviteLink makes the link storage+arc://<storage key>?invite=<token>.
func InviteLink(storage, token string) string {
	return InviteScheme + storage + "?invite=" + token
}

// ParseInvite reads an invite link, or a bare key of a storage citizen.
// It returns the key in hex and the token, which is empty for a bare key.
func ParseInvite(text string) (storage, token string, err error) {
	rest, ok := strings.CutPrefix(text, InviteScheme)
	if !ok {
		key := strings.ToLower(text)
		if !IsHex64(key) {
			return "", "", fmt.Errorf("%q is neither an invite link %s<key>?invite=<token> nor a key of 64 hex digits", text, InviteScheme)
		}
		return key, "", nil
	}
	storage, query, _ := strings.Cut(rest, "?")
	storage = strings.ToLower(strings.TrimSuffix(storage, "/"))
	token, ok = strings.CutPrefix(query, "invite=")
	if !IsHex64(storage) || !ok || !IsToken(token) {
		return "", "", fmt.Errorf("%q is not an invite link %s<key>?invite=<32 hex digits>", text, InviteScheme)
	}
	return storage, token, nil
}

// HumanSize writes a number of bytes for a person, such as 312 MiB or
// 1.5 GiB.
func HumanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n), ""
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < 1024 {
			break
		}
		value, unit = value/1024, u
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + unit
}

// ParseSize reads a number of bytes, such as 4096, 64KiB, 512MiB or 1GiB.
// An empty value is 0.
func ParseSize(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	number, shift := value, 0
	for suffix, s := range map[string]int{"KiB": 10, "MiB": 20, "GiB": 30, "TiB": 40} {
		if rest, ok := strings.CutSuffix(value, suffix); ok {
			number, shift = rest, s
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is not a size such as 4096, 64KiB, 512MiB or 1GiB", value)
	}
	return n << shift, nil
}
