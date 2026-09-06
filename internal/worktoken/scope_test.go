package worktoken_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
)

// A sessions token declares no tenant of its own — it inherits the session's
// (plan 42 §6.1), and stops authenticating the moment that session's workspace
// is archived, exactly as an unknown token does.
func TestAuthenticateInheritsTheSessionsScope(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, env := pgtest.NewSession(t, pool, "self_hosted")

	ws := domain.NewID(domain.PrefixWorkspace).String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', 'B')`, ws); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET workspace_id = $2 WHERE id = $1`, sess, ws); err != nil {
		t.Fatalf("move session: %v", err)
	}

	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, env, sess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	item, err := q.Poll(ctx, env, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("poll: %v, %v", item, err)
	}
	token, err := worktoken.Mint(ctx, pool, item.ID.String(), sess.String())
	if err != nil {
		t.Fatal(err)
	}

	p, err := worktoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := domain.Scope{OrgID: "default", WorkspaceID: ws, ProjectID: "default"}
	if p.Scope != want {
		t.Errorf("scope = %+v, want %+v", p.Scope, want)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE workspaces SET archived_at = now() WHERE id = $1`, ws); err != nil {
		t.Fatalf("archive workspace: %v", err)
	}
	if p, err := worktoken.Authenticate(ctx, pool, token); err != nil || p != (worktoken.Principal{}) {
		t.Errorf("token in an archived workspace authenticated as %+v (%v), want the zero principal", p, err)
	}
}
