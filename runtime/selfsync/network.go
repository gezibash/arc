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
	"github.com/gezibash/arc/core/draft"
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

// mapped is what the listings say of one blob.
type mapped struct {
	seals map[SealID]*sealed
	// old holds the sides that list a blob of format 1 with this ID.
	old []int
	// sides holds each side that lists a piece of the blob, also a lost
	// one or one of another seal.
	sides map[int]bool
	// lost holds the pieces that failed an audit or did not open.
	lost []piece
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
	olds    oldState
	listed  bool
	// written holds the blobs that this sync coded with a new seal.
	written map[string]bool
	// repairing says that fetch and put count their bytes as repair.
	repairing bool
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
		keys: map[uint32]Keys{}, failed: map[uint32]error{}, blobs: map[string]*mapped{}, written: map[string]bool{}}
	r.report.Sides = make([]SideReport, len(sides))
	seen, err := s.loadSeen()
	if err != nil {
		return r.report, err
	}
	r.seen = seen
	if r.olds, err = s.loadOld(); err != nil {
		return r.report, err
	}
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
	if s.Repair {
		if err := r.audit(ctx); err != nil {
			return r.report, errors.Join(err, r.save())
		}
	}
	if err := r.read(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	// The deletion of unnamed parts goes before the write, so that this
	// sync gives it to the storage citizens.
	if s.Repair {
		if err := r.unnamed(ctx); err != nil {
			return r.report, errors.Join(err, r.save())
		}
	}
	if err := r.write(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	if err := r.repair(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	r.drained()
	r.suspects()
	if err := r.kept(ctx); err != nil {
		return r.report, errors.Join(err, r.save())
	}
	return r.report, r.save()
}

// kept fills Report.Kept from the listings at the end of the sync, also of
// the sides with no pipe now. A put that a side answered held gives no tag,
// so its record counts at the next listing.
func (r *run) kept(ctx context.Context) error {
	// The map is built again after the puts and the repair. Its refusals
	// are already in the report.
	n := len(r.report.Refused)
	err := r.mapPieces(ctx)
	r.report.Refused = r.report.Refused[:n]
	if err != nil {
		return err
	}
	records, err := Records(r.s.Store, r.s.Citizen)
	if err != nil {
		return err
	}
	r.report.Kept = map[nostr.ID]bool{}
	for _, event := range records {
		if r.onStorage(event) {
			r.report.Kept[event.ID] = true
		}
	}
	r.prune()
	return nil
}

func (r *run) save() error {
	var errs []error
	for i, side := range r.sides {
		errs = append(errs, r.s.save(side.Key.Hex(), r.states[i]))
	}
	errs = append(errs, r.s.writeJSON(r.s.seenPath(), r.seen), r.s.writeJSON(r.s.oldPath(), r.olds))
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
			if e.Deleted {
				delete(st.Lost, e.ID)
				delete(st.Proofs, e.ID)
			}
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

// binaryGen reads the generation of a tag that mapPieces checked.
func binaryGen(tag []byte) uint32 {
	gen, _ := TagGen(tag)
	return gen
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
				_, blob, err := Seal(r.current, r.s.Device, event, body)
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

// count is S: each active storage citizen whose app keeps tags, or whose
// app the device did not ask yet.
func (r *run) count() int {
	s := 0
	for i, side := range r.sides {
		if side.Active && !r.old[i] {
			s++
		}
	}
	return s
}

// sealRecord seals a record into a new blob with the newest keys.
func (r *run) sealRecord(event nostr.Event, body []byte) ([]byte, error) {
	_, blob, err := Seal(r.current, r.s.Device, event, body)
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
