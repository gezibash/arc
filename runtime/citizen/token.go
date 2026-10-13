package citizen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/kv"
)

// tokenBucket keeps the token of this device: the event with which the
// citizen key lets the device key read sealed data on a relay.
var (
	tokenBucket = []byte("device-token")
	tokenKey    = []byte("token")
)

// tokenMargin is the time that a token must still last when a device uses
// it, so that it does not end during a command.
const tokenMargin = 60 * 60

// delegation gives the relays of a device its device key and token. It
// asks the signer for a new token only after the signer answered for the
// citizen key, so a device with no path to the signer never waits for it.
type delegation struct {
	sess   *Session
	mu     sync.Mutex
	loaded bool
	token  *nostr.Event
	asked  bool
}

func (d *delegation) Token(ctx context.Context) (nostr.Signer, nostr.Event, bool) {
	token := d.current()
	if token == nil {
		return nil, nostr.Event{}, false
	}
	now := nostr.Now()
	end, err := devices.TokenEnd(*token, d.sess.Device.Public, now)
	if err != nil || end < now+tokenMargin {
		return nil, nostr.Event{}, false
	}
	return d.sess.Device, *token, true
}

// Answered asks the signer for a new token when this device holds none,
// or when its token is in the second half of its time. It asks once for
// each session.
func (d *delegation) Answered(ctx context.Context) {
	token := d.current()
	d.mu.Lock()
	if d.asked {
		d.mu.Unlock()
		return
	}
	if token != nil {
		now := nostr.Now()
		if end, err := devices.TokenEnd(*token, d.sess.Device.Public, now); err == nil && now < token.CreatedAt+(end-token.CreatedAt)/2 {
			d.mu.Unlock()
			return
		}
	}
	d.asked = true
	d.mu.Unlock()
	if err := d.sess.RenewToken(ctx); err != nil {
		d.sess.warnf("arc: this device has no new token for the relays: %v\n", err)
	}
}

// current returns the token on the disk, or nil.
func (d *delegation) current() *nostr.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded {
		return d.token
	}
	d.loaded = true
	var text []byte
	_ = d.sess.roots.View(func(tx kv.Tx) error {
		if b := tx.Bucket(tokenBucket); b != nil {
			text = b.Get(tokenKey)
		}
		return nil
	})
	if text == nil {
		return nil
	}
	var token nostr.Event
	if err := json.Unmarshal(text, &token); err != nil {
		return nil
	}
	d.token = &token
	return d.token
}

func (d *delegation) set(token nostr.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loaded, d.token = true, &token
}

// RenewToken asks the signer to sign a new token for the device key of this
// machine, and keeps it. A machine that holds the citizen key needs none.
func (sess *Session) RenewToken(ctx context.Context) error {
	if !sess.Remote || sess.delegation == nil {
		return errors.New("only a device that signs through a remote signer uses a token")
	}
	now := nostr.Now()
	token := devices.NewToken(sess.Device.Public, now)
	if err := sess.Signer.SignEvent(ctx, &token); err != nil {
		return err
	}
	citizen, _, err := devices.CheckToken(token, sess.Device.Public, now)
	if err != nil {
		return fmt.Errorf("the signer gave a token that is not valid: %w", err)
	}
	if citizen != sess.Key.Public {
		return fmt.Errorf("the signer signed the token as %s, not as %s", citizen.Hex(), sess.Key.Public.Hex())
	}
	text, err := json.Marshal(token)
	if err != nil {
		return err
	}
	if err := sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(tokenBucket)
		if err != nil {
			return err
		}
		return b.Put(tokenKey, text)
	}); err != nil {
		return err
	}
	sess.delegation.set(token)
	return nil
}
