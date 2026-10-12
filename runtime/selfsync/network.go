package selfsync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/sdk/storage"
)

// Side is one storage citizen of a sync.
type Side struct {
	// Key is the key of the storage citizen. Its hex names the state file.
	Key nostr.PubKey
	// Label names it in messages, such as its installed name.
	Label string
	// Conn is the open session, or nil if no pipe opened. A side with no
	// pipe still counts: the device keeps its last listing.
	Conn Blobs
	// Active says that the storage list names it active. Only an active
	// storage citizen gets new pieces, and counts in the code of a new
	// blob. A draining one is read.
	Active bool
	// Relayed says that the pipe goes through TURN. A device does not add
	// a missing piece through it: that is repair, and repair waits for a
	// direct path (decision 7 of the storage network proposal).
	Relayed bool
}

// getChunk is the count of blobs that a device rebuilds in one round.
const getChunk = 256

// piece is one listed piece: where it is, and what its tag says.
type piece struct {
	side int
	id   string
	tag  []byte
	h    Header
}

// sealed is the pieces of one seal of one blob, by index.
type sealed struct {
	h      Header
	pieces map[int][]piece
}

// count is the number of indices of the seal that some storage citizen
// lists.
func (s *sealed) count() int { return len(s.pieces) }

// mapped is what the listings say of one blob.
type mapped struct {
	seals map[SealID]*sealed
	// old holds the sides that list a blob of format 1 with this ID.
	old []int
	// sides holds each side that lists a piece of the blob.
	sides map[int]bool
}

// run is one sync over all storage citizens.
type run struct {
	s      *Syncer
	sides  []Side
	states []state
	report Report
	mu     sync.Mutex
	// open says that a side has a pipe that works.
	open []bool
	// old says that the storage app of a side keeps no tags.
	old     []bool
	idKeys  [][]byte
	current Keys
	keys    map[uint32]Keys
	failed  map[uint32]error
	blobs   map[string]*mapped
	seen    map[string]bool
	devices []nostr.PubKey
	listed  bool
}

