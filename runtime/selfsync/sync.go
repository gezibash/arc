package selfsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/sdk/atomicfile"
	"github.com/gezibash/arc/sdk/storage"
)

// Kinds are the kinds of self data. They stay in the local store, and go
// only to storage citizens, as blobs.
var Kinds = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion}

// Filter matches the self data that the citizen key signed: the drafts of
// the format before the data roots, and the drafts of a machine that holds
// the citizen key.
func Filter(me nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: Kinds, Authors: []nostr.PubKey{me}}
}

// Records returns the self data of a citizen in the local store: what the
// citizen key signed, and what a device key signed with a p tag that names
// the citizen.
func Records(s Store, me nostr.PubKey) ([]nostr.Event, error) {
	signed, err := s.Query(Filter(me))
	if err != nil {
		return nil, err
	}
	named, err := s.Query(nostr.Filter{Kinds: Kinds, Tags: nostr.TagMap{"p": {me.Hex()}}})
	if err != nil {
		return nil, err
	}
	seen := make(map[nostr.ID]bool, len(signed))
	for _, event := range signed {
		seen[event.ID] = true
	}
	for _, event := range named {
		if !seen[event.ID] && draft.Owner(event) == me {
			signed = append(signed, event)
		}
	}
	return signed, nil
}

// Store is the local store of the device.
type Store interface {
	Save(nostr.Event) (store.Result, error)
	SaveFrom(nostr.Event, store.Source) (store.Result, error)
	Query(nostr.Filter) ([]nostr.Event, error)
}

// Bodies keeps the encrypted bodies of drafts on the device, by the
// SHA-256 of their bytes. A blob of version 2 carries the body of its wrap.
type Bodies interface {
	Has(sum string) bool
	Get(sum string) ([]byte, error)
	Put(sum string, data []byte) error
}

// Blobs is one open session with a storage citizen. *storage.Conn is one.
// GetMany and PutMany keep several requests in flight on a storage app
// that supports it, and send one request at a time to an older one.
// Pieces says if the app keeps the tags of pieces. Audit returns
// storage.ErrOlderApp if the app has no audits.
type Blobs interface {
	Pieces(ctx context.Context) (bool, error)
	List(ctx context.Context, after uint64) ([]storage.Entry, uint64, error)
	GetMany(ctx context.Context, ids []string, f func(id string, blob []byte, err error) error) error
	PutMany(ctx context.Context, blobs []storage.Blob) ([]storage.PutResult, error)
	DeleteMany(ctx context.Context, ids []string) ([]storage.Item, error)
	Audit(ctx context.Context, ids []string, nonce []byte) ([]storage.Item, error)
}

// putChunk is the count of records that a device seals before it puts
// them.
const putChunk = 64

