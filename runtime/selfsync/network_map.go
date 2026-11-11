package selfsync

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	"fiatjaf.com/nostr"
)

// mapPieces builds the map of the pieces from the listing of each side,
// also of a side with no pipe now. Each device builds it alone; no device
// writes a shared map.
func (r *run) mapPieces(ctx context.Context) error {
	r.blobs = map[string]*mapped{}
	gens := map[uint32]int{}
	for i := range r.sides {
		st := &r.states[i]
		for id, present := range st.Remote {
			if !present {
				continue
			}
			text, ok := st.Tags[id]
			if !ok {
				r.blob(id).old = append(r.blob(id).old, i)
				continue
			}
			tag, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				r.refuse("%s: piece %s: the tag is not base64", r.sides[i].Label, id)
				continue
			}
			gen, err := TagGen(tag)
			if err != nil {
				r.refuse("%s: piece %s: %v", r.sides[i].Label, id, err)
				continue
			}
			keys, err := r.keysOf(ctx, gen)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				gens[gen]++
				continue
			}
			h, err := OpenTag(keys, tag)
			if err != nil {
				r.refuse("%s: piece %s: %v", r.sides[i].Label, id, err)
				continue
			}
			// A storage citizen that moves a tag to another ID gives a
			// piece that the device did not put there.
			if PieceID(r.idKeys[i], h.Blob, h.Seal, h.Index) != id {
				r.refuse("%s: piece %s: the tag names another piece", r.sides[i].Label, id)
				continue
			}
			m := r.blob(h.Blob)
			m.sides[i] = true
			if st.Lost[id] {
				m.lost = append(m.lost, piece{side: i, id: id, tag: tag, h: h})
				continue
			}
			sl := m.seals[h.Seal]
			if sl == nil {
				sl = &sealed{h: h, pieces: map[int][]piece{}}
				m.seals[h.Seal] = sl
			}
			if sl.h.K != h.K || sl.h.Size != h.Size {
				r.refuse("%s: piece %s: the tag names another code than the other pieces of its seal", r.sides[i].Label, id)
				continue
			}
			sl.pieces[h.Index] = append(sl.pieces[h.Index], piece{side: i, id: id, tag: tag, h: h})
		}
	}
	for gen, n := range gens {
		r.refuse("%d pieces of generation %d: %v", n, gen, r.failed[gen])
	}
	return nil
}

func (r *run) blob(id string) *mapped {
	m := r.blobs[id]
	if m == nil {
		m = &mapped{seals: map[SealID]*sealed{}, sides: map[int]bool{}}
		r.blobs[id] = m
	}
	return m
}

// best returns the seal that a device keeps: the seal with the most
// pieces among those with k or more, and the lowest seal of those. Each
// device thus makes the same choice. With open, it counts only the pieces
// on sides with a pipe.
func (r *run) best(m *mapped, open bool) *sealed {
	var out *sealed
	var most int
	for _, sl := range m.seals {
		n := 0
		for _, ps := range sl.pieces {
			if !open || slices.ContainsFunc(ps, func(p piece) bool { return r.open[p.side] }) {
				n++
			}
		}
		if n < sl.h.K {
			continue
		}
		if out == nil || n > most || (n == most && compareSeal(sl.h.Seal, out.h.Seal) < 0) {
			out, most = sl, n
		}
	}
	return out
}

// rebuilding is one blob that the device rebuilds from its pieces.
type rebuilding struct {
	id string
	sl *sealed
	// shards are the shards that opened, by index.
	shards map[int][]byte
	// tried holds the pieces that the device asked for.
	tried map[string]bool
	// old are the sides of a blob of format 1, in the order to try.
	old  []int
	blob []byte
}

// next returns the pieces to ask for: enough to reach k shards, from the
// sides with a pipe, in the rank of the blob. It returns nothing if no
// more pieces can come.
func (r *run) next(b *rebuilding) []piece {
	if b.sl == nil {
		for _, i := range b.old {
			if r.open[i] && !b.tried[fmt.Sprint(i)] {
				b.tried[fmt.Sprint(i)] = true
				return []piece{{side: i, id: b.id}}
			}
		}
		return nil
	}
	need := b.sl.h.K - len(b.shards)
	if need <= 0 {
		return nil
	}
	rank := r.rankOf(b.id)
	var options []piece
	for index, ps := range b.sl.pieces {
		if _, ok := b.shards[index]; ok {
			continue
		}
		for _, p := range ps {
			if r.open[p.side] && !b.tried[p.id] {
				options = append(options, p)
				break
			}
		}
	}
	slices.SortFunc(options, func(a, b piece) int { return rank[a.side] - rank[b.side] })
	if len(options) < need {
		// Ask for each piece left: the ones that open may still be enough
		// with the shards held.
		need = len(options)
	}
	options = options[:need]
	for _, p := range options {
		b.tried[p.id] = true
	}
	return options
}

// rankOf gives the place of each side in the rank of a blob.
func (r *run) rankOf(blob string) map[int]int {
	keys := make([]nostr.PubKey, len(r.sides))
	for i, side := range r.sides {
		keys[i] = side.Key
	}
	place := map[nostr.PubKey]int{}
	for n, key := range r.current.Rank(blob, keys) {
		place[key] = n
	}
	out := make(map[int]int, len(r.sides))
	for i, side := range r.sides {
		out[i] = place[side.Key]
	}
	return out
}

// binaryGen reads the generation of a tag that mapPieces checked.
func binaryGen(tag []byte) uint32 {
	gen, _ := TagGen(tag)
	return gen
}

// allListed says that the device listed each side to the end once, so
// its listings show each piece that the storage citizens hold.
func (r *run) allListed() bool {
	for i := range r.sides {
		if !r.states[i].Listed {
			return false
		}
	}
	return true
}

// rebuildable returns a rebuild for each blob that the pipes can give,
// and a reason for each other blob that a storage citizen that answers
// later can give. It counts each blob that no listing holds k pieces of
// one seal of in Report.Short.
func (r *run) rebuildable(ids []string) (all []*rebuilding, unreadable []string) {
	for _, id := range ids {
		m := r.blobs[id]
		b := &rebuilding{id: id, shards: map[int][]byte{}, tried: map[string]bool{}}
		// A blob of format 1 on a side with a pipe is one get. Else the
		// pieces rebuild it.
		if slices.ContainsFunc(m.old, func(i int) bool { return r.open[i] }) {
			b.old = m.old
		} else if b.sl = r.best(m, true); b.sl == nil {
			// No wait gives k pieces of one seal: a deletion that did not
			// end, or lost pieces. Repair ends both.
			if len(m.old) == 0 && r.best(m, false) == nil && r.allListed() {
				r.report.Short++
			} else {
				unreadable = append(unreadable, r.short(id, m))
			}
			continue
		}
		all = append(all, b)
	}
	return all, unreadable
}

// short says why a blob cannot be rebuilt now.
func (r *run) short(id string, m *mapped) string {
	if len(m.seals) == 0 {
		return id + ": no storage citizen that holds its whole blob gave it"
	}
	k, open := 0, 0
	for _, sl := range m.seals {
		n := 0
		for _, ps := range sl.pieces {
			if slices.ContainsFunc(ps, func(p piece) bool { return r.open[p.side] }) {
				n++
			}
		}
		if n >= open {
			k, open = sl.h.K, n
		}
	}
	return fmt.Sprintf("%s: %d of the %d pieces that rebuild it answered", id, open, k)
}
