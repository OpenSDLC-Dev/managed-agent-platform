package api_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// TestEnsureEnvironmentKeyRotatesPerEnvironment pins the issuance helper: it
// registers one live worker credential per environment (Authorization: Bearer),
// storing only the hash, and a re-mint revokes the environment's previous key
// so a rotated-away value stops authenticating. Issuance is a deliberate
// divergence — the reference mints environment keys in its console, with no wire
// endpoint — so this helper is the platform's own provisioning primitive.
func TestEnsureEnvironmentKeyRotatesPerEnvironment(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	envA := createEnvironment(t, s, map[string]any{"name": "a", "config": map[string]any{"type": "self_hosted"}})
	envB := createEnvironment(t, s, map[string]any{"name": "b", "config": map[string]any{"type": "self_hosted"}})
	idA, _ := envA["id"].(string)
	idB, _ := envB["id"].(string)

	if err := api.EnsureEnvironmentKey(ctx, s.pool, idA, "ek-alpha"); err != nil {
		t.Fatalf("EnsureEnvironmentKey A: %v", err)
	}
	// Idempotent re-mint of the same value is a no-op, not a duplicate row.
	if err := api.EnsureEnvironmentKey(ctx, s.pool, idA, "ek-alpha"); err != nil {
		t.Fatalf("EnsureEnvironmentKey A (idempotent): %v", err)
	}
	if err := api.EnsureEnvironmentKey(ctx, s.pool, idB, "ek-beta"); err != nil {
		t.Fatalf("EnsureEnvironmentKey B: %v", err)
	}

	// One live row per environment; nothing stored in plaintext.
	var live int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM environment_keys WHERE revoked_at IS NULL").Scan(&live); err != nil {
		t.Fatalf("count live keys: %v", err)
	}
	if live != 2 {
		t.Fatalf("live environment_keys = %d, want 2", live)
	}
	var plaintext int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM environment_keys WHERE key_hash IN ('ek-alpha', 'ek-beta')").Scan(&plaintext); err != nil {
		t.Fatalf("scan plaintext: %v", err)
	}
	if plaintext != 0 {
		t.Fatal("environment key stored in plaintext")
	}

	// Rotation: a fresh value for env A revokes the old one but leaves B intact.
	if err := api.EnsureEnvironmentKey(ctx, s.pool, idA, "ek-alpha-2"); err != nil {
		t.Fatalf("rotate A: %v", err)
	}
	var liveForA int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL", idA).Scan(&liveForA); err != nil {
		t.Fatalf("count live keys for A: %v", err)
	}
	if liveForA != 1 {
		t.Fatalf("live keys for env A after rotation = %d, want 1", liveForA)
	}
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL", idB).Scan(&liveForA); err != nil {
		t.Fatalf("count live keys for B: %v", err)
	}
	if liveForA != 1 {
		t.Fatalf("env B key disturbed by env A rotation: live = %d, want 1", liveForA)
	}
}
