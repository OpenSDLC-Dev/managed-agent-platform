package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// 0035 holds the default workspace as a RECOGNISED row rather than a created
// one (plan 42 slice 1): every scoped row already carries workspace_id
// 'default', so the registry that gives those values a referent has to come up
// already naming it, live and unarchived. Exactly one row, because a second
// would mean the migration created a tenant nobody asked for.
func TestWorkspacesRegistryHoldsTheDefaultWorkspace(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&count); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if count != 1 {
		t.Fatalf("workspaces rows = %d, want 1", count)
	}

	var id, org, name string
	var archivedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT id, org_id, name, archived_at FROM workspaces`).
		Scan(&id, &org, &name, &archivedAt); err != nil {
		t.Fatalf("read default workspace: %v", err)
	}
	if id != "default" || org != "default" {
		t.Errorf("default workspace = (%s,%s), want (default,default)", id, org)
	}
	if name != "Default Workspace" {
		t.Errorf("default workspace name = %q, want %q", name, "Default Workspace")
	}
	if archivedAt != nil {
		t.Errorf("default workspace archived_at = %v, want NULL", *archivedAt)
	}
}
