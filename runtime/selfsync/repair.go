package selfsync

import (
	"context"
	"maps"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/sdk/storage"
)

// RepairReport says what repair did in one sync, or what it would do.
type RepairReport struct {
	// Rebuilt is the number of pieces that the device made again from k
	// other pieces, or from its own copy of the record, and put.
	Rebuilt int
	// Moved is the number of pieces that the device copied from a
	// draining storage citizen to an active one.
	Moved int
	// Sealed is the number of records that the device sealed again with a
	// new seal: no seal had k pieces, S fell below the k of the blob, or a
	// lost piece had no other place.
	Sealed int
	// Deleted is the number of pieces that the device deleted: lost,
	// moved, extra, or of an older seal.
	Deleted int
	// Down and Up are the bytes that repair got and put.
	Down, Up int64
	// Pending is the number of blobs that need repair, when the sync does
	// not repair.
	Pending int
	// WaitDirect is the number of blobs whose repair needs a pipe through
	// TURN, which the device does not use for repair by default.
	WaitDirect int
}

// job is the repair of one blob.
type job struct {
	id string
	// sl is the seal that the blob keeps; nil if it gets a new seal.
	sl *sealed
	// spare is the number of pieces that the blob can still lose.
	spare int
	moves []move
	grows []grow
	// reseal gives the blob a new seal and the code of S.
	reseal bool
	// event is the record of the blob, if the store holds it.
	event *nostr.Event
	// drops are the pieces to delete once each put of the job holds.
	drops []piece
	b     *rebuilding
	puts  []put
	// moved is the number of puts that are moves; they come first.
	moved int
}

// move copies one piece from a draining storage citizen to a free one.
type move struct {
	from piece
	to   int
	data []byte
}

// grow makes one missing index of the seal for a free storage citizen.
type grow struct{ index, to int }

func (j *job) work() int {
	n := len(j.moves) + len(j.grows) + len(j.drops)
	if j.reseal {
		n++
	}
	return n
}

