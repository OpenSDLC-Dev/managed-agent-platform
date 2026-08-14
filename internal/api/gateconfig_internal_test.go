package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestStartEmissionCoalescesPerSession(t *testing.T) {
	pool := pgtest.NewPool(t)
	sessionID, _ := pgtest.NewSession(t, pool, "cloud")
	s := &server{pool: pool, log: events.NewLog(pool)}
	net := domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"env.example.com"}}
	probes := []unreachableProbe{{
		credentialID: "vcrd_conflict", vaultID: "vlt_x",
		allowedHosts: []string{"blocked.example.com"},
	}}

	// An in-flight emission for the session blocks a second launch — a client
	// fetching faster than emissions drain gets one goroutine, not a stack.
	s.emitting.Store(string(sessionID), struct{}{})
	if s.startEmission(context.Background(), string(sessionID), net, probes) {
		t.Fatal("startEmission launched while one was already in flight")
	}
	s.emitting.Delete(string(sessionID))

	if !s.startEmission(context.Background(), string(sessionID), net, probes) {
		t.Fatal("startEmission did not launch with none in flight")
	}
	// The launched emission lands its event and releases the in-flight marker.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, busy := s.emitting.Load(string(sessionID))
		if !busy && unreachableEventCount(t, s, sessionID) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("emission did not complete: busy=%v events=%d", busy, unreachableEventCount(t, s, sessionID))
		}
		time.Sleep(20 * time.Millisecond)
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

// A declaration this platform would refuse to dial is not a promise of reach
// either, so its host is not sent. Driven at the reader, because the create
// grammar rejects these before a session could carry one — which is also why
// this is the only place the rule can be observed.
func TestTheGatesMCPHostsSkipAUrlThePlatformWouldNotDial(t *testing.T) {
	limited := domain.Networking{Type: domain.NetLimited, AllowMCPServers: true}
	agent := []byte(`{"mcp_servers":[
		{"url":"https://good.example/mcp"},
		{"url":"ftp://ftp.example/mcp"},
		{"url":"://not-a-url"},
		{"url":"https:///no-host"},
		{"url":""},
		{"url":"https://*.example.com/mcp"},
		{"url":"https://user:pw@second.example:8443/mcp"}]}`)

	got := mcpGateHosts(limited, agent)
	// The wildcard is the one that would widen rather than narrow: a host set
	// reads `*.example.com` as a suffix rule, so sending it would open every
	// subdomain of example.com to the sandbox from one declaration. The userinfo
	// and the port are dropped with the rest of the URL — only a host is sent.
	if want := []string{"good.example", "second.example"}; !slices.Equal(got, want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
	for _, h := range got {
		if strings.ContainsAny(h, "*:@") {
			t.Errorf("host %q carries more than a host name", h)
		}
	}
	// An agent document that will not decode leaves the gate with no MCP hosts
	// rather than failing the fetch it is blocking on.
	if got := mcpGateHosts(limited, []byte(`{"mcp_servers":`)); got != nil {
		t.Errorf("hosts = %v, want none for an unreadable agent", got)
	}
}
