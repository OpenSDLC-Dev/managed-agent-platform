package api

import (
	"context"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/jackc/pgx/v5/pgxpool"
)

// requireGateToken is the auth middleware guarding the internal gate-config
// endpoint: a session's egress gate presents its per-session gtk_ token as an
// Authorization: Bearer credential (internal/gatetoken). The resolved session id
// is stored in the request context for the handler. A missing/empty Bearer or an
// unknown/revoked token whose session is archived is a 401 — a gate stops being
// served the moment its token stops authenticating (fail-closed). The lane is
// selected by path (isGateConfigPath), never by inspecting the token value, so a
// gtk_ token never reaches a management handler and an x-api-key never reaches
// this one.
func requireGateToken(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, hasBearer := bearerToken(r)
		if !hasBearer || token == "" {
			writeError(w, r, errAuth("missing Authorization: Bearer gate token"))
			return
		}
		sessionID, err := gatetoken.Authenticate(r.Context(), pool, token)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if sessionID == "" {
			writeError(w, r, errAuth("invalid gate token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeySession, sessionID)))
	})
}

// isGateConfigPath reports whether p is the internal gate-config route. It is a
// single fixed path (no wildcard), so dispatchAuth matches it exactly; the
// escaped path is passed in for the same reason the worker lanes take it — the
// auth choice never depends on %2F decoding.
func isGateConfigPath(p string) bool {
	return p == gateconfig.Path
}

// sessionFrom returns the session a gate's Bearer token authorised, or "" outside
// a gate-authenticated request.
func sessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeySession).(string)
	return s
}
