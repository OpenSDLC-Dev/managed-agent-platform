package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/jackc/pgx/v5"
)

// HTTP selects one credential. Keep the actor precedence explicit even when a
// context has both a sessions token and a selected API key.
func TestOwnedKeyMemoryActorSessionPrecedence(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyPrincipal, "apikey_selected")
	ctx = context.WithValue(ctx, ownedKeyContextKey{}, apiKeyAuthority{KeyID: "apikey_selected", OwnerID: "principal_owner", OwnerKind: "service_account", Role: identity.RoleAdmin})
	ctx = context.WithValue(ctx, ctxKeyWorkSession, "sesn_worker")
	want := map[string]string{"type": "session_actor", "session_id": "sesn_worker"}
	if got := memoryActor(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("memory actor=%v want=%v", got, want)
	}
	if got := principalFrom(ctx); got != "apikey_selected" {
		t.Fatalf("audit principal=%q want the selected key", got)
	}
}

// A database outage must not become an authentication failure for a real key.
func TestOwnedKeyLookupErrorIsNotAnInvalidKey(t *testing.T) {
	failure := errors.New("fixture database unavailable")
	_, err := authenticateManagementKey(t.Context(), ownedKeyFailedQuery{failure}, "test-secret")
	if !errors.Is(err, failure) {
		t.Fatalf("lookup error=%v want original database error", err)
	}
}

type ownedKeyFailedQuery struct{ err error }

func (q ownedKeyFailedQuery) QueryRow(context.Context, string, ...any) pgx.Row { return q }
func (q ownedKeyFailedQuery) Scan(...any) error                                { return q.err }

func TestOwnedKeyRoleNoneAndPrecedence(t *testing.T) {
	for _, role := range []identity.Role{identity.RoleUser, identity.RoleDeveloper, identity.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			ctx := context.WithValue(t.Context(), ctxKeyIdentity, identityPrincipal{ID: "principal_human", Role: identity.RoleAdmin})
			ctx = context.WithValue(ctx, ownedKeyContextKey{}, apiKeyAuthority{KeyID: "apikey_selected", OwnerID: "principal_owner", Role: role})
			if err := requireRole(ctx, identity.RoleNone); err == nil {
				t.Fatal("owned key bypassed RoleNone")
			}
			if err := requireRole(ctx, identity.RoleAdmin); (err == nil) != (role == identity.RoleAdmin) {
				t.Fatalf("owned role=%s error=%v; human context must not override it", role, err)
			}
		})
	}
}
