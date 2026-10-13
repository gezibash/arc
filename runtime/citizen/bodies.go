package citizen

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/gezibash/arc/adapters/store/bodies"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/selfsync"
)

// BodyGrace is the age of a body that no wrap names before prune removes
// it. Another program can write a body and name it a moment later.
const BodyGrace = time.Hour

// KeepBody keeps the encrypted body of a draft, before the wrap that names
// it goes to the store.
func (e *Environment) KeepBody(sum string, data []byte) error { return e.Session.bodies.Put(sum, data) }

// Body reads the encrypted body with the SHA-256 sum.
func (e *Environment) Body(sum string) ([]byte, error) { return e.Session.bodies.Get(sum) }

// bodySums names the body of each wrap of the citizen in the store.
func (s *Session) bodySums() (map[string]bool, error) {
	records, err := selfsync.Records(s.store, s.Key.Public)
	if err != nil {
		return nil, err
	}
	sums := map[string]bool{}
	for _, event := range records {
		if ref, ok := draft.Body(event); ok {
			sums[ref.Sum] = true
		}
	}
	return sums, nil
}

// PruneBodies removes each body that no wrap in the store names and that
// is older than BodyGrace: the body of a deleted or replaced file.
func (s *Session) PruneBodies() (int, error) {
	keep, err := s.bodySums()
	if err != nil {
		return 0, err
	}
	return s.bodies.Prune(keep, time.Now().Add(-BodyGrace))
}

// BodiesReport counts the bodies that one sync with a directory moved.
type BodiesReport struct {
	Sent, Received int
	// Missing names each body that a wrap names and that neither side
	// holds.
	Missing []string
}

// SyncBodies syncs the bodies of the drafts with a directory that the
// citizen carries, after the sync of the events: dir/bodies holds them,
// named by their SHA-256. A body goes only where a wrap names it.
func (s *Session) SyncBodies(dir string) (BodiesReport, error) {
	var report BodiesReport
	sums, err := s.bodySums()
	if err != nil {
		return report, err
	}
	carried := bodies.Dir{Path: filepath.Join(dir, "bodies")}
	for sum := range sums {
		switch {
		case s.bodies.Has(sum) && !carried.Has(sum):
			data, err := s.bodies.Get(sum)
			if err != nil {
				return report, err
			}
			if err := carried.Put(sum, data); err != nil {
				return report, err
			}
			report.Sent++
		case !s.bodies.Has(sum) && carried.Has(sum):
			data, err := carried.Get(sum)
			if err != nil {
				return report, fmt.Errorf("the directory: %w", err)
			}
			if err := s.bodies.Put(sum, data); err != nil {
				return report, err
			}
			report.Received++
		case !s.bodies.Has(sum):
			report.Missing = append(report.Missing, sum)
		}
	}
	return report, nil
}
