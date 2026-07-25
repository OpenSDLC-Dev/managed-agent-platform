package gatetoken_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

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
	if got, _ := gatetoken.Authenticate(ctx, pool, first); got != "" {
		t.Errorf("the prior token still authenticates (%q); it should be revoked", got)
	}
	if got, _ := gatetoken.Authenticate(ctx, pool, second); got != sess.String() {
		t.Errorf("the current token = %q, want session %q", got, sess)
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
	if got, _ := gatetoken.Authenticate(ctx, pool, token); got != "" {
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
