package apikey_test

import (
	"context"
	"os"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// A key authenticates while its row is active and unexpired by the database's
// clock, and an unknown, archived or expired key returns no id and no error.
func TestAuthenticate(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	for _, row := range []struct{ id, key, status, expires string }{
		{"key_active", "sk-active", "active", "NULL"},
		{"key_future", "sk-future", "active", "now() + interval '1 hour'"},
		{"key_archived", "sk-archived", "archived", "NULL"},
		{"key_expired", "sk-expired", "active", "now() - interval '1 second'"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO api_keys (id, name, key_hash, status, expires_at) VALUES ($1, $1, $2, $3, `+row.expires+`)`,
			row.id, apikey.Hash(row.key), row.status); err != nil {
			t.Fatal(err)
		}
	}
	for key, want := range map[string]string{
		"sk-active": "key_active", "sk-future": "key_future",
		"sk-archived": "", "sk-expired": "", "sk-unknown": "",
	} {
		got, err := apikey.Authenticate(ctx, pool, key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("Authenticate(%s) = %q, want %q", key, got, want)
		}
	}
}

// Hash is the hex SHA-256 the api_keys rows are keyed by, so a row written by
// one server authenticates on the other.
func TestHash(t *testing.T) {
	const want = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b" // sha256("secret")
	if got := apikey.Hash("secret"); got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}
}

func TestCheckBrainKey(t *testing.T) {
	const boot = "sk-map-boot"
	for brain, refused := range map[string]bool{
		"":               false,
		"sk-map-brain":   false,
		"sk map brain":   false, // inner whitespace survives HTTP
		boot:             true,
		boot + " ":       true,
		"\tsk-map-brain": true,
		"sk-map-brain\n": true,
		"sk-map-brain\r": true,
	} {
		if err := apikey.CheckBrainKey(boot, brain); (err != nil) != refused {
			t.Errorf("%q: %v, want refused %v", brain, err, refused)
		}
	}
}
