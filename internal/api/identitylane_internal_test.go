package api

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestPrincipalFromResolvesEitherLane pins what sessions.created_by records.
//
// It is an in-package test of the resolution rule itself — bare context, machine
// lane, human lane, and both set at once, which no single HTTP request can
// exhibit. It was written when slice 2 registered every mutation at
// identity.RoleNone and no human could create a session at all; slice 3 opened
// POST /v1/sessions to developer, and the end-to-end half now has its own test
// over real HTTP (TestAHumanCreatedSessionRecordsThePrincipal). Both are worth
// keeping: the failure they guard is silent, since created_by is nullable and
// nothing downstream checks it, so the first evidence would be an audit trail
// that had been quietly recording nothing.
func TestPrincipalFromResolvesEitherLane(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if got := principalFrom(ctx); got != "" {
		t.Errorf("principalFrom on a bare context = %q, want empty", got)
	}

	machine := context.WithValue(ctx, ctxKeyPrincipal, "bootstrap")
	if got := principalFrom(machine); got != "bootstrap" {
		t.Errorf("machine lane: principalFrom = %q, want the api key's name", got)
	}

	human := context.WithValue(ctx, ctxKeyIdentity,
		identityPrincipal{ID: "principal_abc", Role: identity.RoleAdmin})
	if got := principalFrom(human); got != "principal_abc" {
		t.Errorf("identity lane: principalFrom = %q, want the human's principal id", got)
	}

	// Both set is unreachable today — dispatch picks one lane — but if it ever
	// became reachable, the machine credential is the one that authenticated the
	// request, so it is the one the audit row must name.
	both := context.WithValue(human, ctxKeyPrincipal, "bootstrap")
	if got := principalFrom(both); got != "bootstrap" {
		t.Errorf("both lanes: principalFrom = %q, want the machine principal", got)
	}
}

// The membership lookup's empty input is the one that must never widen.
// identityScope refuses a member of nothing before it gets here, so this arm is
// unreachable through a request — which is exactly why it is pinned in
// isolation: "no ids" and "every live workspace" are one guard apart, and the
// evidence that the guard went missing would be a human resolving into a tenant
// they are not a member of.
func TestLiveWorkspaceScopesAmongNothingIsNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	// A second live workspace, so "every live workspace" and "the one seeded
	// row" are distinguishable answers.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', 'B')`,
		domain.NewID(domain.PrefixWorkspace).String()); err != nil {
		t.Fatalf("insert workspace B: %v", err)
	}
	for _, ids := range [][]string{nil, {}} {
		got, err := liveWorkspaceScopesAmong(ctx, pool, ids)
		if err != nil {
			t.Fatalf("liveWorkspaceScopesAmong(%v): %v", ids, err)
		}
		if len(got) != 0 {
			t.Errorf("liveWorkspaceScopesAmong(%v) = %+v, want nothing", ids, got)
		}
	}
	// And the all-workspaces reader still answers the other question, capped at
	// the two rows its caller's decision needs.
	all, err := allLiveWorkspaceScopes(ctx, pool)
	if err != nil {
		t.Fatalf("allLiveWorkspaceScopes: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("allLiveWorkspaceScopes = %+v, want the two live workspaces", all)
	}
}
