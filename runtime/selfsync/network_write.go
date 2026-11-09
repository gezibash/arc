package selfsync

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/sdk/storage"
)

// put is one piece or blob to put on one side.
type put struct {
	side int
	blob storage.Blob
}

// write puts each record that the storage citizens lack. A new record gets
// a new seal: t pieces on the t active storage citizens with a pipe that
// rank highest for its blob. A blob that a storage citizen already holds
// is left to repair. A storage citizen with an older app gets whole blobs
// of format 1.
func (r *run) write(ctx context.Context) error {
	s := r.count()
	var pieceSides []int
	for i, side := range r.sides {
		if side.Active && !r.old[i] && r.open[i] {
			pieceSides = append(pieceSides, i)
		}
	}
	r.report.K, r.report.T = Code(s)
	writers := slices.ContainsFunc(r.sides, func(side Side) bool { return side.Active })
	if !writers {
		return nil
	}
	// Read the store again: a deletion that came in removed records.
	records, err := Records(r.s.Store, r.s.Citizen)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(records, putChunk) {
		var puts []put
		for _, event := range chunk {
			id := r.current.BlobID(event.ID)
			m := r.blobs[id]
			if m == nil {
				held, err := r.heldBack(ctx, event)
				if err != nil {
					return err
				}
				if held {
					continue
				}
			}
			// The body of a wrap is read only for a blob that goes out:
			// a file of 63 MiB is not read on each sync.
			var whole []byte
			sealed := func() ([]byte, bool, error) {
				if whole != nil {
					return whole, true, nil
				}
				body, ok := r.bodyOf(event)
				if !ok {
					return nil, false, nil
				}
				_, blob, err := Seal(ctx, r.current, r.s.Device, event, body)
				whole = blob
				return blob, err == nil, err
			}
			skip := false
			// A storage citizen with an older app gets the whole blob.
			for i, side := range r.sides {
				if !side.Active || !r.old[i] || !r.open[i] {
					continue
				}
				if _, listed := r.states[i].Remote[id]; listed {
					continue
				}
				blob, ok, err := sealed()
				if err != nil {
					return err
				}
				if !ok {
					skip = true
					break
				}
				puts = append(puts, put{side: i, blob: storage.Blob{ID: id, Data: blob}})
			}
			if skip || s == 0 {
				continue
			}
			// A blob with pieces, also lost ones, is for repair. A blob of
			// format 1 still holds the record: a later step moves it to
			// pieces.
			if m != nil && (len(m.old) > 0 || len(m.seals) > 0 || len(m.lost) > 0) {
				continue
			}
			blob, ok, err := sealed()
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			p, err := r.code(blob, id, pieceSides)
			if err != nil {
				return err
			}
			if p != nil {
				r.report.Sent++
				r.written[id] = true
			}
			puts = append(puts, p...)
		}
		if _, err := r.put(ctx, puts); err != nil {
			return err
		}
	}
	return nil
}

// sealRecord seals a record into a new blob with the newest keys.
func (r *run) sealRecord(ctx context.Context, event nostr.Event, body []byte) ([]byte, error) {
	_, blob, err := Seal(ctx, r.current, r.s.Device, event, body)
	return blob, err
}

// bodyOf reads the body of a wrap that names one. It returns nil for a
// record with no body. It returns false, and notes why, for a wrap whose
// body this device lacks: a directory carried the wrap without it. Such a
// record waits; the device that holds the body puts it.
func (r *run) bodyOf(event nostr.Event) ([]byte, bool) {
	ref, ok := draft.Body(event)
	if !ok {
		return nil, true
	}
	if r.s.Bodies == nil {
		r.refuse("%s: the draft has a body, and this device keeps no bodies", event.ID.Hex())
		return nil, false
	}
	body, err := r.s.Bodies.Get(ref.Sum)
	if err != nil {
		r.refuse("%s: the body of the draft is not on this device: %v", event.ID.Hex(), err)
		return nil, false
	}
	return body, true
}

// hasBody says if the device holds the body of a record, or the record
// names none.
func (r *run) hasBody(event nostr.Event) bool {
	ref, ok := draft.Body(event)
	if !ok {
		return true
	}
	return r.s.Bodies != nil && r.s.Bodies.Has(ref.Sum)
}

// code codes a blob with a new seal and the code of S, and makes one
// piece for each of the first t sides of its rank. With fewer than k
// sides, it puts nothing: the record waits for the next sync.
func (r *run) code(blob []byte, id string, sides []int) ([]put, error) {
	k, t := Code(r.count())
	if len(sides) < k {
		r.report.Waiting++
		return nil, nil
	}
	shards, err := Shards(blob, k)
	if err != nil {
		return nil, err
	}
	seal, err := NewSeal()
	if err != nil {
		return nil, err
	}
	h := Header{Blob: id, Seal: seal, K: k, Size: int64(len(blob))}
	rank := r.rankOf(id)
	order := slices.Clone(sides)
	slices.SortFunc(order, func(a, b int) int { return rank[a] - rank[b] })
	var puts []put
	for index, i := range order[:min(t, len(order))] {
		h.Index = index
		tag, data, err := SealPiece(r.current, h, shards[index])
		if err != nil {
			return nil, err
		}
		puts = append(puts, put{side: i, blob: storage.Blob{ID: PieceID(r.idKeys[i], id, seal, index), Data: data, Tag: tag}})
	}
	return puts, nil
}

// put sends the puts to each side, all sides at the same time, and keeps
// what each side stored. ok says, for each put, that the side holds the ID
// now.
func (r *run) put(ctx context.Context, puts []put) ([]bool, error) {
	bySide := make([][]storage.Blob, len(r.sides))
	for _, p := range puts {
		bySide[p.side] = append(bySide[p.side], p.blob)
	}
	results := make([][]storage.PutResult, len(r.sides))
	r.each(func(i int) {
		if len(bySide[i]) == 0 {
			return
		}
		var err error
		results[i], err = r.sides[i].Conn.PutMany(ctx, bySide[i])
		if err != nil {
			r.fail(i, err)
		}
	})
	held := map[[2]string]bool{}
	for i, blobs := range bySide {
		st := &r.states[i]
		for n, res := range results[i] {
			b := blobs[n]
			switch {
			case res.Err != nil:
				r.fail(i, fmt.Errorf("put %s: %w", b.ID, res.Err))
			case res.Status == storage.StatusStored:
				held[[2]string{fmt.Sprint(i), b.ID}] = true
				r.report.Sides[i].Stored++
				st.Remote[b.ID] = true
				if r.repairing {
					r.report.Repair.Up += int64(len(b.Data))
				}
				if b.Tag != nil {
					st.Tags[b.ID] = base64.StdEncoding.EncodeToString(b.Tag)
					st.Proofs[b.ID] = newProof(b.ID, b.Data)
				}
			case res.Status == storage.StatusHeld:
				// Another put of this ID came after the listing. The next
				// listing gives the tag of a piece.
				held[[2]string{fmt.Sprint(i), b.ID}] = true
				r.report.Sides[i].Held++
				if b.Tag == nil {
					st.Remote[b.ID] = true
				}
			case res.Status == storage.StatusTombstoned:
				st.Remote[b.ID] = false
			}
		}
	}
	ok := make([]bool, len(puts))
	for n, p := range puts {
		ok[n] = held[[2]string{fmt.Sprint(p.side), p.blob.ID}]
	}
	return ok, ctx.Err()
}
