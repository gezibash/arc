package selfsync

import (
	"context"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
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
	// Coded is the number of blobs of format 1 that the device moved to
	// pieces: it put k or more pieces, and deleted the whole copies.
	Coded int
	// Old is the number of blobs that still have a whole copy of format 1
	// to move to pieces after the sync.
	Old int
	// Replaced is the number of blobs whose record a newer version
	// replaced or a deletion named, and whose pieces and whole copies the
	// repair deleted. In a sync that does not repair, it is the number of
	// such blobs that wait.
	Replaced int
	// Unnamed is the number of blobs of parts and checkpoints that no page
	// names, whose pieces and whole copies the repair deleted. Replaced
	// does not count them. In a sync that does not repair, it is the
	// number of such blobs that wait.
	Unnamed int
	// UnnamedParts and UnnamedCheckpoints are the parts and checkpoints
	// that this device signed, that no page names, and that the repair
	// deleted from the store. The next repair frees their blobs.
	UnnamedParts, UnnamedCheckpoints int
	// UnnamedKept are the parts and checkpoints that no page names, which
	// the repair kept: a write may still run that names them.
	UnnamedKept int
}

// migrateMax is the most blobs of format 1 that one sync moves to pieces.
// The move puts up to 2 times the bytes of each blob, so one tick of arc
// serve does a bounded part of a large home.
const migrateMax = 1024

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
	// migrate says that the blob has whole copies of format 1 to move to
	// pieces. olds are those copies on sides that repair may use. The job
	// deletes them once k pieces of its seal are held.
	migrate bool
	olds    []piece
	// dropsOld says that the drops hold the whole copies.
	dropsOld bool
	// whole are the sides with a pipe that hold a whole copy, for a blob
	// with no record in the store and no seal of k pieces.
	whole []int
	// replaced says that the store replaced or deleted the record of the
	// blob. The job deletes each piece and whole copy, and puts nothing.
	replaced bool
	// unnamed says that the deletion of the record names parts and
	// checkpoints that no page names.
	unnamed bool
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
// it. A rebuild or an audit uses a pipe through TURN only with Syncer.TURN;
// a deletion uses each open pipe.
func (r *run) repair(ctx context.Context) error {
	usable := func(turn bool) func(int) bool {
		return func(i int) bool { return r.open[i] && (turn || !r.sides[i].Relayed) }
	}
	if r.s.Repair {
		if err := r.learnOn(ctx, usable(r.s.TURN)); err != nil {
			return err
		}
	}
	jobs, err := r.planJobs(usable)
	if err != nil {
		return err
	}
	defer r.countOld()
	if !r.s.Repair {
		for _, j := range jobs {
			if j.unnamed {
				r.report.Repair.Unnamed++
			} else if j.replaced {
				r.report.Repair.Replaced++
			} else {
				r.report.Repair.Pending++
			}
		}
		return nil
	}
	sortJobs(jobs)
	// One sync moves at most migrateMax blobs of format 1. The next sync
	// moves the rest.
	if n := slices.IndexFunc(jobs, func(j *job) bool { return j.migrate }); n >= 0 && len(jobs)-n > migrateMax {
		jobs = jobs[:n+migrateMax]
	}
	return r.runRepair(ctx, jobs, usable(r.s.TURN))
}

// planJobs plans a job for each blob that needs one, on the sides that
// repair may use, and counts what waits for a direct path.
func (r *run) planJobs(usable func(turn bool) func(int) bool) ([]*job, error) {
	records, err := Records(r.s.Store, r.s.Citizen)
	if err != nil {
		return nil, err
	}
	local := make(map[string]*nostr.Event, len(records))
	for n := range records {
		local[r.current.BlobID(records[n].ID)] = &records[n]
	}
	r.deleted(records)
	var jobs []*job
	for id, m := range r.blobs {
		use := usable(r.s.TURN)
		if _, old := r.olds.Old[id]; old {
			// A deletion moves no bytes of the blob, so it runs on a pipe
			// through TURN too (docs/storage/SPEC.md, section 4.6).
			use = usable(true)
		}
		j := r.plan(id, m, local[id], use)
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
	return jobs, nil
}

// sortJobs puts the blobs of replaced records first: they need no bytes,
// and they free quota for the puts. Then the blobs with the fewest spare
// pieces. A blob of format 1 goes after each blob that can lose a piece:
// its whole copies still serve.
func sortJobs(jobs []*job) {
	slices.SortFunc(jobs, func(a, b *job) int {
		if a.replaced != b.replaced {
			if a.replaced {
				return -1
			}
			return 1
		}
		if a.migrate != b.migrate {
			if a.migrate {
				return 1
			}
			return -1
		}
		if a.spare != b.spare {
			return a.spare - b.spare
		}
		return strings.Compare(a.id, b.id)
	})
}

// runRepair runs the jobs. Repair reads and writes only on the pipes that
// it may use. The deletions go first, on each open pipe: they move no
// bytes of a blob. Then the other jobs, on the pipes that a rebuild may
// use.
func (r *run) runRepair(ctx context.Context, jobs []*job, rebuild func(int) bool) error {
	open := slices.Clone(r.open)
	defer func() {
		for i := range r.open {
			r.open[i] = r.open[i] && open[i]
		}
	}()
	r.repairing = true
	defer func() { r.repairing = false }()
	deletions := 0
	for deletions < len(jobs) && jobs[deletions].replaced {
		deletions++
	}
	for chunk := range slices.Chunk(jobs[:deletions], getChunk) {
		if err := r.runJobs(ctx, chunk); err != nil {
			return err
		}
	}
	for i := range r.open {
		r.open[i] = rebuild(i)
	}
	for chunk := range slices.Chunk(jobs[deletions:], getChunk) {
		if err := r.runJobs(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// countOld counts the blobs that still have a whole copy of format 1 to
// move to pieces.
func (r *run) countOld() {
	for id, m := range r.blobs {
		if _, old := r.olds.Old[id]; old {
			continue
		}
		if slices.ContainsFunc(m.old, func(i int) bool {
			return !(r.sides[i].Active && r.old[i]) && r.states[i].Remote[id]
		}) {
			r.report.Repair.Old++
		}
	}
}
