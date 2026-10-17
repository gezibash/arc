package citizen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/terminal"
)

// movedFile marks that no relay holds a draft of this citizen that the
// storage citizens hold.
const movedFile = "moved"

// moveDrafts deletes from each relay the drafts, checkpoints and parts of
// this citizen that the storage citizens hold, and the private relay lists
// of NIP-37 (docs/storage/SPEC.md, section 4.2). It runs after the import.
// The signer signs each NIP-09 deletion as the citizen; a deletion names
// at most selfsync.MaxMoved events. The deletions go to the relays only,
// never to the local store. The draft of the keyed root stays: a device
// that signs through a bunker reads it from the relays.
//
// A deletion goes only to a relay whose NIP-11 document has the tag
// draft.RetiredTag: that relay serves the deletion to nobody. Another relay
// would serve it to an older device of the citizen, and the store of that
// device would delete the drafts. Such a relay keeps the drafts.
//
// A relay that holds nothing to delete gets no deletion, so a second run
// sends nothing. When each relay answered, and each relay with the tag
// holds nothing that it can delete, the device writes the marker and stops
// asking. arc sync --import removes the marker.
func (sess *Session) moveDrafts(ctx context.Context, cfg SelfSync, kept map[nostr.ID]bool) {
	if len(sess.Relays) == 0 {
		return
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, importedFile)); err != nil {
		return
	}
	marker := filepath.Join(cfg.Dir, movedFile)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	filter := nostr.Filter{Kinds: selfsync.RetiredKinds, Authors: []nostr.PubKey{sess.Key.Public}}
	done := true
	for _, t := range sess.Relays {
		if r, ok := t.(relay.Relay); ok {
			tags, err := r.Tags(ctx)
			if err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the relay did not say if it hides deletions, so no draft was deleted: %v", t.Name(), err)))
				done = false
				continue
			}
			if !slices.Contains(tags, draft.RetiredTag) {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: an older or another relay; the old drafts stay on it, because it would serve their deletion to older devices", t.Name())))
				continue
			}
		}
		batch, err := t.Fetch(ctx, filter)
		if err != nil {
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the drafts on the relay were not listed, so none were deleted: %v", t.Name(), err)))
			done = false
			continue
		}
		var doomed []nostr.Event
		waiting := 0
		for _, event := range batch.Events {
			if event.PubKey != sess.Key.Public || (event.Kind == 31234 && event.Tags.GetD() == iface.KeyedRootD) {
				continue
			}
			ok, err := selfsync.Movable(sess.store, kept, event)
			if err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: %v", t.Name(), err)))
				return
			}
			if ok {
				doomed = append(doomed, event)
			} else {
				waiting++
			}
		}
		deleted := 0
		for i, request := range selfsync.Moves(doomed, nostr.Now()) {
			n := min(selfsync.MaxMoved, len(doomed)-i*selfsync.MaxMoved)
			if err := sess.Signer.SignEvent(ctx, &request); err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the deletion of %d drafts was not signed: %v", t.Name(), n, err)))
				done = false
				break
			}
			if err := t.Send(ctx, request); err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the relay did not take the deletion of %d drafts: %v", t.Name(), n, err)))
				done = false
				break
			}
			deleted += n
		}
		if deleted > 0 {
			fmt.Fprintf(cfg.Out, "%s: deleted %d old drafts and private relay lists from the relay\n", t.Name(), deleted)
		}
		if waiting > 0 {
			done = false
			fmt.Fprintf(cfg.Out, "%s: %d drafts stay until the storage citizens hold them\n", t.Name(), waiting)
		}
	}
	if done {
		if err := os.WriteFile(marker, []byte(nostr.Now().Time().UTC().Format("2006-01-02T15:04:05Z")+"\n"), 0o600); err != nil {
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: %v", err)))
		}
	}
}
