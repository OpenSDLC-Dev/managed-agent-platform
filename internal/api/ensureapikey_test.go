package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// A management key resolves exactly one workspace (plan 42 §6.2), so a value
// configured in a second workspace is a MOVE, not a coexistence: the upsert
// conflicts on the globally unique key_hash and carries workspace_id across.
// Without that column in the DO UPDATE set the row would keep the workspace it
// was first configured in, and a key an operator configured for B would quietly
// authenticate into A — the one outcome tenancy exists to prevent.
func TestEnsureAPIKeyMovesAKeyConfiguredInAnotherWorkspace(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	registerWorkspace(t, s.pool, workspaceB)

	const key = "ak-configured-twice"
	if err := api.EnsureAPIKey(ctx, s.pool, "boot", key); err != nil {
		t.Fatalf("EnsureAPIKey in the default workspace: %v", err)
	}
	warnings := captureWarnings(t)
	if err := api.EnsureAPIKeyInWorkspace(ctx, s.pool, workspaceB, "boot", key); err != nil {
		t.Fatalf("EnsureAPIKeyInWorkspace: %v", err)
	}

	var workspace string
	var rows int
	if err := s.pool.QueryRow(ctx,
		`SELECT workspace_id, count(*) OVER () FROM api_keys WHERE key_hash = $1`,
		sha256Hex(key)).Scan(&workspace, &rows); err != nil {
		t.Fatalf("read the configured key: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows for one key value = %d, want 1 — one secret, one workspace", rows)
	}
	if workspace != workspaceB {
		t.Errorf("workspace_id = %q, want %q — the key stayed in the workspace it was first configured in",
			workspace, workspaceB)
	}
	// Loud, because nothing else would ever mention it: this runs once at boot.
	got := warnings()
	for _, want := range []string{"another workspace", "default", workspaceB} {
		if !strings.Contains(got, want) {
			t.Errorf("move warning does not mention %q:\n%s", want, got)
		}
	}
}

// The other half of the same decision: re-configuring a value in the workspace
// it already belongs to is an ordinary boot. It must not move the row and must
// not cry wolf, or every restart of every deployment logs a tenancy warning.
func TestEnsureAPIKeyIsQuietWhenTheWorkspaceIsUnchanged(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	registerWorkspace(t, s.pool, workspaceB)

	const key = "ak-rotated-in-place"
	if err := api.EnsureAPIKeyInWorkspace(ctx, s.pool, workspaceB, "boot", key); err != nil {
		t.Fatalf("first EnsureAPIKeyInWorkspace: %v", err)
	}
	warnings := captureWarnings(t)
	if err := api.EnsureAPIKeyInWorkspace(ctx, s.pool, workspaceB, "boot", key); err != nil {
		t.Fatalf("second EnsureAPIKeyInWorkspace: %v", err)
	}

	var workspace string
	if err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM api_keys WHERE key_hash = $1`, sha256Hex(key)).Scan(&workspace); err != nil {
		t.Fatalf("read the configured key: %v", err)
	}
	if workspace != workspaceB {
		t.Errorf("workspace_id = %q, want %q", workspace, workspaceB)
	}
	if got := warnings(); strings.Contains(got, "workspace") {
		t.Errorf("a same-workspace re-configuration warned:\n%s", got)
	}
}

// The insert path, which has no conflict to carry a workspace across: a value
// this deployment has never seen must still land in the workspace it was
// configured for, or the move above would be the only thing tenancy-aware.
func TestEnsureAPIKeyWritesTheConfiguredWorkspaceOnAFreshKey(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	registerWorkspace(t, s.pool, workspaceB)

	const key = "ak-brand-new-in-b"
	if err := api.EnsureAPIKeyInWorkspace(ctx, s.pool, workspaceB, "boot", key); err != nil {
		t.Fatalf("EnsureAPIKeyInWorkspace: %v", err)
	}

	var org, workspace, project string
	if err := s.pool.QueryRow(ctx,
		`SELECT org_id, workspace_id, project_id FROM api_keys WHERE key_hash = $1`,
		sha256Hex(key)).Scan(&org, &workspace, &project); err != nil {
		t.Fatalf("read the configured key: %v", err)
	}
	// org_id and project_id are frozen at 'default' (plan 42 decision 3); only
	// the workspace is a real selector.
	if org != "default" || workspace != workspaceB || project != "default" {
		t.Errorf("key scope = (%s,%s,%s), want (default,%s,default)", org, workspace, project, workspaceB)
	}
}
