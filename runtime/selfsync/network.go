package selfsync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"fiatjaf.com/nostr"
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
	// namedOf holds what named returned for each address in this sync.
	namedOf map[string]namedSet
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
		keys: map[uint32]Keys{}, failed: map[uint32]error{}, blobs: map[string]*mapped{}, written: map[string]bool{}, namedOf: map[string]namedSet{}}
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
