package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureWarnings routes WARN-and-above logging into a buffer for one test and
// returns a locked accessor for it.
//
// The stdlib-log save/restore is not optional, for the reason internal/worker's
// namesake documents: slog.SetDefault reroutes the standard log package into
// whatever handler it installs, and restoring only slog.Default() does not undo
// that — on the way back the previous handler IS a *defaultHandler, so
// SetDefault's type check skips the log.SetOutput call and log keeps pointing at
// this finished test's handler. Every later log.Print in this package's test
// binary would vanish into it, taking the httptest control plane's diagnostics
// with them.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	return captureLogs(t, slog.LevelWarn)
}

// captureLogs is captureWarnings at any level, for a test whose line is logged
// below WARN — a refusal the client caused, whose detail the operator keeps.
func captureLogs(t *testing.T, level slog.Level) func() string {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

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

	key := issueAPIKey(t, s, map[string]any{"name": "status-matrix"})["raw_key"].(string)
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

	key := issueAPIKey(t, s, map[string]any{"name": "expiring"})["raw_key"].(string)
	call := func() int {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": key})
		res.Body.Close()
		return res.StatusCode
	}
	// Seconds through make_interval rather than a duration string, because the
	// two grammars disagree silently on signed values: Postgres signs each
	// field, so `'-1h30m0s'::interval` is `-00:30:00`, while Go's
	// `(-90 * time.Minute).String()` is that same text and means minus ninety
	// minutes. A wrong value rather than an error is what makes a test pass for
	// the wrong reason, and the negative duration this one passes below escapes
	// only by having a single field. A number has no grammar to disagree about.
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

// Owned keys may share a name; each credential has its own lifecycle.
func TestOwnedKeysMayShareAName(t *testing.T) {
	s := newTestServer(t)
	first := issueAPIKey(t, s, map[string]any{"name": "shared"})
	second := issueAPIKey(t, s, map[string]any{"name": "shared"})
	if first["id"] == second["id"] || first["raw_key"] == second["raw_key"] {
		t.Fatal("separate issuances reused credential identity")
	}
	for _, row := range []map[string]any{first, second} {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": row["raw_key"].(string)})
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("owned names collide: %d", res.StatusCode)
		}
	}
}
