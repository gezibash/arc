package selfsync

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/gezibash/arc/sdk/storage"
)

// runJobs gets what the jobs need, puts their pieces, and deletes the
// pieces that each job drops once all its puts hold.
func (r *run) runJobs(ctx context.Context, jobs []*job) error {
	if err := r.getMoves(ctx, jobs); err != nil {
		return err
	}
	var rebuild []*rebuilding
	for _, j := range jobs {
		if len(j.grows) == 0 && !j.reseal {
			continue
		}
		// The device seals its own record again for a new seal, and for a
		// whole copy of k = 1: no download.
		// A wrap whose body the device lacks is got and rebuilt.
		if j.event != nil && (j.reseal || j.sl.h.K == 1) && r.hasBody(*j.event) {
			if body, ok := r.bodyOf(*j.event); ok {
				blob, err := r.sealRecord(*j.event, body)
				if err != nil {
					return err
				}
				if j.reseal || int64(len(blob)) == j.sl.h.Size {
					j.b = &rebuilding{id: j.id, sl: j.sl, blob: blob}
					continue
				}
			}
		}
		j.b = &rebuilding{id: j.id, sl: j.sl, shards: map[int][]byte{}, tried: map[string]bool{}, old: j.whole}
		rebuild = append(rebuild, j.b)
	}
	// A whole copy is coded only if it opens: a storage citizen that gives
	// other bytes must not replace the copies of the others. The device
	// then asks the next storage citizen that holds a copy.
	for len(rebuild) > 0 {
		if err := r.fetch(ctx, rebuild); err != nil {
			return err
		}
		var again []*rebuilding
		for _, b := range rebuild {
			if b.sl != nil || b.blob == nil {
				continue
			}
			if err := r.whole(ctx, b.id, b.blob); err != nil {
				r.refuse("%s: a whole copy of format 1 does not open: %v", b.id, err)
				b.blob = nil
				again = append(again, b)
			}
		}
		rebuild = again
	}
	var puts []put
	for _, j := range jobs {
		if err := r.makePuts(j); err != nil {
			return err
		}
		puts = append(puts, j.puts...)
	}
	ok, err := r.put(ctx, puts)
	if err != nil {
		return err
	}
	at := 0
	var drops []piece
	for _, j := range jobs {
		got := ok[at : at+len(j.puts)]
		at += len(j.puts)
		for n := range j.puts {
			switch {
			case !got[n] || j.reseal:
			case n < j.moved:
				r.report.Repair.Moved++
			default:
				r.report.Repair.Rebuilt++
			}
		}
		// A job that needed bytes and got none, or a put that did not
		// hold, drops nothing: the old pieces still serve.
		needs := len(j.moves) > 0 || len(j.grows) > 0 || j.reseal
		if slices.Contains(got, false) || (needs && len(j.puts) == 0) {
			continue
		}
		if j.reseal && !j.migrate {
			r.report.Repair.Sealed++
		}
		if j.dropsOld && len(j.olds) > 0 {
			r.report.Repair.Coded++
		}
		if j.unnamed {
			r.report.Repair.Unnamed++
		} else if j.replaced {
			r.report.Repair.Replaced++
		}
		drops = append(drops, j.drops...)
	}
	return r.drop(ctx, drops)
}

// learnOn learns about the blobs that the device got before this version,
// on the pipes that repair may use.
func (r *run) learnOn(ctx context.Context, usable func(int) bool) error {
	records, err := Records(r.s.Store, r.s.Citizen)
	if err != nil {
		return err
	}
	local := make(map[string]bool, len(records))
	for _, event := range records {
		local[r.current.BlobID(event.ID)] = true
	}
	r.deleted(records)
	open := slices.Clone(r.open)
	may := make([]bool, len(r.open))
	for i := range r.open {
		may[i] = usable(i)
		r.open[i] = may[i]
	}
	r.repairing = true
	err = r.learn(ctx, local)
	r.repairing = false
	// A side that repair may not use keeps its pipe for the rest of the
	// sync. A side that failed stays closed.
	for i := range r.open {
		if !may[i] {
			r.open[i] = open[i]
		}
	}
	return err
}

// whole checks a whole copy of format 1 before the device codes it: its
// size, its generation, and that it opens under its ID.
func (r *run) whole(ctx context.Context, id string, blob []byte) error {
	if !storage.GoodSize(int64(len(blob))) {
		return fmt.Errorf("a blob of %d bytes", len(blob))
	}
	gen, err := Gen(blob)
	if err != nil {
		return err
	}
	keys, err := r.keysOf(ctx, gen)
	if err != nil {
		return err
	}
	_, _, err = Open(keys, id, blob)
	return err
}