// SyncAll syncs with each storage citizen at the same time
// (docs/storage/SPEC.md, section 4.1). It lists each one, and builds the
// map of the pieces from the listings. It gets k pieces of one seal of
// each blob that it lacks, rebuilds the blob, and gives the record to the
// store. Then it codes each record that no storage citizen holds into t
// pieces, and puts one piece on each of t active storage citizens. A
// storage citizen whose app keeps no tags gets whole blobs of format 1.
// The error says that the state did not save; the failure of one storage
// citizen is in its SideReport.
func (s *Syncer) SyncAll(ctx context.Context, sides []Side) (Report, error) {
	r := &run{s: s, sides: sides, states: make([]state, len(sides)), open: make([]bool, len(sides)),
		old: make([]bool, len(sides)), idKeys: make([][]byte, len(sides)),
		keys: map[uint32]Keys{}, failed: map[uint32]error{}, blobs: map[string]*mapped{}}
	r.report.Sides = make([]SideReport, len(sides))
	seen, err := s.loadSeen()
	if err != nil {
		return r.report, err
	}
	r.seen = seen
	for i, side := range sides {
		st, err := s.load(side.Key.Hex())
		if err != nil {
			return r.report, err
		}
		for id := range st.Seen {
			r.seen[id] = true
		}
		st.Seen = nil
		r.states[i] = st
		r.old[i] = st.Pieces != nil && !*st.Pieces
	}
	r.current, err = s.Newest(ctx)
	if err != nil {
		return r.report, err
	}
	r.keys[r.current.Gen] = r.current
	for i, side := range sides {
		if r.idKeys[i], err = r.current.PieceIDKey(side.Key); err != nil {
			return r.report, err
		}
	}

	r.list(ctx)
	if err := r.mapPieces(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	if err := r.read(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	if err := r.write(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	return r.report, r.save()
}

func (r *run) save() error {
	var errs []error
	for i, side := range r.sides {
		errs = append(errs, r.s.save(side.Key.Hex(), r.states[i]))
	}
	errs = append(errs, r.s.writeJSON(r.s.seenPath(), r.seen))
	return errors.Join(errs...)
}

// fail ends the sync of one side.
func (r *run) fail(i int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.report.Sides[i].Err == nil {
		r.report.Sides[i].Err = err
	}
	r.open[i] = false
}

func (r *run) done(i int) {
	r.mu.Lock()
	r.report.Sides[i].Done = time.Now()
	r.mu.Unlock()
}

// each runs f for each side with an open pipe, all at the same time.
func (r *run) each(f func(i int)) {
	var wg sync.WaitGroup
	for i := range r.sides {
		if r.open[i] {
			wg.Go(func() { f(i); r.done(i) })
		}
	}
	wg.Wait()
}

// list asks each storage citizen for its features, and lists it from its
// cursor.
func (r *run) list(ctx context.Context) {
	for i, side := range r.sides {
		r.open[i] = side.Conn != nil
	}
	r.each(func(i int) {
		conn, st := r.sides[i].Conn, &r.states[i]
		pieces, err := conn.Pieces(ctx)
		if err != nil {
			r.fail(i, err)
			return
		}
		st.Pieces = &pieces
		r.old[i] = !pieces
		r.report.Sides[i].Old = !pieces
		entries, next, err := conn.List(ctx, st.Cursor)
		for _, e := range entries {
			st.Remote[e.ID] = !e.Deleted
			delete(st.Tags, e.ID)
			if !e.Deleted && e.Tag != "" {
				st.Tags[e.ID] = e.Tag
			}
		}
		st.Cursor = next
		if err != nil {
			r.fail(i, err)
			return
		}
		st.Listed = true
	})
}

// keysOf returns the keys of a generation, and asks for each generation
// once in a sync, also if it fails.
func (r *run) keysOf(ctx context.Context, gen uint32) (Keys, error) {
	if k, ok := r.keys[gen]; ok {
		return k, nil
	}
	if err := r.failed[gen]; err != nil {
		return Keys{}, err
	}
	k, err := r.s.Keys(ctx, gen)
	if err != nil {
		r.failed[gen] = err
		return k, err
	}
	r.keys[gen] = k
	return k, nil
}

// refuse notes one blob or piece that the device does not take.
func (r *run) refuse(format string, args ...any) {
	r.report.Refused = append(r.report.Refused, fmt.Sprintf(format, args...))
}

// mapPieces builds the map of the pieces from the listing of each side,
// also of a side with no pipe now. Each device builds it alone; no device
// writes a shared map.
func (r *run) mapPieces(ctx context.Context) error {
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
			if PieceID(r.idKeys[i], h.Blob, h.Index) != id {
				r.refuse("%s: piece %s: the tag names another piece", r.sides[i].Label, id)
				continue
			}
			m := r.blob(h.Blob)
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
			m.sides[i] = true
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
					continue
				}
				r.report.Sides[i].Got++
				if b.sl == nil {
					b.blob = data
					continue
				}
				keys, err := r.keysOf(ctx, binaryGen(p.tag))
				if err == nil {
					var shard []byte
					if shard, err = OpenPiece(keys, p.tag, p.h, data); err == nil {
						b.shards[p.h.Index] = shard
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

// binaryGen reads the generation of a tag that mapPieces checked.
func binaryGen(tag []byte) uint32 {
	gen, _ := TagGen(tag)
	return gen
}

// read gets each blob that the device lacks, and gives its record to the
// store.
func (r *run) read(ctx context.Context) error {
	records, err := r.s.Store.Query(Filter(r.s.Citizen))
	if err != nil {
		return err
	}
	local := make(map[string]bool, len(records))
	for _, event := range records {
		local[r.current.BlobID(event.ID)] = true
	}
	var lack []string
	for id, m := range r.blobs {
		if !local[id] && !r.seen[id] && (len(m.old) > 0 || len(m.seals) > 0) {
			lack = append(lack, id)
		}
	}
	slices.Sort(lack)
	for chunk := range slices.Chunk(lack, getChunk) {
		var all []*rebuilding
		for _, id := range chunk {
			m := r.blobs[id]
			b := &rebuilding{id: id, shards: map[int][]byte{}, tried: map[string]bool{}}
			// A blob of format 1 on a side with a pipe is one get. Else the
			// pieces rebuild it.
			if slices.ContainsFunc(m.old, func(i int) bool { return r.open[i] }) {
				b.old = m.old
			} else if b.sl = r.best(m, true); b.sl == nil {
				r.report.Unreadable = append(r.report.Unreadable, r.short(id, m))
				continue
			}
			all = append(all, b)
		}
		if err := r.fetch(ctx, all); err != nil {
			return err
		}
		for _, b := range all {
			if b.blob == nil {
				r.report.Unreadable = append(r.report.Unreadable, r.short(b.id, r.blobs[b.id]))
				continue
			}
			if err := r.take(ctx, b.id, b.blob); err != nil {
				return err
			}
		}
	}
	return nil
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

// take gives the record of a blob to the store, as section 3.5 says.
func (r *run) take(ctx context.Context, id string, blob []byte) error {
	if !r.listed {
		l, ok, err := r.s.Devices(ctx)
		if err != nil {
			return fmt.Errorf("the device list: %w", err)
		}
		r.listed, r.devices = true, []nostr.PubKey{}
		if ok {
			// The citizen key may always write.
			r.devices = append(r.devices, r.s.Citizen)
			for _, d := range l.Devices {
				r.devices = append(r.devices, d.Key)
			}
		}
	}
	result, err := r.s.take(ctx, id, blob, r.devices, r.keysOf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.refuse("%s: %v", id, err)
		// A device can join the list later, so its blob gets a new chance
		// on the next sync.
		if !errors.Is(err, errNotListed) {
			r.seen[id] = true
		}
		return nil
	}
	r.seen[id] = true
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

// put is one piece or blob to put on one side.
type put struct {
	side int
	blob storage.Blob
}

// write puts each record that the storage citizens lack. A new record gets
// a new seal: t pieces on the t active storage citizens with a pipe that
// rank highest for its blob. A blob with k or more pieces of one seal
// keeps that seal; if it has fewer than t pieces, the device rebuilds it
// and puts each missing index on the next active storage citizen that
// holds no piece of it. A storage citizen with an older app gets whole
// blobs of format 1.
func (r *run) write(ctx context.Context) error {
	// S counts each active storage citizen whose app keeps tags, or whose
	// app the device did not ask yet.
	var s int
	var pieceSides []int
	for i, side := range r.sides {
		if !side.Active || r.old[i] {
			continue
		}
		s++
		if r.open[i] {
			pieceSides = append(pieceSides, i)
		}
	}
	r.report.K, r.report.T = Code(s)
	direct := slices.DeleteFunc(slices.Clone(pieceSides), func(i int) bool { return r.sides[i].Relayed })
	writers := slices.ContainsFunc(r.sides, func(side Side) bool { return side.Active })
	if !writers {
		return nil
	}
	// Read the store again: a deletion that came in removed records.
	records, err := r.s.Store.Query(Filter(r.s.Citizen))
	if err != nil {
		return err
	}
	var grow []*rebuilding
	for chunk := range slices.Chunk(records, putChunk) {
		var puts []put
		for _, event := range chunk {
			id := r.current.BlobID(event.ID)
			m := r.blobs[id]
			var whole []byte
			// A storage citizen with an older app gets the whole blob.
			for i, side := range r.sides {
				if !side.Active || !r.old[i] || !r.open[i] {
					continue
				}
				if _, listed := r.states[i].Remote[id]; listed {
					continue
				}
				if whole == nil {
					if _, whole, err = Seal(r.current, r.s.Device, event); err != nil {
						return err
					}
				}
				puts = append(puts, put{side: i, blob: storage.Blob{ID: id, Data: whole}})
			}
			if s == 0 {
				continue
			}
			switch {
			case m != nil && len(m.old) > 0:
				// A blob of format 1 still holds the record. A later step
				// moves it to pieces.
			case m == nil || len(m.seals) == 0:
				p, err := r.code(event, id, pieceSides)
				if err != nil {
					return err
				}
				puts = append(puts, p...)
			default:
				sl := r.best(m, false)
				if sl == nil {
					// Only the listings of each storage citizen show that no
					// seal has k pieces.
					if !slices.ContainsFunc(r.states, func(st state) bool { return !st.Listed }) {
						r.report.Mixed++
					}
					continue
				}
				if sl.count() < min(s, 2*sl.h.K) && slices.ContainsFunc(direct, func(i int) bool { return !m.sides[i] }) {
					grow = append(grow, &rebuilding{id: id, sl: sl, shards: map[int][]byte{}, tried: map[string]bool{}})
				}
			}
		}
		if err := r.put(ctx, puts); err != nil {
			return err
		}
	}
	// A missing piece costs k pieces down and one up. Only direct pipes
	// carry it. This is the last step of the sync, so the relayed pipes
	// stay closed to it.
	for i, side := range r.sides {
		r.open[i] = r.open[i] && !side.Relayed
	}
	for chunk := range slices.Chunk(grow, getChunk) {
		if err := r.fetch(ctx, chunk); err != nil {
			return err
		}
		var puts []put
		for _, b := range chunk {
			if b.blob == nil {
				continue
			}
			p, err := r.extend(b, s, direct)
			if err != nil {
				return err
			}
			puts = append(puts, p...)
		}
		if err := r.put(ctx, puts); err != nil {
			return err
		}
	}
	return nil
}

// code seals a record into a new blob, codes it, and makes one piece for
// each of the first t sides of its rank. With fewer than k sides, it puts
// nothing: the record waits for the next sync.
func (r *run) code(event nostr.Event, id string, sides []int) ([]put, error) {
	k, t := r.report.K, r.report.T
	if len(sides) < k {
		r.report.Waiting++
		return nil, nil
	}
	_, blob, err := Seal(r.current, r.s.Device, event)
	if err != nil {
		return nil, err
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
		puts = append(puts, put{side: i, blob: storage.Blob{ID: PieceID(r.idKeys[i], id, index), Data: data, Tag: tag}})
	}
	r.report.Sent++
	return puts, nil
}

// extend makes the missing indices of a seal that k or more pieces hold,
// from the rebuilt blob, for the sides that hold no piece of the blob, in
// the rank of the blob. The other pieces stay.
func (r *run) extend(b *rebuilding, s int, sides []int) ([]put, error) {
	shards, err := Shards(b.blob, b.sl.h.K)
	if err != nil {
		return nil, err
	}
	m := r.blobs[b.id]
	rank := r.rankOf(b.id)
	var free []int
	for _, i := range sides {
		if !m.sides[i] {
			free = append(free, i)
		}
	}
	slices.SortFunc(free, func(a, b int) int { return rank[a] - rank[b] })
	need := min(s, 2*b.sl.h.K) - b.sl.count()
	var puts []put
	for index := 0; index < 2*b.sl.h.K && need > 0 && len(free) > 0; index++ {
		if _, held := b.sl.pieces[index]; held {
			continue
		}
		h := b.sl.h
		h.Index = index
		tag, data, err := SealPiece(r.current, h, shards[index])
		if err != nil {
			return nil, err
		}
		i := free[0]
		free = free[1:]
		puts = append(puts, put{side: i, blob: storage.Blob{ID: PieceID(r.idKeys[i], b.id, index), Data: data, Tag: tag}})
		need--
	}
	return puts, nil
}

// put sends the puts to each side, all sides at the same time, and keeps
// what each side stored.
func (r *run) put(ctx context.Context, puts []put) error {
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
	for i, blobs := range bySide {
		st := &r.states[i]
		for n, res := range results[i] {
			b := blobs[n]
			switch {
			case res.Err != nil:
				r.fail(i, fmt.Errorf("put %s: %w", b.ID, res.Err))
			case res.Status == storage.StatusStored:
				r.report.Sides[i].Stored++
				st.Remote[b.ID] = true
				if b.Tag != nil {
					st.Tags[b.ID] = base64.StdEncoding.EncodeToString(b.Tag)
				}
			case res.Status == storage.StatusHeld:
				// Another put of this ID came after the listing. The next
				// listing gives the tag of a piece.
				r.report.Sides[i].Held++
				if b.Tag == nil {
					st.Remote[b.ID] = true
				}
			case res.Status == storage.StatusTombstoned:
				st.Remote[b.ID] = false
			}
		}
	}
	return ctx.Err()
}
