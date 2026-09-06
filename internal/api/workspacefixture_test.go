package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// The second-workspace fixture, proved before anything is built on it: the
// isolation tests are only as good as their ability to produce a request that
// genuinely belongs to another tenant, and a seam that quietly wrote `default`
// everywhere would make every one of them pass for the wrong reason.
func TestWorkspaceBFixtureProducesASecondTenant(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	key := newWorkspaceBKey(t, s.pool)

	// The workspace is live: an archived one resolves no credential at all.
	var archived *string
	if err := s.pool.QueryRow(ctx,
		`SELECT archived_at::text FROM workspaces WHERE id = $1`, workspaceB).Scan(&archived); err != nil {
		t.Fatalf("read workspace B: %v", err)
	}
	if archived != nil {
		t.Errorf("workspace B archived_at = %v, want NULL", *archived)
	}
	// The key is bound to it, and it authenticates.
	var workspace string
	if err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM api_keys WHERE key_hash = $1`, sha256Hex(key)).Scan(&workspace); err != nil {
		t.Fatalf("read workspace B's key: %v", err)
	}
	if workspace != workspaceB {
		t.Errorf("key workspace_id = %q, want %q", workspace, workspaceB)
	}
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": key})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("workspace B's key: %d, want 200", res.StatusCode)
	}

	// And a resource chain in that workspace, which is what an isolation test
	// asks the fixture for. Only the environment is told the scope; the agent,
	// the session and both threads take it off the row they hang from, so this
	// also pins that inheritance.
	sessionID, envID := pgtest.NewSessionInScope(t, s.pool, "cloud",
		domain.Scope{OrgID: "default", WorkspaceID: workspaceB, ProjectID: "default"})
	childID := pgtest.NewChildThread(t, s.pool, sessionID)
	for _, row := range []struct {
		what string
		q    string
		arg  any
	}{
		{"environment", `SELECT workspace_id FROM environments WHERE id = $1`, envID},
		{"agent", `SELECT a.workspace_id FROM agents a JOIN sessions s ON s.agent_id = a.id WHERE s.id = $1`, sessionID},
		{"session", `SELECT workspace_id FROM sessions WHERE id = $1`, sessionID},
		{"primary thread", `SELECT workspace_id FROM session_threads WHERE id = $1`, domain.PrimaryThreadID(sessionID)},
		{"child thread", `SELECT workspace_id FROM session_threads WHERE id = $1`, childID},
	} {
		var got string
		if err := s.pool.QueryRow(ctx, row.q, row.arg).Scan(&got); err != nil {
			t.Fatalf("%s: %v", row.what, err)
		}
		if got != workspaceB {
			t.Errorf("%s workspace_id = %q, want %q", row.what, got, workspaceB)
		}
	}
}
