package worktoken_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// TestSecretIsTheReferenceWorkerEnvelope: unpadded URL-safe base64 of a JSON
// object with one key, sessions_token — what v1.66.0's sessionsTokenFromSecret
// reads (it strips padding first, so unpadded is the stricter choice).
func TestSecretIsTheReferenceWorkerEnvelope(t *testing.T) {
	secret := worktoken.Secret("wtk_abc")
	if strings.ContainsAny(secret, "=+/") {
		t.Errorf("secret %q is padded or not URL-safe", secret)
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil || len(env) != 1 || env["sessions_token"] != "wtk_abc" {
		t.Errorf("secret decodes to %s; want {\"sessions_token\":\"wtk_abc\"}", raw)
	}
}

// TestMintAndAuthenticate: the token authenticates its live item — and only
// that item, only while it is live — and nothing but its hash is at rest.
func TestMintAndAuthenticate(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess, env := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, env, sess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	item, err := q.Poll(ctx, env, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("poll: %v, %v", item, err)
	}

	token, err := worktoken.Mint(ctx, pool, item.ID.String(), sess.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, worktoken.TokenPrefix) {
		t.Errorf("token %q lacks the %s prefix", token, worktoken.TokenPrefix)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_session_tokens WHERE token_hash = $1 OR id = $1`, token).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the token itself is at rest (%d rows, %v)", stored, err)
	}
	p, err := worktoken.Authenticate(ctx, pool, token)
	if err != nil {
		t.Fatal(err)
	}
	if p != (worktoken.Principal{WorkID: item.ID.String(), SessionID: sess.String(), EnvironmentID: env.String()}) {
		t.Errorf("principal = %+v", p)
	}
	if p, _ := worktoken.Authenticate(ctx, pool, "wtk_unknown"); p != (worktoken.Principal{}) {
		t.Errorf("an unknown token authenticated as %+v", p)
	}

	// A stop keeps the token for a minute from its request, the lease aside:
	// the reference worker's wind-down and post-stop memory flush ride it.
	for _, tc := range []struct {
		name, sql string
	}{
		{"a just-stopped item", `UPDATE work_items SET state = 'stopped', stop_requested_at = now(), stopped_at = now(), lease_expires_at = NULL WHERE id = $1`},
		{"a stopping item whose frozen lease lapsed", `UPDATE work_items SET state = 'stopping', stop_requested_at = now(), stopped_at = NULL, lease_expires_at = now() - interval '1 second' WHERE id = $1`},
		// The worker's whole notice-and-flush (15 s + 30 s) fits the window.
		{"an item whose stop was requested 45 s ago", `UPDATE work_items SET state = 'stopped', stop_requested_at = now() - interval '45 seconds', stopped_at = now(), lease_expires_at = NULL WHERE id = $1`},
	} {
		if _, err := pool.Exec(ctx, tc.sql, item.ID.String()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p, err := worktoken.Authenticate(ctx, pool, token); err != nil || p.WorkID != item.ID.String() {
			t.Errorf("%s: the token does not authenticate (%+v, %v)", tc.name, p, err)
		}
	}
	// The join conditions, one at a time on the same row.
	for _, tc := range []struct {
		name, sql string
	}{
		{"a lapsed lease", `UPDATE work_items SET state = 'active', stop_requested_at = NULL, stopped_at = NULL, lease_expires_at = now() - interval '1 second' WHERE id = $1`},
		{"an item whose stop was requested a minute ago", `UPDATE work_items SET state = 'stopped', lease_expires_at = NULL, stop_requested_at = now() - interval '61 seconds', stopped_at = now() WHERE id = $1`},
		{"a re-handed-out item", `UPDATE work_items SET state = 'queued', id = 'work_00000000000000000000000000' WHERE id = $1`},
	} {
		if _, err := pool.Exec(ctx, tc.sql, item.ID.String()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p, err := worktoken.Authenticate(ctx, pool, token); err != nil || p != (worktoken.Principal{}) {
			t.Errorf("%s: the token still authenticates (%+v, %v)", tc.name, p, err)
		}
	}
	// A fresh item and token for the archived-session condition.
	if _, err := pool.Exec(ctx, `UPDATE work_items SET id = $1, state = 'queued', lease_expires_at = now() + interval '1 minute' WHERE session_id = $2`, item.ID.String(), sess.String()); err != nil {
		t.Fatal(err)
	}
	if p, err := worktoken.Authenticate(ctx, pool, token); err != nil || p.WorkID != item.ID.String() {
		t.Fatalf("the restored item does not authenticate (%+v, %v)", p, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sess.String()); err != nil {
		t.Fatal(err)
	}
	if p, err := worktoken.Authenticate(ctx, pool, token); err != nil || p != (worktoken.Principal{}) {
		t.Errorf("an archived session's token still authenticates (%+v, %v)", p, err)
	}
}
