package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// carry takes on another citizen's courier-form wrap, if the hop limit
// allows. The limit comes from beside the event on the carrier, and a reader
// never trusts it past StartHops.
func (m *Mail) carry(wrap nostr.Event, hops int, report *Report) error {
	if hops > StartHops {
		hops = StartHops
	}
	if hops <= 0 || wrap.Tags.Find("w") == nil {
		return nil
	}
	if body, _ := json.Marshal(wrap); len(body) > MaxCarryBytes {
		return nil
	}

	var held carried
	if found, err := m.get(carryBucket, wrap.ID.Hex(), &held); err != nil || found {
		return err
	}

	result, err := m.node.Store.Save(wrap)
	if err != nil {
		return err
	}
	if result.Outcome == store.Refused {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+result.Reason)
		return nil
	}

	entry := carried{
		Wrap: wrap.ID.Hex(), Courier: true, Hops: hops - 1,
		Received: m.now().UTC(), Expires: expiry(wrap, m.now()),
	}
	if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
		return err
	}
	report.Carried++
	return nil
}

// expiry is the expiration of a wrap, or MaxAge from now when it names none.
func expiry(wrap nostr.Event, now time.Time) time.Time {
	if tag := wrap.Tags.Find("expiration"); len(tag) > 1 {
		if at, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
			return time.Unix(at, 0).UTC()
		}
	}
	return now.Add(private.MaxAge).UTC()
}

// evict drops the oldest mail of other citizens past MaxCarried.
func (m *Mail) evict() error {
	var others []carried
	if err := m.each(func(c carried) {
		if !c.Own {
			others = append(others, c)
		}
	}); err != nil {
		return err
	}
	if len(others) <= MaxCarried {
		return nil
	}

	sort.Slice(others, func(a, b int) bool { return others[a].Received.Before(others[b].Received) })
	return m.update(func(tx kv.Tx) error {
		for _, c := range others[:len(others)-MaxCarried] {
			if err := tx.Bucket(carryBucket).Delete([]byte(c.Wrap)); err != nil {
				return err
			}
		}
		return nil
	})
}

// StoppedError says that a transport stopped answering partway through the
// sends of a sync. The wraps that did not go wait for the next sync.
type StoppedError struct {
	// Transport is the name of the transport.
	Transport string
	// Sent is the number of wraps that it took, of Waiting.
	Sent, Waiting int
	// Err is the error of the send that got no answer.
	Err error
}

func (e *StoppedError) Error() string {
	return fmt.Sprintf("%s stopped answering after it took %d of %d wraps: %v", e.Transport, e.Sent, e.Waiting, e.Err)
}

func (e *StoppedError) Unwrap() error { return e.Err }

// give sends the transport every wrap that this node moves. A carrier gets
// the courier form only, with its hop limit, because the relay form names the
// recipient. A relay gets each wrap once.
func (m *Mail) give(ctx context.Context, t transport.Transport, carrier transport.Carrier, isCarrier bool, report *Report) error {
	var entries []carried
	if err := m.each(func(c carried) { entries = append(entries, c) }); err != nil {
		return err
	}

	waiting := 0
	for _, entry := range entries {
		if !isCarrier && !slices.Contains(entry.SentTo, t.Name()) {
			waiting++
		}
	}
	sent := 0
	for _, entry := range entries {
		if isCarrier && !entry.Courier {
			continue
		}
		if !isCarrier && slices.Contains(entry.SentTo, t.Name()) {
			continue
		}

		id, err := nostr.IDFromHex(entry.Wrap)
		if err != nil {
			continue
		}
		events, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue
		}

		if isCarrier {
			err = carrier.SendHops(ctx, events[0], entry.Hops)
		} else {
			err = t.Send(ctx, events[0])
		}
		// A relay that gave no answer would make each wrap after this one
		// wait or fail the same way: stop, and keep the rest for the next
		// sync. It refused nothing, so no wrap counts as refused (#465).
		if err != nil && !isCarrier && m.Unanswered != nil && m.Unanswered(err) {
			return &StoppedError{Transport: t.Name(), Sent: sent, Waiting: waiting, Err: err}
		}
		if err != nil {
			report.Refused = append(report.Refused, "not sent "+entry.Wrap+": "+err.Error())
			continue
		}
		report.Sent++
		sent++

		if !isCarrier {
			entry.SentTo = append(entry.SentTo, t.Name())
			if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
				return err
			}
		}
		if entry.Own && entry.Rumor != "" {
			if err := m.attempted(outboxKey(entry.Rumor, entry.To)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Mail) attempted(key string) error {
	// Read first: each write transaction ends with an fsync, and a courier
	// gets again the wraps of an acknowledgement, which has no outbox entry.
	open := false
	if err := m.view(func(tx kv.Tx) error {
		if b := tx.Bucket(outboxBucket); b != nil {
			if data := b.Get([]byte(key)); data != nil {
				var out Outgoing
				if err := json.Unmarshal(data, &out); err != nil {
					return err
				}
				open = out.Delivered.IsZero()
			}
		}
		return nil
	}); err != nil || !open {
		return err
	}
	return m.update(func(tx kv.Tx) error {
		data := tx.Bucket(outboxBucket).Get([]byte(key))
		if data == nil {
			return nil
		}
		var out Outgoing
		if err := json.Unmarshal(data, &out); err != nil {
			return err
		}
		if !out.Delivered.IsZero() {
			return nil
		}
		out.Attempts++
		return put(tx, outboxBucket, key, out)
	})
}

func (m *Mail) expire() error {
	return m.update(func(tx kv.Tx) error {
		cursor := tx.Bucket(carryBucket).Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var entry carried
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if m.now().After(entry.Expires) {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return m.expireDropped(tx)
	})
}

func (m *Mail) Carrying() (int, error) {
	count := 0
	err := m.each(func(c carried) {
		if !c.Own {
			count++
		}
	})
	return count, err
}

func (m *Mail) each(fn func(carried)) error {
	return m.view(func(tx kv.Tx) error {
		return tx.Bucket(carryBucket).ForEach(func(_, v []byte) error {
			var c carried
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			fn(c)
			return nil
		})
	})
}
