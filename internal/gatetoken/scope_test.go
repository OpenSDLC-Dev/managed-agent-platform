package gatetoken_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// A gate token declares no tenant of its own — it inherits the session's
// (plan 42 §6.1), and stops authenticating the moment that session's workspace
// is archived, exactly as an unknown token does.
func TestAuthenticateInheritsTheSessionsScope(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	ws := domain.NewID(domain.PrefixWorkspace).String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', 'B')`, ws); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET workspace_id = $2 WHERE id = $1`, sess, ws); err != nil {
		t.Fatalf("move session: %v", err)
	}

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got, scope, err := gatetoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != sess.String() {
		t.Errorf("Authenticate = %q, want session %q", got, sess)
	}
	want := domain.Scope{OrgID: "default", WorkspaceID: ws, ProjectID: "default"}
	if scope != want {
		t.Errorf("scope = %+v, want %+v", scope, want)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE workspaces SET archived_at = now() WHERE id = $1`, ws); err != nil {
		t.Fatalf("archive workspace: %v", err)
	}
	got, scope, err = gatetoken.Authenticate(ctx, pool, token)
	if err != nil || got != "" || scope != (domain.Scope{}) {
		t.Errorf("token in an archived workspace resolved to %q %+v (%v), want nothing", got, scope, err)
	}
}