// repair plans the repair of each blob with pieces (docs/storage/SPEC.md,
// section 4.6). Without Syncer.Repair it only counts the blobs that need
// it. Repair uses a pipe through TURN only with Syncer.TURN.
func (r *run) repair(ctx context.Context) error {
	records, err := r.s.Store.Query(Filter(r.s.Citizen))
	if err != nil {
		return err
	}
	local := make(map[string]*nostr.Event, len(records))
	for n := range records {
		local[r.current.BlobID(records[n].ID)] = &records[n]
	}
	usable := func(turn bool) func(int) bool {
		return func(i int) bool { return r.open[i] && (turn || !r.sides[i].Relayed) }
	}
	var jobs []*job
	for id, m := range r.blobs {
		j := r.plan(id, m, local[id], usable(r.s.TURN))
		if !r.s.TURN {
			// A plan that may use TURN shows what waits for a direct path.
			if dry := r.plan(id, m, local[id], usable(true)); dry != nil && (j == nil || dry.work() > j.work()) {
				r.report.Repair.WaitDirect++
			}
		}
		if j != nil {
			jobs = append(jobs, j)
		}
	}
	if !r.s.Repair {
		r.report.Repair.Pending = len(jobs)
		return nil
	}
	// The blobs with the fewest spare pieces go first.
	slices.SortFunc(jobs, func(a, b *job) int {
		if a.spare != b.spare {
			return a.spare - b.spare
		}
		return strings.Compare(a.id, b.id)
	})
	// Repair reads and writes only on the pipes that it may use.
	open := slices.Clone(r.open)
	defer func() {
		for i := range r.open {
			r.open[i] = r.open[i] && open[i]
		}
	}()
	for i := range r.open {
		r.open[i] = usable(r.s.TURN)(i)
	}
	r.repairing = true
	defer func() { r.repairing = false }()
	for chunk := range slices.Chunk(jobs, getChunk) {
		if err := r.runJobs(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// plan says what one blob needs. usable says which sides repair may use.
// It returns nil if the blob needs nothing, or if repair cannot help it
// now.
func (r *run) plan(id string, m *mapped, event *nostr.Event, usable func(int) bool) *job {
	s := r.count()
	// A blob of format 1 moves to pieces in a later step. A blob that this
	// sync coded is new.
	if s == 0 || len(m.old) > 0 || r.written[id] {
		return nil
	}
	rank := r.rankOf(id)
	byRank := func(a, b int) int { return rank[a] - rank[b] }
	writable := func(i int) bool { return r.sides[i].Active && !r.old[i] && usable(i) }
	var writers []int
	for i := range r.sides {
		if writable(i) {
			writers = append(writers, i)
		}
	}
	j := &job{id: id, event: event, sl: r.best(m, false)}
	all := slices.Clone(m.lost)
	for _, sl := range m.seals {
		for _, ps := range sl.pieces {
			all = append(all, ps...)
		}
	}
	drop := func(ps []piece) {
		for _, p := range ps {
			if usable(p.side) {
				j.drops = append(j.drops, p)
			}
		}
	}
	reseal := func() *job {
		// A new seal needs the record, or k pieces of a seal to rebuild
		// the blob, and k sides for its pieces.
		k, _ := Code(s)
		if (event == nil && j.sl == nil) || len(writers) < k {
			return nil
		}
		j.reseal, j.spare, j.moves, j.grows, j.drops = true, -1, nil, nil, nil
		drop(all)
		return j
	}
	// Rule 2 of section 6.7 of the proposal: no seal has k pieces. Or S
	// fell below the k of the blob.
	if j.sl == nil || s < j.sl.h.K {
		return reseal()
	}
	sl := j.sl
	want := min(s, 2*sl.h.K)
	// active holds the index of each piece on an active storage citizen,
	// on the side of the highest rank. Each other piece of the seal is
	// extra: on a draining side, or a second copy of one index.
	active := map[int]bool{}
	var extra []piece
	var draining []int
	for index := range 2 * sl.h.K {
		ps := slices.Clone(sl.pieces[index])
		slices.SortFunc(ps, func(a, b piece) int { return byRank(a.side, b.side) })
		for _, p := range ps {
			if r.sides[p.side].Active && !active[index] {
				active[index] = true
				continue
			}
			extra = append(extra, p)
		}
		if !active[index] && len(ps) > 0 {
			draining = append(draining, index)
		}
	}
	healthy := len(sl.pieces)
	j.spare = healthy - sl.h.K
	var free []int
	for _, i := range writers {
		if !m.sides[i] {
			free = append(free, i)
		}
	}
	slices.SortFunc(free, byRank)
	// take returns the first free side that has no tombstone for the
	// index of the seal.
	take := func(index int) (int, bool) {
		for n, i := range free {
			if _, listed := r.states[i].Remote[PieceID(r.idKeys[i], id, sl.h.Seal, index)]; listed {
				continue
			}
			free = slices.Delete(free, n, n+1)
			return i, true
		}
		return 0, false
	}
	have := len(active)
	// Drain: copy the piece of a draining side to a free active side.
	for _, index := range draining {
		if have >= want {
			break
		}
		from := slices.IndexFunc(sl.pieces[index], func(p piece) bool { return usable(p.side) })
		if from < 0 {
			continue
		}
		to, ok := take(index)
		if !ok {
			continue
		}
		j.moves = append(j.moves, move{from: sl.pieces[index][from], to: to})
		active[index] = true
		have++
	}
	// A storage citizen that is gone, or lost a piece: make a missing
	// index from k pieces.
	for index := 0; index < 2*sl.h.K && have < want && len(free) > 0; index++ {
		if active[index] {
			continue
		}
		to, ok := take(index)
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
	if have < want && len(free) == 0 && slices.ContainsFunc(m.lost, func(p piece) bool { return writable(p.side) && !r.holds(sl, p.side) }) {
		return reseal()
	}
	drop(m.lost)
	if have >= want {
		drop(extra)
		for _, other := range m.seals {
			if other != sl {
				for _, ps := range other.pieces {
					drop(ps)
				}
			}
		}
	}
	if j.work() == 0 {
		return nil
	}
	return j
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
		if j.event != nil && (j.reseal || j.sl.h.K == 1) {
			blob, err := r.sealRecord(*j.event)
			if err != nil {
				return err
			}
			if j.reseal || int64(len(blob)) == j.sl.h.Size {
				j.b = &rebuilding{id: j.id, sl: j.sl, blob: blob}
				continue
			}
		}
		j.b = &rebuilding{id: j.id, sl: j.sl, shards: map[int][]byte{}, tried: map[string]bool{}}
		rebuild = append(rebuild, j.b)
	}
	if err := r.fetch(ctx, rebuild); err != nil {
		return err
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
		if j.reseal {
			r.report.Repair.Sealed++
		}
		drops = append(drops, j.drops...)
	}
	return r.drop(ctx, drops)
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
