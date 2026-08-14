package api

import (
	"context"
	"slices"
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

	s.emitUnreachableCredentials(context.Background(), string(sessionID), net, nil, probes)
	if n := unreachableEventCount(t, s, sessionID); n != 1 {
		t.Fatalf("first emission wrote %d events, want 1", n)
	}
	// Re-detection of the same conflict must dedupe against the committed event.
	s.emitUnreachableCredentials(context.Background(), string(sessionID), net, nil, probes)
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
	if s.startEmission(context.Background(), string(sessionID), net, nil, probes) {
		t.Fatal("startEmission launched while one was already in flight")
	}
	s.emitting.Delete(string(sessionID))

	if !s.startEmission(context.Background(), string(sessionID), net, nil, probes) {
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
	s.emitUnreachableCredentials(context.Background(), string(sessionID), limited, nil, []unreachableProbe{
		{credentialID: "vcrd_covered", vaultID: "vlt_x", allowedHosts: []string{"env.example.com"}},
		{credentialID: "vcrd_wide", vaultID: "vlt_x", unrestricted: true},
	})
	s.emitUnreachableCredentials(context.Background(), string(sessionID),
		domain.Networking{Type: domain.NetUnrestricted}, nil, []unreachableProbe{
			{credentialID: "vcrd_any", vaultID: "vlt_x", allowedHosts: []string{"anywhere.example.com"}},
		})
	if n := unreachableEventCount(t, s, sessionID); n != 0 {
		t.Errorf("no-conflict emissions wrote %d events, want 0", n)
	}
}

// Every declaration that would widen the gate past the server it names is
// refused. The list is agent-controlled and passes no URL grammar at all
// (parseMCPServers requires a non-empty string), so this reader is where the
// shape is decided — which is why it is driven directly.
func TestTheGatesMCPEndpointsRefuseWhatWouldWiden(t *testing.T) {
	limited := domain.Networking{Type: domain.NetLimited, AllowMCPServers: true}
	declared := []byte(`[
		{"url":"https://good.example/mcp"},
		{"url":"ftp://ftp.example/mcp"},
		{"url":"://not-a-url"},
		{"url":"https:///no-host"},
		{"url":""},
		{"url":"https://*.example.com/mcp"},
		{"url":"http://[::1]:9000/mcp"},
		{"url":"https://[2606:4700:4700::1111]/mcp"},
		{"url":"http://169.254.169.254/latest/meta-data/"},
		{"url":"http://127.0.0.1:8080/mcp"},
		{"url":"http://[64:ff9b::a9fe:a9fe]/mcp"},
		{"url":"https://user:pw@SECOND.example:8443/mcp"},
		{"url":"http://fourth.example:0443/mcp"},
		{"url":"https://.example.com/mcp"},
		{"url":"https://a..b.example/mcp"},
		{"url":"https://999.999.999.999/mcp"},
		{"url":"https://port.example:0/mcp"},
		{"url":"https://port.example:99999/mcp"},
		{"url":"https://trailing.example./mcp"},
		{"url":"http://third.example/mcp"}]`)

	got, refused := mcpGateEndpoints(limited, declared)
	// The port is part of the endpoint, defaulted from the scheme when the URL
	// names none, and the host is lowercased the way a host set matches.
	want := []string{"good.example:443", "second.example:8443", "fourth.example:443", "third.example:80"}
	if !slices.Equal(got, want) {
		t.Errorf("endpoints = %v, want %v", got, want)
	}
	// Every refusal is counted, because a refusal is invisible to the sandbox —
	// it sees the same 403 any unadmitted host gets — so the count is the only
	// thing an operator has to correlate with.
	if want := len(got) + refused; want != 20 {
		t.Errorf("endpoints + refused = %d, want one per declaration (20)", want)
	}
	// Each refusal for its own reason:
	//   the wildcard, which a host set reads as a suffix rule;
	//   the two IPv6 literals, which the gate cannot match consistently over
	//     both CONNECT (bracket-stripped) and plain HTTP (bracketed) — one of
	//     them a routable address, so it is refused by this rule and not by the
	//     address floor;
	//   the three addresses the platform's own MCP client refuses — link-local
	//     (cloud metadata), loopback, and the same metadata address hidden in a
	//     NAT64 wrapper;
	//   the empty DNS labels, the malformed dotted-numeric address, and the
	//     trailing dot, all of which an operator's allowed_hosts grammar refuses
	//     too — this reader asks that grammar rather than restating it;
	//   the two impossible ports;
	//   and the userinfo and path, which never leave this function.
	// The one that is normalized rather than refused is `:0443`, which is the
	// port the dialer would resolve it to.
}

// A declared array that will not decode leaves the gate with no MCP endpoints
// rather than failing the fetch it is blocking on. A resolved agent with no
// mcp_servers key projects to SQL NULL, which pgx scans into a nil []byte; a
// key whose value is JSON null is the separate `null` row.
func TestTheGatesMCPEndpointsFailClosedOnAnUnreadableArray(t *testing.T) {
	limited := domain.Networking{Type: domain.NetLimited, AllowMCPServers: true}
	for name, declared := range map[string][]byte{
		"a truncated array": []byte(`[{"url":`),
		"a JSON null":       []byte(`null`),
		"an object":         []byte(`{"url":"https://good.example/mcp"}`),
		"nothing at all":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			got, refused := mcpGateEndpoints(limited, declared)
			if len(got) != 0 || refused != 0 {
				t.Errorf("endpoints = %v, refused = %d, want neither", got, refused)
			}
		})
	}
}

// The hosts this handler widens the gate by are hosts a credential can be
// substituted on, so a credential naming one is not reported unreachable. The
// emission is permanent and deduped, so a false one would ride the session for
// its whole life.
func TestACredentialOnADeclaredMCPHostIsNotUnreachable(t *testing.T) {
	pool := pgtest.NewPool(t)
	sessionID, _ := pgtest.NewSession(t, pool, "cloud")
	s := &server{pool: pool, log: events.NewLog(pool)}
	net := domain.Networking{Type: domain.NetLimited, AllowMCPServers: true,
		AllowedHosts: []string{"env.example.com"}}
	probes := []unreachableProbe{{
		credentialID: "vcrd_mcp", vaultID: "vlt_x",
		allowedHosts: []string{"mcp.example.com"},
	}}

	// The same probe, judged against a policy that is not told the MCP host: the
	// conflict is real, and this is what the assertion below has to be different
	// from.
	s.emitUnreachableCredentials(context.Background(), string(sessionID), net, nil, probes)
	if n := unreachableEventCount(t, s, sessionID); n != 1 {
		t.Fatalf("without the MCP host the emitter wrote %d events, want 1", n)
	}

	other, _ := pgtest.NewSession(t, pool, "cloud")
	s.emitUnreachableCredentials(context.Background(), string(other), net,
		hostsOf([]string{"mcp.example.com:443"}), probes)
	if n := unreachableEventCount(t, s, other); n != 0 {
		t.Errorf("with the MCP host the emitter wrote %d events, want 0", n)
	}
}
