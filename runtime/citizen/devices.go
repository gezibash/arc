package citizen

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/node"
)

// errSignerOnly says that a machine without the citizen key cannot change
// the device list or the storage list. A signer refuses to sign them for a
// device.
var errSignerOnly = errors.New("only the signer, which holds the citizen key, changes the device list and the storage list")

// DeviceList returns the newest device list of this citizen, from the store
// and the relays. It returns false when no list exists. It keeps the list on
// the disk, so that the store checks the signers of self data with no path
// to the signer. When the newest event is the list that this machine opened
// last, it asks the signer nothing.
func (sess *Session) DeviceList(ctx context.Context) (devices.List, bool, error) {
	filter := devices.Filter(sess.Key.Public)
	reports, errs := sess.Node.Pull(ctx, filter, sess.Relays)
	events, err := sess.Node.Store.Query(filter)
	if err != nil {
		return devices.List{}, false, err
	}
	unreached := node.Unreached(reports, errs)
	if at, ok := sess.signers.at(); ok {
		newer := false
		for _, event := range events {
			newer = newer || event.CreatedAt > at
		}
		if !newer {
			return sess.signers.list(), true, nil
		}
	}
	if len(events) == 0 && unreached != nil {
		return devices.List{}, false, fmt.Errorf("this machine holds no device list, and %w", unreached)
	}
	list, ok, err := NewestDeviceList(ctx, sess.Signer, sess.Key.Public, events)
	if err != nil {
		return list, ok, err
	}
	if err := sess.keepList(ctx, list, ok); err != nil {
		sess.warnf("arc: the device list is not kept on this disk: %v\n", err)
	}
	return list, ok, nil
}

// NewestDeviceList opens the newest list among events that the citizen
// signed. It skips an event that does not open.
func NewestDeviceList(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey, events []nostr.Event) (devices.List, bool, error) {
	var newest *nostr.Event
	var list devices.List
	var last error
	for _, event := range events {
		if newest != nil && !devices.Newer(event, *newest) {
			continue
		}
		opened, err := devices.Open(ctx, k, citizen, event)
		if err != nil {
			last = err
			continue
		}
		newest, list = &event, opened
	}
	if newest == nil && last != nil {
		return devices.List{}, false, last
	}
	return list, newest != nil, nil
}

// PublishDeviceList seals the list with the citizen key, keeps it, and
// sends it to the relays of the citizen. It returns the list with the time
// of its event. It fails when no relay takes it.
func (sess *Session) PublishDeviceList(ctx context.Context, list devices.List) (devices.List, error) {
	if sess.Remote {
		return devices.List{}, errSignerOnly
	}
	event, err := devices.Seal(ctx, sess.Signer, list, nostr.Now())
	if err != nil {
		return devices.List{}, err
	}
	list.At = event.CreatedAt
	if err := sess.keepList(ctx, list, true); err != nil {
		sess.warnf("arc: the device list is not kept on this disk: %v\n", err)
	}
	return list, sess.publishOwn(ctx, event, "the device list")
}

// publishOwn keeps an event of the citizen and sends it to the relays of the
// citizen. It fails when no relay takes it.
func (sess *Session) publishOwn(ctx context.Context, event nostr.Event, what string) error {
	_, sent, err := sess.Node.Publish(ctx, event, sess.Relays)
	if err != nil {
		return err
	}
	var failed []error
	for _, s := range sent {
		if s.Err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", s.Transport, s.Err))
		}
	}
	if len(sent) == 0 {
		return errors.New("this citizen has no relays: add one with arc relay add")
	}
	if len(failed) == len(sent) {
		return fmt.Errorf("no relay took %s: %w", what, errors.Join(failed...))
	}
	return nil
}

// RequireSigner refuses a machine that signs through a remote signer.
func (sess *Session) RequireSigner() error {
	if sess.Remote {
		return errSignerOnly
	}
	return nil
}
