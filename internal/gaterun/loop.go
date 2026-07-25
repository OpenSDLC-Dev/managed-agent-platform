package gaterun

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
)

// Fetcher retrieves the session's current gate config from the control plane.
// *gateconfig.Client satisfies it.
type Fetcher interface {
	Fetch(ctx context.Context) (*gateconfig.Config, error)
}

// RunFetchLoop keeps handler's gate current for the session's life: it fetches
// once immediately, then every interval, converting each config into a fresh
// gate and swapping it in atomically.
//
// The two error postures are the revocation contract (docs/plan/12 slice 4c-2b):
//   - gateconfig.ErrUnauthorized — the token was revoked or the session archived
//     — is terminal. The loop swaps in a deny-all gate and returns the error, so
//     the caller shuts the server down: an unauthorized gate stops serving
//     (fail-closed), and a 401 is the one unambiguous "revoked" signal.
//   - any other error is transient (a control-plane blip or network hiccup): the
//     loop keeps the last-known-good gate and retries next tick, so a momentary
//     outage — even one longer than any TTL — never cuts a live session's egress.
//
// It returns nil when ctx is cancelled.
func RunFetchLoop(ctx context.Context, f Fetcher, handler *SwappableHandler, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		cfg, err := f.Fetch(ctx)
		switch {
		case err == nil:
			handler.Swap(gate.New(Convert(cfg)))
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, gateconfig.ErrUnauthorized):
			handler.Swap(gate.New(gate.Config{})) // deny-all until the server stops
			return err
		default:
			slog.WarnContext(ctx, "gate config fetch failed; keeping last-known-good", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
