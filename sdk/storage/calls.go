package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"

	"github.com/gezibash/arc/sdk/direct"
)

// Put gives one blob. The size of blob must be a power of two, at least
// MinBlob: see Pad. The status is StatusStored,
// StatusHeld or StatusTombstoned.
func (s *Conn) Put(ctx context.Context, id string, blob []byte) (string, error) {
	return s.put(ctx, Blob{ID: id, Data: blob})
}

// tagOf is the tag of a put request: base64 of the tag, or nothing.
func tagOf(b Blob) string {
	if b.Tag == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b.Tag)
}

func (s *Conn) put(ctx context.Context, b Blob) (string, error) {
	blob := b.Data
	r, err := s.do(ctx, Request{Op: OpPut, ID: b.ID, Size: int64(len(blob)), Tag: tagOf(b)})
	if err != nil || r.Status != StatusSend {
		return r.Status, err
	}
	if err := direct.Send(s.dc, bytes.NewReader(blob), int64(len(blob))); err != nil {
		return "", err
	}
	r, err = s.reply(ctx)
	return r.Status, err
}

// List returns the entries after a cursor, and the next cursor. It reads
// each page until the end.
func (s *Conn) List(ctx context.Context, after uint64) ([]Entry, uint64, error) {
	var all []Entry
	for {
		r, err := s.do(ctx, Request{Op: OpList, After: after, Limit: MaxList})
		if err != nil {
			return all, after, err
		}
		if err := checkIDs(r.Entries); err != nil {
			return all, after, err
		}
		all = append(all, r.Entries...)
		if len(r.Entries) == 0 || r.Next <= after {
			return all, after, nil
		}
		after = r.Next
	}
}

// Get returns the bytes of one blob.
func (s *Conn) Get(ctx context.Context, id string) ([]byte, error) {
	r, err := s.do(ctx, Request{Op: OpGet, ID: id})
	if err != nil {
		return nil, err
	}
	return s.blob(ctx, r.Size)
}

// blob reads the bytes of one blob or piece of a get.
func (s *Conn) blob(ctx context.Context, size int64) ([]byte, error) {
	if !GoodSize(size) && !GoodPieceSize(size) {
		return nil, fmt.Errorf("the storage citizen announced a blob of %d bytes", size)
	}
	data := make([]byte, 0, size)
	for int64(len(data)) < size {
		m, err := s.next(ctx)
		if err != nil {
			return nil, err
		}
		if m.IsString || int64(len(data)+len(m.Data)) > size {
			return nil, errors.New("the storage citizen sent other bytes than the size of the blob")
		}
		data = append(data, m.Data...)
	}
	return data, nil
}

// Delete removes one blob. The status is StatusDeleted or
// StatusAbsent.
func (s *Conn) Delete(ctx context.Context, id string) (string, error) {
	r, err := s.do(ctx, Request{Op: OpDelete, ID: id})
	return r.Status, err
}

// Usage is the result of a usage request.
type Usage struct {
	Used, Quota int64
	Blobs       int
	// State, Plan and Expires are empty from an older storage app.
	State, Plan string
	Expires     int64
}

// Usage returns the bytes in use, the quota of the account, the number of
// blobs, and the record of the account.
func (s *Conn) Usage(ctx context.Context) (Usage, error) {
	r, err := s.do(ctx, Request{Op: OpUsage})
	return Usage{Used: r.Used, Quota: r.Quota, Blobs: r.Blobs, State: r.State, Plan: r.Plan, Expires: r.Expires}, err
}

// Window asks the app for its features once, with hello, and returns the
// count of requests that the device keeps in flight: Window or less for an
// app with FeatureBatch, and 1 for an older app.
func (s *Conn) Window(ctx context.Context) (int, error) {
	if s.window > 0 {
		return s.window, nil
	}
	r, err := s.do(ctx, Request{Op: OpHello})
	s.features = r.Features
	switch {
	case IsRefused(err, ErrInvalidRequest):
		s.window = 1
	case err != nil:
		return 0, err
	case slices.Contains(r.Features, FeatureBatch) && r.Window > 1:
		s.window = min(r.Window, Window)
	default:
		s.window = 1
	}
	return s.window, nil
}

// Pieces says if the app keeps the tags of pieces. It asks hello once. An
// older app ignores a tag, so a device puts only blobs of format 1 on it.
func (s *Conn) Pieces(ctx context.Context) (bool, error) {
	return s.Has(ctx, FeaturePieces)
}