// Syncer syncs the self data of one citizen on one device.
type Syncer struct {
	Citizen nostr.PubKey
	// Device is the key that signs each change: the device key, or the
	// citizen key on the machine that holds it.
	Device nostr.SecretKey
	Store  Store
	// Bodies keeps the bodies of the drafts that name one.
	Bodies Bodies
	// Newest returns the keys of the newest generation that this device
	// holds. The device seals each new blob with them.
	Newest func(ctx context.Context) (Keys, error)
	// Keys returns the keys of one generation, to open a blob of it. It
	// fails for a generation that this device does not hold.
	Keys func(ctx context.Context, gen uint32) (Keys, error)
	// Devices returns the device list of the citizen, and false when the
	// citizen has none.
	Devices func(ctx context.Context) (devices.List, bool, error)
	// Opener returns the opener of the app whose drafts hold the kind
	// inside, as the k tag of a draft names it, and false for a kind of no
	// app. Repair opens the drafts and checkpoints with it to find the
	// parts that no page names. Nil keeps each part.
	Opener func(kind string) (draft.Opener, bool)
	// State is the directory of the cursor of each storage citizen.
	State string
	// Repair lets the sync audit, rebuild, move and delete pieces
	// (docs/storage/SPEC.md, section 4.6). arc serve repairs on each tick;
	// arc sync only with --repair.
	Repair bool
	// TURN lets repair use a pipe through TURN. By default repair waits
	// for a direct path.
	TURN bool
	// Now gives the time of the audits. Nil is time.Now.
	Now func() time.Time
	// Writing says whether a write of self data runs on this machine now.
	// A repair then keeps each part with no blob, whatever its age. Nil
	// means no write runs.
	Writing func() (bool, error)
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Report says what one sync did.
type Report struct {
	// Received is the number of records that the store took.
	Received int
	// Held is the number of records that the store already held, or held
	// a newer version of.
	Held int
	// Sent is the number of records that the device coded into pieces of
	// a new seal and put.
	Sent int
	// K and T are the code of a new blob: k of t pieces rebuild it.
	K, T int
	// Waiting is the number of records that no storage citizen holds yet,
	// because fewer than k storage citizens answered.
	Waiting int
	// Unreadable holds one reason for each blob that the device lacks and
	// cannot rebuild now: fewer than k of its pieces answered.
	Unreadable []string
	// Short is the number of blobs that the device lacks, and that no
	// listing holds k pieces of one seal of: a deletion that did not end,
	// or lost pieces.
	Short int
	// Repair says what repair did, or what it would do.
	Repair RepairReport
	// Refused holds one reason for each blob or piece that the device did
	// not take.
	Refused []string
	// Sides holds what the sync did with each storage citizen, in the
	// order of the sides.
	Sides []SideReport
	// Kept holds the record IDs that the storage citizens hold at the end
	// of the sync, from their listings: k pieces of one seal, or a whole
	// blob of format 1. Movable reads it.
	Kept map[nostr.ID]bool
}

// SideReport says what one sync did with one storage citizen.
type SideReport struct {
	// Got is the number of pieces and blobs that the device got from it.
	Got int
	// Stored is the number of pieces and blobs that it stored.
	Stored int
	// Held is the number of puts that it answered held.
	Held int
	// Old says that its storage app keeps no tags. The device puts only
	// blobs of format 1 on it.
	Old bool
	// Audited is the number of pieces that the audit checked, and
	// AuditFailed the number that were absent or changed.
	Audited, AuditFailed int
	// Suspect says that 3 or more pieces failed the audits of today.
	Suspect bool
	// FailedToday is the number of pieces that failed the audits of today.
	FailedToday int
	// Empty says that a draining storage citizen holds no piece and no
	// blob now. The user can mark it gone.
	Empty bool
	// Done is the end of the last request to it.
	Done time.Time
	// Err says why the sync with it failed.
	Err error
}

// stateVersion is the version of the state file of a storage citizen.
// Version 2 keeps the tags of the pieces. A device lists again from 0 if
// the file has an older version: an older device does not keep tags.
const stateVersion = 2

// state is what a device remembers of one storage citizen.
type state struct {
	V int `json:"v,omitempty"`
	// Cursor is the last sequence number that the device listed.
	Cursor uint64 `json:"cursor"`
	// Remote holds each ID that the storage citizen listed: true if it holds
	// the blob or the piece, false if it was deleted.
	Remote map[string]bool `json:"remote"`
	// Tags holds the tag of each piece that the storage citizen listed, in
	// base64. An ID with no tag is a blob of format 1.
	Tags map[string]string `json:"tags,omitempty"`
	// Pieces says if the storage app keeps tags, from its last hello.
	Pieces *bool `json:"pieces,omitempty"`
	// Listed says that the device listed the storage citizen to the end
	// once.
	Listed bool `json:"listed,omitempty"`
	// Seen holds the blob IDs that an older device got. A device of this
	// version moves them to the shared file seen-3.json.
	Seen map[string]bool `json:"seen,omitempty"`
	// Answered is the Unix time of the last pipe that opened.
	Answered int64 `json:"answered,omitempty"`
	// SilentSince is the Unix time of the first failed open after the
	// last pipe that opened. It is 0 while the storage citizen answers.
	SilentSince int64 `json:"silent_since,omitempty"`
	// DirectFailed is the Unix time of the last open in which the direct
	// attempts found no path and the TURN server of the relay opened the
	// pipe. It is 0 after a direct path opens.
	DirectFailed int64 `json:"direct_failed,omitempty"`
	// Lost holds each listed piece that failed an audit or did not open.
	// It counts for no seal, and repair replaces it.
	Lost map[string]bool `json:"lost,omitempty"`
	// Proofs holds, for some pieces, a secret nonce and the audit proof
	// of the bytes that the device put or got, in base64. An audit spends
	// it.
	Proofs map[string]string `json:"proofs,omitempty"`
	// Audited is the Unix time of the last audit.
	Audited int64 `json:"audited,omitempty"`
	// AuditDay is the day of FailedToday, in days since 1970 in UTC.
	AuditDay int64 `json:"audit_day,omitempty"`
	// FailedToday is the number of pieces that failed the audits of
	// AuditDay.
	FailedToday int `json:"failed_today,omitempty"`
}

// SilentWarning is the time with no answer after which each sync warns
// that a storage citizen is silent. Only the user marks it gone.
const SilentWarning = 7 * 24 * time.Hour

// Answered records that a pipe to the storage citizen opened at the time at.
func (s *Syncer) Answered(name string, at time.Time) error {
	st, err := s.load(name)
	if err != nil {
		return err
	}
	st.Answered, st.SilentSince = at.Unix(), 0
	return s.save(name, st)
}

// Unanswered records that no pipe to the storage citizen opened at the time
// at. It returns the time since the first failed open after the last
// answer: a device that was off itself does not count that time.
func (s *Syncer) Unanswered(name string, at time.Time) (time.Duration, error) {
	st, err := s.load(name)
	if err != nil {
		return 0, err
	}
	if st.SilentSince == 0 {
		st.SilentSince = at.Unix()
		if err := s.save(name, st); err != nil {
			return 0, err
		}
	}
	return at.Sub(time.Unix(st.SilentSince, 0)), nil
}

// TURNFirstFor is the time after the direct attempts to a storage citizen
// found no path in which each sync opens the pipe to it through the TURN
// server of the relay first (docs/storage/SPEC.md, section 4.6).
const TURNFirstFor = 6 * time.Hour

// TURNFirst gives the end of the time in which a sync at the time at opens
// the pipe to the storage citizen through the TURN server of the relay
// first. It gives the zero time when the sync tries the direct attempts
// first. A clock that went back before the failure ends the time.
func (s *Syncer) TURNFirst(name string, at time.Time) (time.Time, error) {
	st, err := s.load(name)
	if err != nil || st.DirectFailed == 0 {
		return time.Time{}, err
	}
	failed := time.Unix(st.DirectFailed, 0)
	if until := failed.Add(TURNFirstFor); !at.Before(failed) && at.Before(until) {
		return until, nil
	}
	return time.Time{}, nil
}

// DirectPath records the direct attempts of an open at the time at whose
// pipe opened: found says that a direct path opened, and not the TURN
// server of the relay.
func (s *Syncer) DirectPath(name string, at time.Time, found bool) error {
	st, err := s.load(name)
	if err != nil {
		return err
	}
	st.DirectFailed = at.Unix()
	if found {
		st.DirectFailed = 0
	}
	return s.save(name, st)
}

func (s *Syncer) statePath(name string) string {
	return filepath.Join(s.State, name+".json")
}

func (s *Syncer) load(name string) (state, error) {
	st := state{V: stateVersion, Remote: map[string]bool{}, Tags: map[string]string{}, Seen: map[string]bool{},
		Lost: map[string]bool{}, Proofs: map[string]string{}}
	raw, err := os.ReadFile(s.statePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s: %w", s.statePath(name), err)
	}
	if st.V < stateVersion {
		st.V, st.Cursor, st.Remote, st.Tags = stateVersion, 0, nil, nil
	}
	if st.Remote == nil {
		st.Remote = map[string]bool{}
	}
	if st.Tags == nil {
		st.Tags = map[string]string{}
	}
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}
	if st.Lost == nil {
		st.Lost = map[string]bool{}
	}
	if st.Proofs == nil {
		st.Proofs = map[string]string{}
	}
	return st, nil
}

