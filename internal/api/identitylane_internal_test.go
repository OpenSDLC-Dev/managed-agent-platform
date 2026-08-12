package api

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
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
