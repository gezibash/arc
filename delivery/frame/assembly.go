package frame

import (
	"bytes"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/compact"
)

const (
	MaxAssemblies    = 128
	AssemblyLifetime = 30 * time.Second
	// MaxBufferedBytes bounds bodies across every incomplete assembly.
	MaxBufferedBytes = 8 << 20
)

type assembly struct {
	started time.Time
	total   uint16
	hops    byte
	size    int
	parts   map[uint16][]byte
}

// Assembler's zero value is ready to use. Call from one goroutine, or provide
// external synchronization. Pass a monotonic clock (normally time.Now) to Add
// and Expire. The transport should call Expire on a timer even while idle.
type Assembler struct {
	pending  map[[8]byte]*assembly
	buffered int
}

// Completed is a parsed, not yet authenticated, event. Hops is the smallest
// remaining hop limit seen across its fragments, including duplicates.
type Completed struct {
	Event nostr.Event
	Hops  byte
}

func (a *Assembler) drop(id [8]byte) {
	if p := a.pending[id]; p != nil {
		a.buffered -= p.size
		delete(a.pending, id)
	}
}

// Expire releases assemblies 30 seconds after their first fragment. Additional
// fragments and duplicates never extend that deadline. It returns the count.
func (a *Assembler) Expire(now time.Time) int {
	removed := 0
	for id, p := range a.pending {
		if !now.Before(p.started.Add(AssemblyLifetime)) {
			a.drop(id)
			removed++
		}
	}
	return removed
}

// Add returns nil until all fragments arrive. Identical duplicates are ignored.
// Conflicting totals or duplicate bodies discard the affected assembly. Capacity
// failures reject the incoming piece without evicting unrelated assemblies.
// Completion clears the assembly; replay suppression belongs to the event store.
func (a *Assembler) Add(f Frame, now time.Time) (*Completed, error) {
	a.Expire(now)
	if err := f.validate(); err != nil {
		return nil, err
	}
	if f.Type == Event {
		return complete(f.Body, f.Hops)
	}
	p := a.pending[f.ID]
	if p != nil {
		if p.total != f.Total {
			a.drop(f.ID)
			return nil, ErrConflict
		}
		if body, ok := p.parts[f.Index]; ok {
			if !bytes.Equal(body, f.Body) {
				a.drop(f.ID)
				return nil, ErrConflict
			}
			p.hops = min(p.hops, f.Hops)
			return nil, nil
		}
		if len(f.Body) > compact.MaxBytes-p.size {
			a.drop(f.ID)
			return nil, ErrSize
		}
	} else if len(a.pending) >= MaxAssemblies {
		return nil, ErrCapacity
	}
	if len(f.Body) > MaxBufferedBytes-a.buffered {
		return nil, ErrCapacity
	}
	if p == nil {
		if a.pending == nil {
			a.pending = make(map[[8]byte]*assembly)
		}
		p = &assembly{started: now, total: f.Total, hops: f.Hops, parts: make(map[uint16][]byte)}
		a.pending[f.ID] = p
	}
	p.parts[f.Index] = append([]byte(nil), f.Body...)
	p.size += len(f.Body)
	a.buffered += len(f.Body)
	p.hops = min(p.hops, f.Hops)
	if len(p.parts) != int(p.total) {
		return nil, nil
	}
	body := make([]byte, 0, p.size)
	for i := uint16(0); i < p.total; i++ {
		body = append(body, p.parts[i]...)
	}
	a.drop(f.ID)
	if fragmentID(body) != f.ID {
		return nil, ErrConflict
	}
	return complete(body, p.hops)
}

func complete(body []byte, hops byte) (*Completed, error) {
	event, err := compact.Decode(body)
	if err != nil {
		return nil, err
	}
	return &Completed{Event: event, Hops: hops}, nil
}
