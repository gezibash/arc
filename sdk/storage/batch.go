package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/terminal"
)

// PutResult is the result of one put of PutMany: a status of Put, or a
// refusal.
type PutResult struct {
	Status string
	Err    error
}

// PutMany gives several blobs, with at most Window puts in flight. Each
// result is for the blob at the same index. A refusal of one put is in
// its result, and the other puts go on. The error says that the pipe
// failed; the results then hold the puts that ended before.
func (s *Conn) PutMany(ctx context.Context, blobs []Blob) ([]PutResult, error) {
	results := make([]PutResult, len(blobs))
	if len(blobs) == 0 {
		return results, nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return results, err
	}
	if window <= 1 {
		for i, b := range blobs {
			status, err := s.put(ctx, b)
			if err != nil && !IsRefused(err, "") {
				return results, err
			}
			results[i] = PutResult{Status: status, Err: err}
		}
		return results, nil
	}

	// One writer sends the bytes, in the order of the send replies. The
	// app gives each run of bytes to the oldest put that it answered send.
	bytesOf := make(chan []byte, window)
	failed := make(chan error, 1)
	go func() {
		for data := range bytesOf {
			if err := direct.Send(s.dc, bytes.NewReader(data), int64(len(data))); err != nil {
				failed <- err
				for range bytesOf {
				}
				return
			}
		}
	}()
	defer close(bytesOf)

	// The window counts puts, and MaxInFlight counts their bytes, so that
	// large blobs do not fill the memory of a small storage citizen.
	inFlight := map[uint64]int{}
	var flying int64
	done := func(n uint64) {
		flying -= int64(len(blobs[inFlight[n]].Data))
		delete(inFlight, n)
	}
	next := 0
	for next < len(blobs) || len(inFlight) > 0 {
		for next < len(blobs) && len(inFlight) < window {
			size := int64(len(blobs[next].Data))
			if len(inFlight) > 0 && flying+size > MaxInFlight {
				break
			}
			s.n++
			if err := s.send(Request{Op: OpPut, N: s.n, ID: blobs[next].ID, Size: size, Tag: tagOf(blobs[next])}); err != nil {
				return results, err
			}
			inFlight[s.n] = next
			flying += size
			next++
		}
		select {
		case err := <-failed:
			return results, err
		default:
		}
		r, err := s.reply(ctx)
		if err != nil && !IsRefused(err, "") {
			return results, err
		}
		i, ok := inFlight[r.N]
		if !ok {
			return results, fmt.Errorf("the storage citizen answered the request %d, which is not in flight", r.N)
		}
		switch {
		case err != nil:
			results[i].Err = err
			done(r.N)
		case r.Status == StatusSend:
			bytesOf <- blobs[i].Data
		default:
			results[i].Status = r.Status
			done(r.N)
		}
	}
	return results, nil
}

// GetMany gets several blobs, MaxBatch IDs in one request. It calls f for
// each ID, in order, with its bytes or a refusal, such as not_found. If f
// returns an error, GetMany reads the rest of the bytes of that request,
// and returns the error.
func (s *Conn) GetMany(ctx context.Context, ids []string, f func(id string, blob []byte, err error) error) error {
	if len(ids) == 0 {
		return nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return err
	}
	if window <= 1 {
		for _, id := range ids {
			blob, err := s.Get(ctx, id)
			if err != nil && !IsRefused(err, "") {
				return err
			}
			if err := f(id, blob, err); err != nil {
				return err
			}
		}
		return nil
	}
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpGet, IDs: chunk})
		if err != nil {
			return err
		}
		var stop error
		for i, item := range r.Items {
			var blob []byte
			var refused error
			if item.Error != "" {
				message := item.Error
				if item.Error == ErrNotFound {
					message = "the account holds no blob " + item.ID
				}
				refused = &Refused{Code: item.Error, Message: message}
			} else if blob, err = s.blob(ctx, item.Size); err != nil {
				return err
			}
			if stop == nil {
				stop = f(chunk[i], blob, refused)
			}
		}
		if stop != nil {
			return stop
		}
	}
	return nil
}