func (s *Syncer) save(name string, st state) error {
	return s.writeJSON(s.statePath(name), st)
}

func (s *Syncer) writeJSON(path string, v any) error {
	if err := os.MkdirAll(s.State, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw, 0o600)
}

// seenPath is the file of the blob IDs that the device got from any
// storage citizen, whatever the store did.
// The record format of the data roots changed the name: an older arc marked
// such blobs as seen when it refused them, so a device fetches each blob
// once more after its update.
// seenPath names the blobs that the device took or refused for good. An
// older device refused each blob of version 2, so this version starts a
// new file: each device gets each blob once more.
func (s *Syncer) seenPath() string { return filepath.Join(s.State, "seen-3.json") }

func (s *Syncer) loadSeen() (map[string]bool, error) {
	seen := map[string]bool{}
	raw, err := os.ReadFile(s.seenPath())
	if errors.Is(err, os.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		return nil, fmt.Errorf("%s: %w", s.seenPath(), err)
	}
	return seen, nil
}

// take opens one blob, checks who made it, and gives the record to the
// store.
func (s *Syncer) take(ctx context.Context, id string, blob []byte, keysOf func(context.Context, uint32) (Keys, error)) (nostr.Event, store.Result, error) {
	gen, err := Gen(blob)
	if err != nil {
		return nostr.Event{}, store.Result{}, err
	}
	keys, err := keysOf(ctx, gen)
	if err != nil {
		return nostr.Event{}, store.Result{}, err
	}
	env, body, err := Open(keys, id, blob)
	if err != nil {
		return nostr.Event{}, store.Result{}, err
	}
	if draft.Owner(env.Record) != s.Citizen {
		return nostr.Event{}, store.Result{}, errors.New("the record is of another citizen")
	}
	if !isSelf(env.Record.Kind) {
		return nostr.Event{}, store.Result{}, fmt.Errorf("the record has kind %d, which is not self data", env.Record.Kind)
	}
	// The store checks the key that signed the record against the device
	// list. A record of a revoked key comes only from a storage citizen,
	// which refused the key after its revocation.
	if body != nil {
		// The body goes to the disk before the wrap that names it. The
		// store can still refuse the wrap; prune removes such a body.
		if s.Bodies == nil {
			return nostr.Event{}, store.Result{}, errors.New("the record has a body, and this device keeps no bodies")
		}
		ref, _ := draft.Body(env.Record)
		if err := s.Bodies.Put(ref.Sum, body); err != nil {
			return nostr.Event{}, store.Result{}, err
		}
	}
	result, err := s.Store.SaveFrom(env.Record, store.StorageCitizen)
	return env.Record, result, err
}

func isSelf(kind nostr.Kind) bool { return slices.Contains(Kinds, kind) }

// Local says if an event is self data that stays in the local store: a
// draft, a checkpoint, a part, or a deletion of them. A device never sends
// it to a relay. It goes only to storage citizens.
func Local(event nostr.Event) bool {
	switch event.Kind {
	case draft.Kind, draft.CheckpointKind, draft.PartKind:
		return true
	case nostr.KindDeletion:
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "k" && slices.Contains([]string{"31234", "1234", "3275"}, tag[1]) {
				return true
			}
		}
	}
	return false
}
