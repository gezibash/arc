package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
)

// incoming separates receipt from execution. Receipt holds the encrypted seal
// until the event store has it; committing this journal always comes first.
// A stale processing record is uncertain, never permission to repeat a mutation.
type incoming struct {
	Seal      string       `json:"seal"`
	Receipt   *nostr.Event `json:"receipt,omitempty"`
	State     string       `json:"state"`
	Started   time.Time    `json:"started,omitzero"`
	ReplySeal string       `json:"reply_seal,omitempty"`
}

func (m *Mail) receiveRequest(ctx context.Context, opened private.Opened, previouslyReceived bool, report *Report) error {
	id := opened.Rumor.ID.Hex()
	err := m.update(func(tx kv.Tx) error {
		if tx.Bucket(inboxBucket).Get([]byte(id)) != nil {
			return nil
		}
		entry := incoming{Seal: opened.Seal.ID.Hex(), Receipt: &opened.Seal, State: "pending"}
		// A seal written by an older ARC version has no execution journal. It
		// might already have run; do not execute it again during an upgrade.
		if previouslyReceived {
			entry.State = "processing"
		}
		return put(tx, inboxBucket, id, entry)
	})
	if err != nil {
		return err
	}
	return m.answer(ctx, id, report)
}

func (m *Mail) pending(ctx context.Context, report *Report) error {
	var ids []string
	if err := m.view(func(tx kv.Tx) error {
		return tx.Bucket(inboxBucket).ForEach(func(k, v []byte) error {
			var entry incoming
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if entry.State != "completed" {
				ids = append(ids, string(k))
			}
			return nil
		})
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.answer(ctx, id, report); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mail) answer(ctx context.Context, id string, report *Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var entry incoming
	found, err := m.get(inboxBucket, id, &entry)
	if err != nil || !found || entry.State == "completed" {
		return err
	}
	if entry.Receipt != nil {
		// A crash on either side of Save leaves enough ciphertext in the
		// journal to finish receipt without waiting for another delivery.
		result, err := m.node.Store.Save(*entry.Receipt)
		if err != nil {
			return err
		}
		err = m.update(func(tx kv.Tx) error {
			entry = incoming{}
			if err := json.Unmarshal(tx.Bucket(inboxBucket).Get([]byte(id)), &entry); err != nil {
				return err
			}
			if entry.Receipt == nil {
				return nil
			}
			entry.Receipt = nil
			if result.Outcome == store.Refused {
				entry.State = "completed"
			}
			return put(tx, inboxBucket, id, entry)
		})
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			report.Refused = append(report.Refused, id+": "+result.Reason)
			return nil
		}
	}
	if m.OnRequest == nil || entry.State == "completed" {
		return nil
	}
	if entry.State == "processing" && !entry.Started.IsZero() && m.now().Before(entry.Started.Add(call.Timeout+time.Minute)) {
		return nil
	}
	// Opening the request cannot execute it. A failed read or signer call
	// must leave pending work retryable. Claim it atomically only afterward.
	sealID, err := nostr.IDFromHex(entry.Seal)
	if err != nil {
		return err
	}
	seals, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{sealID}})
	if err != nil {
		return err
	}
	if len(seals) == 0 {
		return fmt.Errorf("mail: request %s has no saved seal", id)
	}
	request, err := private.OpenSeal(ctx, m.key, seals[0])
	if err != nil {
		return err
	}
	if m.Serves != nil && !m.Serves(request) {
		return nil
	}
	var run, uncertain bool
	err = m.update(func(tx kv.Tx) error {
		data := tx.Bucket(inboxBucket).Get([]byte(id))
		if data == nil {
			return nil
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		switch entry.State {
		case "completed":
			return nil
		case "pending":
			run = true
		case "processing":
			if !entry.Started.IsZero() && m.now().Before(entry.Started.Add(call.Timeout+time.Minute)) {
				return nil
			}
			uncertain = true
		default:
			return fmt.Errorf("mail: unknown request state %q", entry.State)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entry.State = "processing"
		entry.Started = m.now().UTC()
		return put(tx, inboxBucket, id, entry)
	})
	if err != nil || !run && !uncertain {
		return err
	}
	reply := call.Reply{Err: "outcome_unknown a previous execution did not record a result"}
	if run {
		budget, cancel := context.WithTimeout(ctx, call.Timeout)
		reply, err = m.OnRequest(budget, request)
		cancel()
		if err != nil {
			reply = call.Reply{Err: err.Error()}
		}
	}
	rumor := call.ReplyRumor(m.key, request, reply, m.now())
	if err := private.CheckMail(rumor); err != nil {
		// The caller gets an error in place of a reply that mail cannot carry.
		rumor = call.ReplyRumor(m.key, request, call.Reply{Err: TooLargeReply}, m.now())
	}
	seal, entries, err := m.prepare(ctx, request.PubKey, rumor, m.now().Add(private.MaxAge), 0)
	if err != nil {
		return err
	}
	entry.State = "completed"
	entry.ReplySeal = seal.ID.Hex()
	if err := m.update(func(tx kv.Tx) error {
		if err := putCarried(tx, entries); err != nil {
			return err
		}
		return put(tx, inboxBucket, id, entry)
	}); err != nil {
		return err
	}
	m.sendPrepared(ctx, request.PubKey, entries)
	report.Answered++
	return nil
}

// replied files the reply to one of this citizen's calls, if the provider
// that the call went to sent it.
func (m *Mail) replied(opened private.Opened, report *Report) error {
	id, _ := call.ReadReply(opened.Rumor)

	key := outboxKey(id, opened.Author().Hex())
	var out Outgoing
	found, err := m.get(outboxBucket, key, &out)
	if err != nil || !found {
		return err
	}
	if out.Kind != "request" || out.ReplySeal != "" {
		return nil
	}

	out.Delivered = m.now().UTC()
	out.ReplySeal = opened.Seal.ID.Hex()
	report.Replies++
	return m.put(outboxBucket, key, out)
}

// acknowledged marks a message delivered to the author of the
// acknowledgement. The entries of the other members of a group message
// stay as they are.
func (m *Mail) acknowledged(opened private.Opened, report *Report) error {
	tag := opened.Rumor.Tags.Find("e")
	if len(tag) < 2 {
		return nil
	}

	key := outboxKey(tag[1], opened.Author().Hex())
	var out Outgoing
	found, err := m.get(outboxBucket, key, &out)
	if err != nil || !found {
		return err
	}
	if !out.Delivered.IsZero() {
		return nil
	}

	out.Delivered = m.now().UTC()
	report.Delivered++
	return m.put(outboxBucket, key, out)
}
