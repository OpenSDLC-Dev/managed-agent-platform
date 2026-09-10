package gatetoken_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestMintFormat(t *testing.T) {
	a, b := gatetoken.Mint(), gatetoken.Mint()
	for _, tok := range []string{a, b} {
		if !strings.HasPrefix(tok, gatetoken.TokenPrefix) {
			t.Errorf("token %q lacks the %q prefix", tok, gatetoken.TokenPrefix)
		}
		if len(tok) <= len(gatetoken.TokenPrefix)+40 {
			t.Errorf("token %q is too short to carry 256 bits", tok)
		}
	}
	if a == b {
		t.Error("two mints returned the same token — not random")
	}
}

func TestEnsureAndAuthenticate(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := gatetoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != sess.String() {
		t.Errorf("Authenticate = %q, want session %q", got, sess)
	}
}

func TestEnsureStoresHashNotPlaintext(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM session_gate_tokens WHERE session_id = $1 AND revoked_at IS NULL`,
		sess.String()).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	// The column holds the sha256-hex of the token, never the plaintext: a
	// regression that stored the token itself would be a credential-at-rest
	// leak that every round-trip test would still pass (both sides hash
	// symmetrically), so it is asserted directly here.
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); stored != want {
		t.Errorf("token_hash = %q, want sha256-hex %q", stored, want)
	}
	if stored == token {
		t.Error("token_hash stores the plaintext token — credential-at-rest leak")
	}
}

func TestEnsureRevokesPriorToken(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	first := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), first); err != nil {
		t.Fatal(err)
	}
	second := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), second); err != nil {
		t.Fatal(err)
	}

	// One live token per session: re-minting revokes the predecessor.
	got, err := gatetoken.Authenticate(ctx, pool, first)
	if err != nil {
		t.Fatalf("Authenticate(prior): %v", err)
	}
	if got != "" {
		t.Errorf("the prior token still authenticates (%q); it should be revoked", got)
	}
	got, err = gatetoken.Authenticate(ctx, pool, second)
	if err != nil {
		t.Fatalf("Authenticate(current): %v", err)
	}
	if got != sess.String() {
		t.Errorf("the current token = %q, want session %q", got, sess)
	}
}

func TestRevokeInvalidatesLiveToken(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatal(err)
	}
	if err := gatetoken.Revoke(ctx, pool, sess.String()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, err := gatetoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate(revoked): %v", err)
	}
	if got != "" {
		t.Errorf("revoked token still authenticates to %q, want empty", got)
	}
	// Idempotent: a session with no live token (already revoked, or never
	// gated) is a no-op, so a provision retry after a failed teardown can
	// safely revoke again.
	if err := gatetoken.Revoke(ctx, pool, sess.String()); err != nil {
		t.Errorf("second Revoke: %v", err)
	}
}

func TestEnsureNonexistentSessionErrors(t *testing.T) {
	pool := pgtest.NewPool(t)
	// A gate token can only be minted for a real session (the FK); a bad session
	// id surfaces as an error, never a dangling token row.
	if err := gatetoken.Ensure(context.Background(), pool, "sesn_does_not_exist", gatetoken.Mint()); err == nil {
		t.Fatal("expected an error minting a token for a nonexistent session, got nil")
	}
}

func TestAuthenticateUnknownToken(t *testing.T) {
	pool := pgtest.NewPool(t)
	got, err := gatetoken.Authenticate(context.Background(), pool, gatetoken.Mint())
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("unknown token authenticated to %q, want empty", got)
	}
}

func TestAuthenticateArchivedSession(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sess.String()); err != nil {
		t.Fatal(err)
	}
	// An archived session's gate must fail closed — the token no longer authenticates.
	got, err := gatetoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate(archived): %v", err)
	}
	if got != "" {
		t.Errorf("archived session's token authenticated to %q, want empty", got)
	}
}

func TestAuthenticateAfterSessionDeleteCascades(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")

	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sess.String()); err != nil {
		t.Fatal(err)
	}
	// The token row is cascade-deleted; authentication is a clean empty, not an error.
	got, err := gatetoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatalf("Authenticate after cascade: %v", err)
	}
	if got != "" {
		t.Errorf("deleted session's token authenticated to %q, want empty", got)
	}
}

// TestMintWithPrefixAndHashToken: the one mint under another prefix, and the
// digest every token table stores (plan 36 decision 15's sessions token).
func TestMintWithPrefixAndHashToken(t *testing.T) {
	tok := gatetoken.MintWithPrefix("wtk_")
	if !strings.HasPrefix(tok, "wtk_") || len(tok) != len(gatetoken.Mint()) {
		t.Errorf("MintWithPrefix = %q; want a wtk_ token of Mint's length", tok)
	}
	sum := sha256.Sum256([]byte(tok))
	if got := gatetoken.HashToken(tok); got != hex.EncodeToString(sum[:]) {
		t.Errorf("HashToken = %s; want the token's sha256 hex", got)
	}
}

// pgDeadlockDetected is 40P01, which Postgres raises on whichever side of a
// cycle it chooses to abort; pgForeignKeyViolation is 23503, which is how the
// race resolves once there is no cycle to abort.
const (
	pgDeadlockDetected    = "40P01"
	pgForeignKeyViolation = "23503"
)

// A session delete racing a gate re-mint used to close a lock cycle: the delete
// holds the session row and then needs the token rows it cascades into, while
// Ensure held the token rows and then needed the session row for its insert's
// foreign key. Postgres broke the tie by aborting one side, so an ordinary race
// surfaced as a 500 or a failed gate provisioning (#313).
//
// The delete side is spelled out here rather than driven through the API,
// because those two statements *are* the ordering under test: internal/api's
// requireNotRunning takes the session FOR UPDATE, and deleteSession's DELETE
// cascades into session_gate_tokens through migration 0012. A change to either
// belongs in this test too.
//
// What the fix changes is not who wins but that there is a winner at all. Ensure
// now blocks on the session row before it touches a token row, so the delete
// finishes and Ensure fails on the foreign key of a session that is gone — a
// clean loss, and the same outcome the cycle produced on the runs where the
// delete happened to be the survivor.
func TestEnsureDoesNotDeadlockAgainstASessionDelete(t *testing.T) {
	pool := pgtest.NewPool(t)
	sess, _ := pgtest.NewSession(t, pool, "cloud")
	ctx := context.Background()

	// A live predecessor, so Ensure's revoke has a row to take a lock on. Without
	// one it updates nothing, holds nothing, and there is no cycle to reproduce.
	if err := gatetoken.Ensure(ctx, pool, sess.String(), gatetoken.Mint()); err != nil {
		t.Fatalf("seed the predecessor token: %v", err)
	}

	del, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the delete: %v", err)
	}
	defer func() { _ = del.Rollback(ctx) }()
	var status string
	if err := del.QueryRow(ctx,
		`SELECT status FROM sessions WHERE id = $1 FOR UPDATE`, sess).Scan(&status); err != nil {
		t.Fatalf("lock the session row: %v", err)
	}

	ensured := make(chan error, 1)
	go func() { ensured <- gatetoken.Ensure(ctx, pool, sess.String(), gatetoken.Mint()) }()

	// Wait for Ensure to be blocked before the delete asks for anything else.
	// Which lock it waits on is the whole difference between the two orderings,
	// and asserting that here would be asserting the fix rather than its effect
	// — so the wait only requires that it is waiting.
	waitForABlockedBackend(t, pool)

	// The cascade into session_gate_tokens happens here.
	_, delErr := del.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sess)
	if delErr == nil {
		delErr = del.Commit(ctx)
	}
	ensureErr := <-ensured

	for _, e := range []struct {
		who string
		err error
	}{{"the delete", delErr}, {"Ensure", ensureErr}} {
		var pgErr *pgconn.PgError
		if errors.As(e.err, &pgErr) && pgErr.Code == pgDeadlockDetected {
			t.Errorf("%s was aborted as a deadlock: %v", e.who, e.err)
		}
	}
	if delErr != nil {
		t.Errorf("the delete did not win the race cleanly: %v", delErr)
	}
	// And Ensure loses it the way the comment above says it does. Without this,
	// any non-deadlock outcome passes — a nil error included, which would mean a
	// token minted for a session that no longer exists.
	var ensurePG *pgconn.PgError
	if !errors.As(ensureErr, &ensurePG) || ensurePG.Code != pgForeignKeyViolation {
		t.Errorf("Ensure = %v, want the foreign key to refuse a deleted session (%s)",
			ensureErr, pgForeignKeyViolation)
	}
}

// waitForABlockedBackend returns once some backend on this test's database is
// waiting for a lock. pg_locks is cluster-wide where pgtest is per-test — a
// fixture container per test binary, a fresh database per test — so the scope is
// what ties the two together. It is not load-bearing today, since nothing else
// runs in this database, and it is here so that stays true of a reader rather
// than of the fixture's current shape.
func waitForABlockedBackend(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			  WHERE NOT l.granted AND a.datname = current_database()`).Scan(&blocked); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend ever blocked; the race this test exists for did not happen")
}
