package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// TestAPIKeyStatusGatesAuthentication walks the three stored states against the
// management lane. `inactive` is the state plan 32 adds that `revoked_at` could
// not express, and the one worth guarding hardest: it must refuse like a revoked
// key and then work again, because a disable an operator cannot undo is a
// deletion wearing a different label.
func TestAPIKeyStatusGatesAuthentication(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	const key = "ak-status-matrix"
	if err := api.EnsureAPIKey(ctx, s.pool, "status-matrix", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	call := func() int {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": key})
		res.Body.Close()
		return res.StatusCode
	}
	set := func(status string) {
		if _, err := s.pool.Exec(ctx,
			`UPDATE api_keys SET status = $1 WHERE name = 'status-matrix'`, status); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("active key: %d, want 200", got)
	}
	for _, status := range []string{"inactive", "archived"} {
		set(status)
		if got := call(); got != http.StatusUnauthorized {
			t.Errorf("%s key: %d, want 401", status, got)
		}
	}
	set("active")
	if got := call(); got != http.StatusOK {
		t.Errorf("re-activated key: %d, want 200 — a disable must be reversible", got)
	}
}

// TestAPIKeyExpiryIsEvaluatedOnEveryRequest fixes the property that made expiry
// worth doing in the query rather than in a sweeper: the key stops working the
// instant it lapses, with no job that could be down when it does. The row is
// never touched between the two calls — only the clock moves.
func TestAPIKeyExpiryIsEvaluatedOnEveryRequest(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	const key = "ak-expiring"
	if err := api.EnsureAPIKey(ctx, s.pool, "expiring", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	call := func() int {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": key})
		res.Body.Close()
		return res.StatusCode
	}
	// Seconds through make_interval rather than a duration string: Postgres does
	// not parse Go's "1h0m0s" spelling, and a silently-wrong interval here would
	// make the test pass for the wrong reason.
	expire := func(at time.Duration) {
		if _, err := s.pool.Exec(ctx,
			`UPDATE api_keys SET expires_at = now() + make_interval(secs => $1) WHERE name = 'expiring'`,
			at.Seconds()); err != nil {
			t.Fatalf("set expiry: %v", err)
		}
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("key with no expiry: %d, want 200", got)
	}
	expire(time.Hour)
	if got := call(); got != http.StatusOK {
		t.Errorf("key expiring in an hour: %d, want 200", got)
	}
	expire(-time.Second)
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("key that expired a second ago: %d, want 401", got)
	}

	// An expired key is still `active` in the column — `expired` is derived, never
	// stored. If this ever reads 'archived' or 'inactive', something started
	// writing the derived state and the sweeper failure mode is back.
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM api_keys WHERE name = 'expiring'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Errorf("stored status of an expired key = %q, want \"active\" — expiry is derived", status)
	}
}

// TestEnsureAPIKeyRecordsAMaskedHint covers the one field that cannot be
// recovered later: the plaintext is hashed on the way in, so if issuance does not
// record a hint, no listing can ever tell two keys apart.
func TestEnsureAPIKeyRecordsAMaskedHint(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name, key, want string
	}{
		{"prefixed", "sk-map-api01-AbCdefghijklmnopQRST", "sk-map-api01-AbC...QRST"},
		{"no prefix", "0123456789abcdef", "012...cdef"},
		{"too short to mask", "short", ""},
		{"exactly too short", "sk-map-api01-1234567", ""},
		{"one longer", "sk-map-api01-12345678", "sk-map-api01-123...5678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := api.EnsureAPIKey(ctx, s.pool, "hint-"+tc.name, tc.key); err != nil {
				t.Fatalf("EnsureAPIKey: %v", err)
			}
			var hint string
			if err := s.pool.QueryRow(ctx,
				`SELECT partial_key_hint FROM api_keys WHERE name = $1`, "hint-"+tc.name).Scan(&hint); err != nil {
				t.Fatalf("read hint: %v", err)
			}
			if hint != tc.want {
				t.Errorf("partial_key_hint = %q, want %q", hint, tc.want)
			}
			// Whatever the hint is, it must never be the key.
			if hint == tc.key {
				t.Error("the hint is the key itself")
			}
		})
	}
}

// TestConsoleIssuedKeysAreOutsideTheOneLiveRule is the schema half of plan 32's
// central compromise. EnsureAPIKey keeps its one-live-per-name guarantee for the
// rows it owns, while keys an admin issues may share a name freely — which the
// reference allows, verified live on 2026-08-13. The two are told apart by
// created_by, so this is the test that fails if the index is ever rebuilt on the
// name alone.
func TestConsoleIssuedKeysAreOutsideTheOneLiveRule(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if err := api.EnsureAPIKey(ctx, s.pool, "shared", "ak-shared-bootstrap"); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	for i, issuer := range []string{"principal_one", "principal_two"} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO api_keys (id, name, key_hash, created_by) VALUES ($1, 'shared', $2, $3)`,
			"apikey_issued_"+issuer, "hash-shared-"+issuer, issuer); err != nil {
			t.Fatalf("issued key %d sharing a live name was rejected: %v", i, err)
		}
	}
	// ...while a second *unissued* live row under that name is still impossible.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, 'shared', $2)`,
		"apikey_second_unissued", "hash-shared-second"); err == nil {
		t.Error("a second env-var-managed live key was accepted; rotation-by-restart is unguarded")
	}
}
