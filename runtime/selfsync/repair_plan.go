package selfsync

import (
	"slices"

	"fiatjaf.com/nostr"
)

// plan says what one blob needs. usable says which sides repair may use.
// It returns nil if the blob needs nothing, or if repair cannot help it
// now.
func (r *run) plan(id string, m *mapped, event *nostr.Event, usable func(int) bool) *job {
	s := r.count()
	// A blob that this sync coded is new.
	if r.written[id] {
		return nil
	}
	if o, ok := r.olds.Old[id]; ok {
		return r.planReplaced(id, m, o, event, usable)
	}
	if s == 0 {
		return nil
	}
	// A whole copy of format 1 moves to pieces, except on an active
	// storage citizen whose app keeps no tags: it holds whole copies.
	j := &job{id: id, event: event, sl: r.best(m, false)}
	for _, i := range m.old {
		if r.sides[i].Active && r.old[i] {
			continue
		}
		j.migrate = true
		if usable(i) {
			j.olds = append(j.olds, piece{side: i, id: id})
		}
	}
	if !j.migrate && len(m.old) > 0 && len(m.seals) == 0 && len(m.lost) == 0 {
		return nil
	}
	p := &planner{r: r, id: id, m: m, event: event, usable: usable, s: s, j: j, rank: r.rankOf(id)}
	for i := range r.sides {
		if p.writable(i) {
			p.writers = append(p.writers, i)
		}
	}
	p.all = slices.Clone(m.lost)
	for _, sl := range m.seals {
		for _, ps := range sl.pieces {
			p.all = append(p.all, ps...)
		}
	}
	// Rule 2 of section 6.7 of the proposal: no seal has k pieces. Or S
	// fell below the k of the blob.
	if j.sl == nil || s < j.sl.h.K {
		return p.reseal()
	}
	return p.planSeal()
}

// planner plans the repair of one blob: what the steps of plan share.
type planner struct {
	r      *run
	id     string
	m      *mapped
	event  *nostr.Event
	usable func(int) bool
	// s is the number of storage citizens, and j the job so far.
	s    int
	j    *job
	rank map[int]int
	// writers are the sides that take a new piece, and all holds each
	// piece of the blob, also lost ones.
	writers []int
	all     []piece
	// free are the writers that hold no piece of the blob, by rank.
	free []int
}

func (p *planner) byRank(a, b int) int { return p.rank[a] - p.rank[b] }

func (p *planner) writable(i int) bool {
	return p.r.sides[i].Active && !p.r.old[i] && p.usable(i)
}

func (p *planner) drop(ps []piece) {
	for _, pc := range ps {
		if p.usable(pc.side) {
			p.j.drops = append(p.j.drops, pc)
		}
	}
}

// reseal plans a new seal. A new seal needs the record, k pieces of a seal,
// or a whole copy of format 1 to rebuild the blob, and k sides for its
// pieces.
func (p *planner) reseal() *job {
	j := p.j
	k, _ := Code(p.s)
	if j.sl == nil && p.event == nil {
		for _, i := range p.m.old {
			if p.usable(i) {
				j.whole = append(j.whole, i)
			}
		}
	}
	if (p.event == nil && j.sl == nil && len(j.whole) == 0) || len(p.writers) < k {
		return nil
	}
	j.reseal, j.spare, j.moves, j.grows, j.drops = true, -1, nil, nil, nil
	p.drop(p.all)
	// The new seal puts k or more pieces before any delete.
	j.drops, j.dropsOld = append(j.drops, j.olds...), true
	return j
}

// take returns the first free side that has no tombstone for the index of
// the seal.
func (p *planner) take(sl *sealed, index int) (int, bool) {
	for n, i := range p.free {
		if _, listed := p.r.states[i].Remote[PieceID(p.r.idKeys[i], p.id, sl.h.Seal, index)]; listed {
			continue
		}
		p.free = slices.Delete(p.free, n, n+1)
		return i, true
	}
	return 0, false
}

