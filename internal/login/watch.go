package login

import (
	"context"
	"net/http"
	"time"
)

// WatchAuthority keeps a private streaming request bound to a current browser
// session. The caller stops and joins the watcher before returning.
func (m *Manager) WatchAuthority(w http.ResponseWriter, r *http.Request, allowed func(Session) bool) (context.Context, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				session, err := m.Authenticate(checkCtx, r)
				stop()
				if ctx.Err() != nil {
					return
				}
				if err != nil || !allowed(session) {
					cancel()
					// Expire a blocked write as well as the upstream request.
					_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}
