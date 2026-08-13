package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// sha256Hex is the stored form of a key, spelled out here rather than reached for
// across the package boundary: a test that stages a row the way the code would
// proves nothing about the format the code actually uses. This is the second,
// independent statement of it.
func sha256Hex(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

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
		// A key this platform minted: the prefix is public by construction, so it
		// shows in full.
		{"issued", api.IssuedKeyPrefix + "AbCdefghijklmnopQRST", api.IssuedKeyPrefix + "AbC...QRST"},
		{"issued, exactly too short", api.IssuedKeyPrefix + "1234567890abc", ""},
		{"issued, exactly long enough", api.IssuedKeyPrefix + "1234567890abcd", api.IssuedKeyPrefix + "123...abcd"},

		// A real minted shape: 43 base64url characters, whose alphabet contains
		// `-`. This row is why the split is taken at a fixed offset from the known
		// prefix. Under a "cut at the last dash" rule the mask would start after
		// the body's own dash and render this as
		// "sk-map-api01-jJS0rezEtM3wgUCBCwDAZWdaspykfF55zo-Wr4...63cs", publishing
		// all but three characters of a live credential.
		{
			"issued, dash inside the base64url body",
			api.IssuedKeyPrefix + "jJS0rezEtM3wgUCBCwDAZWdaspykfF55zo-Wr4E63cs",
			api.IssuedKeyPrefix + "jJS...63cs",
		},

		// An operator-chosen value: nothing in it is public, so only the tail
		// shows, and only when the value is long enough that four characters are a
		// small fraction of it.
		{"operator value with a dash", "secret-12345678", ""},
		{"operator value, long enough", "secret-1234567890abcdef", "...cdef"},
		{"opaque, exactly long enough", "0123456789abcdef", "...cdef"},
		{"opaque, one short", "0123456789abcde", ""},
		{"too short to mask", "short", ""},
		{"empty", "", ""},

		// Runes, not bytes. Slicing this by byte would cut mid-rune and produce
		// invalid UTF-8, which Postgres refuses on a text column — turning a
		// cosmetic field into a control plane that will not boot.
		{"multi-byte", "パスワード-これはとてもながいかぎです", "...かぎです"},
		{"multi-byte, too short", "éééé", ""},
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
			// A bound, not a spot check. Every `want` above is hand-written, so a
			// bad rule and a bad expectation would agree with each other and the
			// comparison alone would bless the result. This asserts the property
			// the masking exists for, against whatever was actually produced: a
			// hint must never show more of the secret part of a key than it hides.
			// It matters beyond tidiness — key_hash is an unsalted SHA-256, so a
			// mostly-published plaintext is recoverable offline.
			if hint != "" {
				secret := strings.TrimPrefix(tc.key, api.IssuedKeyPrefix)
				shown := utf8.RuneCountInString(
					strings.ReplaceAll(strings.TrimPrefix(hint, api.IssuedKeyPrefix), "...", ""))
				if hidden := utf8.RuneCountInString(secret) - shown; hidden < shown {
					t.Errorf("hint %q shows %d of the key's %d secret characters, hiding only %d",
						hint, shown, utf8.RuneCountInString(secret), hidden)
				}
			}
		})
	}
}

// TestEnsureAPIKeyAdoptsAnIssuedRowAsEnvManaged covers the conflict path, where
// the value configured in CONTROLPLANE_API_KEY turns out to already exist as a row
// somebody issued from the console. Naming a value there makes it env-var-managed,
// whatever it was before, and both inherited fields would otherwise break that:
// an inherited created_by leaves the row outside api_keys_one_live_unissued, so
// the next rotation's archive skips it and the name carries two live credentials
// (#72, reopened); an inherited expires_at lets a control plane boot reporting
// success over a bootstrap key that authenticate() already refuses.
func TestEnsureAPIKeyAdoptsAnIssuedRowAsEnvManaged(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	const key = "ak-adopted"
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, status, created_by, expires_at)
		 VALUES ('apikey_adopted', 'issued-then-configured', $1, 'inactive', 'principal_someone',
		         now() - interval '1 hour')`,
		sha256Hex(key)); err != nil {
		t.Fatalf("stage issued key: %v", err)
	}

	if err := api.EnsureAPIKey(ctx, s.pool, "boot", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}

	var (
		name, status string
		createdBy    *string
		expiresAt    *time.Time
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT name, status, created_by, expires_at FROM api_keys WHERE id = 'apikey_adopted'`).
		Scan(&name, &status, &createdBy, &expiresAt); err != nil {
		t.Fatalf("read adopted row: %v", err)
	}
	if name != "boot" || status != "active" {
		t.Errorf("adopted row = (%q, %q), want (\"boot\", \"active\")", name, status)
	}
	if createdBy != nil {
		t.Errorf("created_by = %q, want NULL — the row is env-var-managed now", *createdBy)
	}
	if expiresAt != nil {
		t.Errorf("expires_at = %v, want NULL — an inherited expiry boots a dead bootstrap key", *expiresAt)
	}

	// The whole point: the adopted key actually authenticates.
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": key})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("adopted key: %d, want 200", res.StatusCode)
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
