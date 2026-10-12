package selfsync

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/gezibash/arc/sdk/storage"
)

// AuditSample is the number of pieces that a device audits on each
// storage citizen each day (decision 11 of the storage network proposal).
const AuditSample = 32

// AuditEvery is the time between two audits of one storage citizen.
const AuditEvery = 24 * time.Hour

// SuspectAfter is the number of pieces that fail the audits of one day
// before the device calls a storage citizen suspect. Only the user drops
// it.
const SuspectAfter = 3

// newProof makes a secret nonce, and the audit proof of the bytes of a
// piece for it, while the device holds the bytes. An audit of the piece
// later sends the nonce, and needs no download. The result is base64 of
// the nonce and the proof.
func newProof(id string, data []byte) string {
	nonce := storage.NewNonce()
	proof, _ := hex.DecodeString(storage.AuditProof(nonce, id, data))
	return base64.StdEncoding.EncodeToString(append(nonce, proof...))
}

// spend splits a kept proof into its nonce and its proof in hex.
func spend(kept string) (nonce []byte, proof string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(kept)
	if err != nil || len(raw) != 2*storage.NonceSize {
		return nil, "", false
	}
	return raw[:storage.NonceSize], hex.EncodeToString(raw[storage.NonceSize:]), true
}

// audit checks AuditSample random pieces of each storage citizen with a
// pipe, once in AuditEvery (docs/storage/SPEC.md, section 4.6). A piece
// with a kept proof gets an audit request with its secret nonce: 32 bytes
// each way. Each other piece is got and opened. A piece that is absent,
// gives another proof, or does not open is lost: the map leaves it out,
// and repair replaces it. A pipe through TURN gets no audit unless the
// device allows TURN for repair.
func (r *run) audit(ctx context.Context) error {
	now := r.s.now()
	day := now.Unix() / int64(24*time.Hour/time.Second)
	type sample struct {
		ids   []string
		lost  []string
		fresh map[string][]byte
	}
	samples := make([]sample, len(r.sides))
	r.each(func(i int) {
		st := &r.states[i]
		if r.sides[i].Relayed && !r.s.TURN {
			return
		}
		if now.Sub(time.Unix(st.Audited, 0)) < AuditEvery {
			return
		}
		var ids []string
		for id, present := range st.Remote {
			if _, tagged := st.Tags[id]; present && tagged && !st.Lost[id] {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		// The storage citizen must not predict the sample.
		for n := len(ids) - 1; n > 0; n-- {
			m := int(randomBelow(uint64(n + 1)))
			ids[n], ids[m] = ids[m], ids[n]
		}
		ids = ids[:min(AuditSample, len(ids))]
		if len(ids) == 0 {
			return
		}
		smp := sample{ids: ids, fresh: map[string][]byte{}}
		var plain []string
		// The device forgets each kept proof, and saves that, before it
		// sends the nonce: a nonce never goes out two times, also after a
		// crash.
		type kept struct {
			nonce []byte
			proof string
		}
		spent := map[string]kept{}
		for _, id := range ids {
			if nonce, proof, ok := spend(st.Proofs[id]); ok {
				spent[id] = kept{nonce, proof}
				delete(st.Proofs, id)
				continue
			}
			plain = append(plain, id)
		}
		if len(spent) > 0 {
			if err := r.s.save(r.sides[i].Key.Hex(), *st); err != nil {
				r.fail(i, err)
				return
			}
		}
		older := false
		for _, id := range ids {
			k, ok := spent[id]
			if !ok {
				continue
			}
			if older {
				plain = append(plain, id)
				continue
			}
			items, err := r.sides[i].Conn.Audit(ctx, []string{id}, k.nonce)
			if errors.Is(err, storage.ErrOlderApp) {
				older = true
				plain = append(plain, id)
				continue
			}
			if err != nil {
				r.fail(i, err)
				return
			}
			if items[0].Error != "" || items[0].Proof != k.proof {
				smp.lost = append(smp.lost, id)
			}
		}
		if len(plain) > 0 {
			err := r.sides[i].Conn.GetMany(ctx, plain, func(id string, data []byte, err error) error {
				if err != nil {
					if storage.IsRefused(err, storage.ErrNotFound) {
						smp.lost = append(smp.lost, id)
						return nil
					}
					return err
				}
				smp.fresh[id] = data
				return nil
			})
			if err != nil {
				r.fail(i, err)
				return
			}
		}
		samples[i] = smp
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	changed := false
	for i, smp := range samples {
		st := &r.states[i]
		if smp.ids == nil {
			continue
		}
		// The bytes that came open with the tag of their ID. The tag is
		// the device's own record of the piece.
		for id, data := range smp.fresh {
			err := r.opens(ctx, st, id, data)
			switch {
			case err == nil:
				st.Proofs[id] = newProof(id, data)
			case errors.Is(err, errNoKeys):
				// The device cannot judge a piece of a generation that it
				// does not hold.
			default:
				smp.lost = append(smp.lost, id)
			}
		}
		for _, id := range smp.lost {
			st.Lost[id] = true
			r.refuse("%s: piece %s failed the audit: the storage citizen lost it or changed it; repair replaces it", r.sides[i].Label, id)
		}
		if st.AuditDay != day {
			st.AuditDay, st.FailedToday = day, 0
		}
		st.FailedToday += len(smp.lost)
		st.Audited = now.Unix()
		r.report.Sides[i].Audited = len(smp.ids)
		r.report.Sides[i].AuditFailed = len(smp.lost)
		changed = changed || len(smp.lost) > 0
	}
	if !changed {
		return nil
	}
	return r.mapPieces(ctx)
}

// suspects reports the failures of the audits of today, from the state of
// each storage citizen.
func (r *run) suspects() {
	day := r.s.now().Unix() / int64(24*time.Hour/time.Second)
	for i, st := range r.states {
		if st.AuditDay == day {
			r.report.Sides[i].FailedToday = st.FailedToday
			r.report.Sides[i].Suspect = st.FailedToday >= SuspectAfter
		}
	}
}

// errNoKeys says that the device does not hold the keys of a piece.
var errNoKeys = errors.New("the device does not hold the keys of the piece")

// opens checks that the bytes of a piece open with the tag that the device
// listed for its ID.
func (r *run) opens(ctx context.Context, st *state, id string, data []byte) error {
	tag, err := base64.StdEncoding.DecodeString(st.Tags[id])
	if err != nil {
		return err
	}
	gen, err := TagGen(tag)
	if err != nil {
		return err
	}
	keys, err := r.keysOf(ctx, gen)
	if err != nil {
		return errNoKeys
	}
	h, err := OpenTag(keys, tag)
	if err != nil {
		return err
	}
	_, err = OpenPiece(keys, tag, h, data)
	return err
}

// randomBelow returns a random number from 0 to n-1, from crypto/rand.
func randomBelow(n uint64) uint64 {
	var b [8]byte
	limit := ^uint64(0) - ^uint64(0)%n
	for {
		_, _ = rand.Read(b[:])
		if v := binary.BigEndian.Uint64(b[:]); v < limit {
			return v % n
		}
	}
}