// DeleteMany removes several blobs, MaxBatch IDs in one request. Each item
// is for the ID at the same index: StatusDeleted or StatusAbsent, or an
// error code.
func (s *Conn) DeleteMany(ctx context.Context, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(ids))
	if window <= 1 {
		for _, id := range ids {
			status, err := s.Delete(ctx, id)
			item := Item{ID: id, Status: status}
			if r, ok := errors.AsType[*Refused](err); ok {
				item.Error = r.Code
			} else if err != nil {
				return items, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpDelete, IDs: chunk})
		if err != nil {
			return items, err
		}
		items = append(items, r.Items...)
	}
	return items, nil
}

// Has says if the app announced a feature in its reply to hello. An older
// app announces none.
func (s *Conn) Has(ctx context.Context, feature string) (bool, error) {
	if _, err := s.Window(ctx); err != nil {
		return false, err
	}
	return slices.Contains(s.features, feature), nil
}

// ErrOlderApp says that the storage app does not know a request. It runs
// an older version.
var ErrOlderApp = errors.New("the storage citizen runs an older storage app; it must update")

// Audit asks the app to prove that it holds blobs, MaxBatch IDs in one
// request. Each item is for the ID at the same index: Size and Proof, or
// an error code. The caller compares each Proof with the AuditProof of the
// bytes that it expects. nonce must be NewNonce, new for this audit.
func (s *Conn) Audit(ctx context.Context, ids []string, nonce []byte) ([]Item, error) {
	if ok, err := s.Has(ctx, FeatureAudit); err != nil || !ok {
		if err == nil {
			err = ErrOlderApp
		}
		return nil, err
	}
	items := make([]Item, 0, len(ids))
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpAudit, IDs: chunk, Nonce: hex.EncodeToString(nonce)})
		if err != nil {
			return items, err
		}
		items = append(items, r.Items...)
	}
	return items, nil
}

// Drain sets the account to StateDraining: the app refuses each put, and
// keeps each read, list and delete. off sets it back to StateOpen. The
// result tells what remains.
func (s *Conn) Drain(ctx context.Context, off bool) (Usage, error) {
	if ok, err := s.Has(ctx, FeatureDrain); err != nil || !ok {
		if err == nil {
			err = ErrOlderApp
		}
		return Usage{}, err
	}
	r, err := s.do(ctx, Request{Op: OpDrain, Off: off})
	return Usage{Used: r.Used, Quota: r.Quota, Blobs: r.Blobs, State: r.State, Plan: r.Plan, Expires: r.Expires}, err
}

// batch sends one get, delete or audit with IDs, and checks that the reply has
// one item for each ID, in order.
func (s *Conn) batch(ctx context.Context, req Request) (Reply, error) {
	s.n++
	n := s.n
	req.N = n
	ids := req.IDs
	r, err := s.do(ctx, req)
	if err != nil {
		return r, err
	}
	if r.N != n || len(r.Items) != len(ids) {
		return r, fmt.Errorf("the storage citizen answered %d items for request %d, not %d items for request %d", len(r.Items), r.N, len(ids), n)
	}
	for i, item := range r.Items {
		if item.ID != ids[i] {
			return r, fmt.Errorf("the storage citizen answered %s in place of %s", terminal.EscapeLine(item.ID), ids[i])
		}
	}
	return r, nil
}

// checkIDs refuses a list reply that names an ID that is not a blob ID: the
// storage citizen misbehaves, and the ID could change the terminal of the
// person who reads it (docs/storage/SPEC.md, section 3.3). The error names
// the ID escaped. A delete needs no check: its reply must name the IDs of
// the request.
func checkIDs(entries []Entry) error {
	for _, e := range entries {
		if !IsID(e.ID) {
			return fmt.Errorf("the storage citizen answered with %s, which is not a blob ID", terminal.EscapeLine(e.ID))
		}
	}
	return nil
}
