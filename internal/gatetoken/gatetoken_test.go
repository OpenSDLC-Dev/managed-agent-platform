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
// What the fix changes is not who wins but that there is a winner at all. This
// test pins the interleaving where the delete reaches the session row first:
// Ensure waits for it and then fails on the foreign key of a session that is
// gone — a clean loss, and the same outcome the cycle produced on the runs where
// the delete happened to be the survivor. The other interleaving has no loser at
// all, and is not what this test is about: the re-mint takes the session row
// first, commits, and the delete then cascades the new token away with it.
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
	waitForABlockedBackend(t, pool, ensured)

	// The cascade into session_gate_tokens happens here.
	_, delErr := del.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sess)
	if delErr == nil {
		delErr = del.Commit(ctx)
	}
	// End the delete's transaction before waiting on Ensure. A failed statement
	// leaves the transaction aborted but still holding its locks until it ends,
	// so on any delete failure — the pre-fix deadlock among them, on the runs
	// where Postgres picks the delete as its victim — Ensure would still be
	// parked behind the session row and this would hang to the package timeout
	// instead of reporting. Rolling back an already-committed tx is a no-op.
	_ = del.Rollback(ctx)
	ensureErr := <-ensured

	// A deadlock on either side is the regression itself, and the outcome
	// assertions below would only restate it under a wrong name.
	for _, e := range []struct {
		who string
		err error
	}{{"the delete", delErr}, {"Ensure", ensureErr}} {
		if pgCodeIs(e.err, pgDeadlockDetected) {
			t.Fatalf("%s was aborted as a deadlock: %v", e.who, e.err)
		}
	}
	if delErr != nil {
		t.Errorf("the delete did not win the race cleanly: %v", delErr)
	}
	// And Ensure loses it the way the comment above says it does. Without this,
	// any non-deadlock outcome passes — a nil error included, which would mean a
	// token minted for a session that no longer exists.
	if !pgCodeIs(ensureErr, pgForeignKeyViolation) {
		t.Errorf("Ensure = %v, want the foreign key to refuse a deleted session (%s)",
			ensureErr, pgForeignKeyViolation)
	}
}

// pgCodeIs reports whether err carries the Postgres SQLSTATE code.
func pgCodeIs(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// waitForABlockedBackend returns once exactly one backend on this test's
// database is waiting for a lock — the form internal/queue and internal/api use
// for the same wait, kept identical so a Postgres upgrade has one dialect to
// revisit rather than one more. The count is exact rather than "some backend"
// so it answers about this test's blocked backend and not the database's mood;
// pgtest gives a fresh database per test, so nothing else can be waiting, and
// the poller's own query is running rather than waiting so it never self-counts.
// It watches ensured as well, because an Ensure that fails without ever blocking
// would otherwise be reported as a timeout with its own error thrown away.
func waitForABlockedBackend(t *testing.T, pool *pgxpool.Pool, ensured <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case err := <-ensured:
			t.Fatalf("Ensure returned before it ever blocked: %v", err)
		default:
		}
		var blocked int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&blocked); err != nil {
			t.Fatalf("count blocked backends: %v", err)
		}
		if blocked == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for Ensure to block, saw %d blocked backend(s); the race this test exists for did not happen", blocked)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