// getMoves gets the piece of each move from its draining side, and keeps
// it only if it opens. A piece that does not open is lost.
func (r *run) getMoves(ctx context.Context, jobs []*job) error {
	asks := make([][]*move, len(r.sides))
	for _, j := range jobs {
		for n := range j.moves {
			mv := &j.moves[n]
			asks[mv.from.side] = append(asks[mv.from.side], mv)
		}
	}
	got := make([]map[string][]byte, len(r.sides))
	r.each(func(i int) {
		if len(asks[i]) == 0 {
			return
		}
		ids := make([]string, len(asks[i]))
		for n, mv := range asks[i] {
			ids[n] = mv.from.id
		}
		got[i] = map[string][]byte{}
		err := r.sides[i].Conn.GetMany(ctx, ids, func(id string, data []byte, err error) error {
			if err == nil {
				got[i][id] = data
			} else if storage.IsRefused(err, storage.ErrNotFound) {
				got[i][id] = nil
			}
			return nil
		})
		if err != nil {
			r.fail(i, err)
		}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, mvs := range asks {
		for _, mv := range mvs {
			data, ok := got[i][mv.from.id]
			if !ok {
				continue
			}
			r.report.Repair.Down += int64(len(data))
			keys, err := r.keysOf(ctx, binaryGen(mv.from.tag))
			if err == nil {
				_, err = OpenPiece(keys, mv.from.tag, mv.from.h, data)
			}
			if err != nil {
				r.states[i].Lost[mv.from.id] = true
				r.refuse("%s: piece %s of blob %s: %v; the next repair makes it again", r.sides[i].Label, mv.from.id, mv.from.h.Blob, err)
				continue
			}
			mv.data = data
		}
	}
	return nil
}

// makePuts makes the puts of one job: the moves first, then the grows, or
// the pieces of a new seal.
func (r *run) makePuts(j *job) error {
	for _, mv := range j.moves {
		if mv.data == nil {
			continue
		}
		h := mv.from.h
		j.puts = append(j.puts, put{side: mv.to, blob: storage.Blob{ID: PieceID(r.idKeys[mv.to], j.id, h.Seal, h.Index), Data: mv.data, Tag: mv.from.tag}})
		j.moved++
	}
	if j.b == nil || j.b.blob == nil {
		return nil
	}
	if j.reseal {
		var writers []int
		for i, side := range r.sides {
			if side.Active && !r.old[i] && r.open[i] {
				writers = append(writers, i)
			}
		}
		puts, err := r.code(j.b.blob, j.id, writers)
		j.puts = append(j.puts, puts...)
		return err
	}
	shards, err := Shards(j.b.blob, j.sl.h.K)
	if err != nil {
		return err
	}
	for _, g := range j.grows {
		h := j.sl.h
		h.Index = g.index
		tag, data, err := SealPiece(r.current, h, shards[g.index])
		if err != nil {
			return err
		}
		j.puts = append(j.puts, put{side: g.to, blob: storage.Blob{ID: PieceID(r.idKeys[g.to], j.id, h.Seal, h.Index), Data: data, Tag: tag}})
	}
	return nil
}

// drop deletes pieces, all sides at the same time.
func (r *run) drop(ctx context.Context, drops []piece) error {
	bySide := make([][]string, len(r.sides))
	for _, p := range drops {
		if !slices.Contains(bySide[p.side], p.id) {
			bySide[p.side] = append(bySide[p.side], p.id)
		}
	}
	items := make([][]storage.Item, len(r.sides))
	r.each(func(i int) {
		if len(bySide[i]) == 0 {
			return
		}
		var err error
		if items[i], err = r.sides[i].Conn.DeleteMany(ctx, bySide[i]); err != nil {
			r.fail(i, err)
		}
	})
	for i, its := range items {
		st := &r.states[i]
		for n, item := range its {
			id := bySide[i][n]
			if item.Error != "" {
				r.refuse("%s: delete %s: %s", r.sides[i].Label, id, item.Error)
				continue
			}
			st.Remote[id] = false
			delete(st.Tags, id)
			delete(st.Lost, id)
			delete(st.Proofs, id)
			r.report.Repair.Deleted++
		}
	}
	return ctx.Err()
}

// drained marks each draining storage citizen that the device listed to
// the end, and that holds no piece and no blob now.
func (r *run) drained() {
	for i, side := range r.sides {
		st := r.states[i]
		if side.Active || !r.open[i] || !st.Listed || r.report.Sides[i].Err != nil {
			continue
		}
		r.report.Sides[i].Empty = !slices.Contains(slices.Collect(maps.Values(st.Remote)), true)
	}
}
