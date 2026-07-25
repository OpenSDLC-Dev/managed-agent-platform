package api

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// These white-box tests pin the emitter's dedupe and no-conflict branches
// deterministically: production runs emitUnreachableCredentials fire-and-forget
// (a goroutine the HTTP tests can only await by settling), but called directly
// its return is the completion barrier, so an absence assertion here cannot
// false-pass on timing.

func unreachableEventCount(t *testing.T, s *server, sessionID domain.ID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM events
		 WHERE session_id = $1 AND type = 'session.error'
		   AND payload->'error'->>'type' = 'credential_host_unreachable_error'`,
		sessionID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func TestEmitUnreachableCredentialsDedupes(t *testing.T) {
	pool := pgtest.NewPool(t)
	sessionID, _ := pgtest.NewSession(t, pool, "cloud")
	s := &server{pool: pool, log: events.NewLog(pool)}
	net := domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"env.example.com"}}
	probes := []unreachableProbe{{
		credentialID: "vcrd_conflict", vaultID: "vlt_x",
		allowedHosts: []string{"env.example.com", "blocked.example.com"},
	}}

	s.emitUnreachableCredentials(context.Background(), string(sessionID), net, probes)
	if n := unreachableEventCount(t, s, sessionID); n != 1 {
		t.Fatalf("first emission wrote %d events, want 1", n)
	}
	// Re-detection of the same conflict must dedupe against the committed event.
	s.emitUnreachableCredentials(context.Background(), string(sessionID), net, probes)
	if n := unreachableEventCount(t, s, sessionID); n != 1 {
		t.Errorf("re-emission wrote %d events, want still 1 (dedupe)", n)
	}
}

func TestEmitUnreachableCredentialsNoConflict(t *testing.T) {
	pool := pgtest.NewPool(t)
	sessionID, _ := pgtest.NewSession(t, pool, "cloud")
	s := &server{pool: pool, log: events.NewLog(pool)}

	// Covered credential under a limited policy; unrestricted credential under
	// a limited policy; any credential under an unrestricted policy.
	limited := domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"env.example.com"}}
	s.emitUnreachableCredentials(context.Background(), string(sessionID), limited, []unreachableProbe{
		{credentialID: "vcrd_covered", vaultID: "vlt_x", allowedHosts: []string{"env.example.com"}},
		{credentialID: "vcrd_wide", vaultID: "vlt_x", unrestricted: true},
	})
	s.emitUnreachableCredentials(context.Background(), string(sessionID),
		domain.Networking{Type: domain.NetUnrestricted}, []unreachableProbe{
			{credentialID: "vcrd_any", vaultID: "vlt_x", allowedHosts: []string{"anywhere.example.com"}},
		})
	if n := unreachableEventCount(t, s, sessionID); n != 0 {
		t.Errorf("no-conflict emissions wrote %d events, want 0", n)
	}
}
