package citizen

import (
	"context"
	"log/slog"
	"time"

	"github.com/gezibash/arc/core/transport"
)

// KeepMail watches the mail of one relay, and watches again when the relay
// drops the connection. A watch that ends at midnight starts again at once,
// with the route tags of the new day.
func (sess *Session) KeepMail(ctx context.Context, r transport.Live, ready func(), log *slog.Logger) {
	for {
		err := sess.Mail.Watch(ctx, r, ready)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		log.Warn("the mail watch ended; watching again", "relay", r.Name(), "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