// planSeal plans the repair of the best seal: drain the pieces of
// draining sides, rebuild missing indices, and drop extra pieces.
func (p *planner) planSeal() *job {
	r, j, m := p.r, p.j, p.m
	sl := j.sl
	want := min(p.s, 2*sl.h.K)
	active, extra, draining := p.indices(sl)
	listed := 0
	for index := range active {
		if slices.ContainsFunc(sl.pieces[index], func(pc piece) bool { return r.sides[pc.side].Active && r.open[pc.side] }) {
			listed++
		}
	}
	healthy := len(sl.pieces)
	j.spare = healthy - sl.h.K
	for _, i := range p.writers {
		if !m.sides[i] {
			p.free = append(p.free, i)
		}
	}
	slices.SortFunc(p.free, p.byRank)
	have := len(active)
	// Drain: copy the piece of a draining side to a free active side.
	for _, index := range draining {
		if have >= want {
			break
		}
		from := slices.IndexFunc(sl.pieces[index], func(pc piece) bool { return p.usable(pc.side) })
		if from < 0 {
			continue
		}
		to, ok := p.take(sl, index)
		if !ok {
			continue
		}
		j.moves = append(j.moves, move{from: sl.pieces[index][from], to: to})
		active[index] = true
		have++
	}
	// A storage citizen that is gone, or lost a piece: make a missing
	// index from k pieces.
	for index := 0; index < 2*sl.h.K && have < want && len(p.free) > 0; index++ {
		if active[index] {
			continue
		}
		to, ok := p.take(sl, index)
		if !ok {
			continue
		}
		j.grows = append(j.grows, grow{index: index, to: to})
		active[index] = true
		have++
	}
	// A lost piece on a side that holds no other piece of the blob, and
	// no free side: the same index there has the same ID. A new seal
	// gives new IDs.
	if have < want && len(p.free) == 0 && slices.ContainsFunc(m.lost, func(pc piece) bool { return p.writable(pc.side) && !r.holds(sl, pc.side) }) {
		return p.reseal()
	}
	p.drop(m.lost)
	// A whole copy goes when k indices of the seal are on active storage
	// citizens that listed this sync, or come with the puts of this job.
	if listed+len(j.moves)+len(j.grows) >= sl.h.K {
		j.drops, j.dropsOld = append(j.drops, j.olds...), true
	}
	if have >= want {
		p.drop(extra)
		for _, other := range m.seals {
			if other != sl {
				for _, ps := range other.pieces {
					p.drop(ps)
				}
			}
		}
	}
	if j.work() == 0 {
		return nil
	}
	return j
}

// indices sorts the pieces of the seal. active holds the index of each
// piece on an active storage citizen, on the side of the highest rank.
// Each other piece of the seal is extra: on a draining side, or a second
// copy of one index. draining holds each index that only draining sides
// hold.
func (p *planner) indices(sl *sealed) (map[int]bool, []piece, []int) {
	active := map[int]bool{}
	var extra []piece
	var draining []int
	for index := range 2 * sl.h.K {
		ps := slices.Clone(sl.pieces[index])
		slices.SortFunc(ps, func(a, b piece) int { return p.byRank(a.side, b.side) })
		for _, pc := range ps {
			if p.r.sides[pc.side].Active && !active[index] {
				active[index] = true
				continue
			}
			extra = append(extra, pc)
		}
		if !active[index] && len(ps) > 0 {
			draining = append(draining, index)
		}
	}
	return active, extra, draining
}

// holds says if a side holds a healthy piece of the seal.
func (r *run) holds(sl *sealed, side int) bool {
	for _, ps := range sl.pieces {
		if slices.ContainsFunc(ps, func(p piece) bool { return p.side == side }) {
			return true
		}
	}
	return false
}

// planReplaced plans the deletion of the blob of a record that the store
// replaced or deleted: each piece, also lost ones and those of each seal,
// and each whole copy, on the sides that repair may use. It waits until
// the storage citizens hold the record that replaced or deleted it, so
// that each other device still reads the newest state.
func (r *run) planReplaced(id string, m *mapped, o replaced, event *nostr.Event, usable func(int) bool) *job {
	// The store keeps the record again: the note is wrong.
	if event != nil {
		delete(r.olds.Old, id)
		return nil
	}
	by, err := r.replacer(o)
	if err != nil || by == nil || !r.onStorage(*by) {
		return nil
	}
	j := &job{id: id, replaced: true, unnamed: isUnnamed(*by)}
	all := slices.Clone(m.lost)
	for _, sl := range m.seals {
		for _, ps := range sl.pieces {
			all = append(all, ps...)
		}
	}
	for _, i := range m.old {
		all = append(all, piece{side: i, id: id})
	}
	for _, p := range all {
		if usable(p.side) {
			j.drops = append(j.drops, p)
		}
	}
	if len(j.drops) == 0 {
		return nil
	}
	return j
}
