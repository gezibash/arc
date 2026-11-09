package selfsync

import (
	"context"
	"fmt"
	"slices"

	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/sdk/storage"
)

// fetch rebuilds the blobs: it asks the sides for pieces in rounds, until
// each blob has k shards that open, or no more pieces can come. The
// requests to each side of a round go at the same time.
func (r *run) fetch(ctx context.Context, all []*rebuilding) error {
	for {
		asks := make([][]piece, len(r.sides))
		owner := map[string]*rebuilding{}
		for _, b := range all {
			if b.blob != nil {
				continue
			}
			for _, p := range r.next(b) {
				asks[p.side] = append(asks[p.side], p)
				owner[fmt.Sprint(p.side, p.id)] = b
			}
		}
		if !slices.ContainsFunc(asks, func(ps []piece) bool { return len(ps) > 0 }) {
			return nil
		}
		got := make([][][]byte, len(r.sides))
		errs := make([][]error, len(r.sides))
		r.each(func(i int) {
			if len(asks[i]) == 0 {
				return
			}
			ids := make([]string, len(asks[i]))
			for n, p := range asks[i] {
				ids[n] = p.id
			}
			got[i], errs[i] = make([][]byte, len(ids)), make([]error, len(ids))
			n := 0
			err := r.sides[i].Conn.GetMany(ctx, ids, func(_ string, data []byte, err error) error {
				got[i][n], errs[i][n] = data, err
				n++
				return nil
			})
			if err != nil {
				r.fail(i, err)
				got[i] = nil
			}
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		for i, ps := range asks {
			for n, p := range ps {
				if got[i] == nil {
					continue
				}
				b := owner[fmt.Sprint(i, p.id)]
				data, err := got[i][n], errs[i][n]
				if err != nil {
					r.refuse("%s: %s: %v", r.sides[i].Label, p.id, err)
					// A listed piece that the storage citizen does not find
					// is lost.
					if b.sl != nil && storage.IsRefused(err, storage.ErrNotFound) {
						r.states[i].Lost[p.id] = true
					}
					continue
				}
				r.report.Sides[i].Got++
				if r.repairing {
					r.report.Repair.Down += int64(len(data))
				}
				if b.sl == nil {
					b.blob = data
					continue
				}
				keys, err := r.keysOf(ctx, binaryGen(p.tag))
				if err == nil {
					var shard []byte
					if shard, err = OpenPiece(keys, p.tag, p.h, data); err == nil {
						b.shards[p.h.Index] = shard
						r.states[i].Proofs[p.id] = newProof(p.id, data)
					} else {
						r.states[i].Lost[p.id] = true
					}
				}
				if err != nil {
					r.refuse("%s: piece %s of blob %s: %v; the device asks another storage citizen", r.sides[i].Label, p.id, b.id, err)
				}
			}
		}
		for _, b := range all {
			if b.sl == nil || b.blob != nil || len(b.shards) < b.sl.h.K {
				continue
			}
			blob, err := Rebuild(b.sl.h, b.shards)
			if err != nil {
				r.refuse("%s: %v", b.id, err)
				b.shards = map[int][]byte{}
				continue
			}
			b.blob = blob
		}
	}
}

// read gets each blob that the device lacks, and gives its record to the
// store. A wrap whose body the device lacks counts as lacking: its blob
// brings the body.
func (r *run) read(ctx context.Context) error {
	records, err := Records(r.s.Store, r.s.Citizen)
	if err != nil {
		return err
	}
	local := make(map[string]bool, len(records))
	for _, event := range records {
		if r.hasBody(event) {
			local[r.current.BlobID(event.ID)] = true
		}
	}
	r.deleted(records)
	var lack []string
	for id, m := range r.blobs {
		// The blob of a replaced or deleted record is not got again.
		if _, old := r.olds.Old[id]; !old && !local[id] && !r.seen[id] && (len(m.old) > 0 || len(m.seals) > 0) {
			lack = append(lack, id)
		}
	}
	slices.Sort(lack)
	for chunk := range slices.Chunk(lack, getChunk) {
		all, unreadable := r.rebuildable(chunk)
		r.report.Unreadable = append(r.report.Unreadable, unreadable...)
		if err := r.fetch(ctx, all); err != nil {
			return err
		}
		for _, b := range all {
			if b.blob == nil {
				r.report.Unreadable = append(r.report.Unreadable, r.short(b.id, r.blobs[b.id]))
				continue
			}
			if err := r.take(ctx, b.id, b.blob, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// take gives the record of a blob to the store, as section 3.5 says, and
// notes a record that the store replaced or deleted. A blob that the
// device learns about counts in no total of the report.
func (r *run) take(ctx context.Context, id string, blob []byte, learning bool) error {
	if !r.listed {
		// The store checks each record against the device list that
		// this machine keeps. Bring it up to date once, before the first
		// record. Without the signer, the kept list serves.
		if _, _, err := r.s.Devices(ctx); err != nil && ctx.Err() == nil {
			r.refuse("the device list did not come, so the list on this machine serves: %v", err)
		}
		r.listed = true
	}
	record, result, err := r.s.take(ctx, id, blob, r.keysOf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.refuse("%s: %v", id, err)
		r.seen[id] = true
		r.olds.Checked[id] = true
		return nil
	}
	// A device can join the list later, so a record of a key that is not
	// on it gets a new chance on the next sync.
	if result.Outcome == store.Refused && result.Reason == store.ReasonUnlisted {
		if !learning {
			r.refuse("%s: the store refused it: %s", id, result.Reason)
		}
		return nil
	}
	r.seen[id] = true
	if err := r.classify(id, record, result); err != nil {
		return err
	}
	if learning && result.Outcome != store.Stored {
		return nil
	}
	switch result.Outcome {
	case store.Stored:
		r.report.Received++
	case store.Refused:
		r.refuse("%s: the store refused it: %s", id, result.Reason)
	default:
		r.report.Held++
	}
	return nil
}
