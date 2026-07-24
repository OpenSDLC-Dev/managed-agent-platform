package vaultresolve_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// newVault inserts a vault (active unless archived) and returns its id.
func newVault(t *testing.T, pool *pgxpool.Pool, archived bool) string {
	t.Helper()
	id := domain.NewID("vlt").String()
	archivedAt := "NULL"
	if archived {
		archivedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO vaults (id, display_name, archived_at) VALUES ($1, 'fixture', %s)`, archivedAt),
		id); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	return id
}

// newEnvCred inserts an environment_variable credential with the given
// secret_name into a vault, active unless archived.
func newEnvCred(t *testing.T, pool *pgxpool.Pool, vaultID, secretName string, archived bool) {
	t.Helper()
	id := domain.NewID("vcrd").String()
	auth := fmt.Sprintf(`{"type":"environment_variable","secret_name":%q,`+
		`"networking":{"type":"unrestricted"},"injection_location":{"body":true,"header":true}}`, secretName)
	archivedAt := "NULL"
	if archived {
		archivedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, cred_key, archived_at)
		             VALUES ($1, $2, 'environment_variable', $3::jsonb, $4, %s)`, archivedAt),
		id, vaultID, auth, "name:"+secretName); err != nil {
		t.Fatalf("insert env cred: %v", err)
	}
}

// newMCPCred inserts a non-env-var credential, which resolution must ignore. Its
// auth doc deliberately also carries a secret_name — a shape no real
// static_bearer credential has — so that its exclusion pins the query's
// auth_type = 'environment_variable' filter: without that filter it would
// resolve to an MCP_TOKEN binding and fail the caller's assertion, whereas the
// empty-secret_name skip alone would not distinguish it.
func newMCPCred(t *testing.T, pool *pgxpool.Pool, vaultID string) {
	t.Helper()
	id := domain.NewID("vcrd").String()
	auth := `{"type":"static_bearer","mcp_server_url":"https://mcp.example.com","secret_name":"MCP_TOKEN"}`
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, cred_key)
		 VALUES ($1, $2, 'static_bearer', $3::jsonb, $4)`,
		id, vaultID, auth, "url:https://mcp.example.com"); err != nil {
		t.Fatalf("insert mcp cred: %v", err)
	}
}

func names(bindings []vaultresolve.Binding) []string {
	out := make([]string, len(bindings))
	for i, b := range bindings {
		out[i] = b.SecretName
	}
	return out
}

func TestBindings(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess := domain.NewID("sesn").String()

	v1 := newVault(t, pool, false)
	v2 := newVault(t, pool, false)
	newEnvCred(t, pool, v1, "API_KEY", false)
	newEnvCred(t, pool, v1, "DB_URL", false)
	newEnvCred(t, pool, v2, "API_KEY", false) // collides with v1's API_KEY
	newEnvCred(t, pool, v2, "V2_ONLY", false)
	newEnvCred(t, pool, v1, "OLD_KEY", true) // archived: excluded
	newMCPCred(t, pool, v1)                  // non-env-var: excluded

	t.Run("resolves active env-var creds, first vault wins on collision", func(t *testing.T) {
		got, err := vaultresolve.Bindings(ctx, pool, sess, []string{v1, v2})
		if err != nil {
			t.Fatal(err)
		}
		// API_KEY appears in both vaults but collapses to one binding; the
		// archived OLD_KEY and the mcp credential are absent.
		want := []string{"API_KEY", "DB_URL", "V2_ONLY"}
		gotNames := names(got)
		sort.Strings(gotNames)
		if strings.Join(gotNames, ",") != strings.Join(want, ",") {
			t.Fatalf("secret_names = %v, want %v", gotNames, want)
		}
		// Every binding carries a distinct, well-formed placeholder.
		seen := map[string]struct{}{}
		for _, b := range got {
			if !strings.HasPrefix(b.Placeholder, egress.PlaceholderPrefix) {
				t.Errorf("binding %q placeholder %q lacks the egress prefix", b.SecretName, b.Placeholder)
			}
			if _, dup := seen[b.Placeholder]; dup {
				t.Errorf("placeholder %q reused across bindings", b.Placeholder)
			}
			seen[b.Placeholder] = struct{}{}
		}
	})

	t.Run("vault order does not change the deduped set", func(t *testing.T) {
		// First-vault-wins picks a different winning row when the order flips,
		// but the secret_name set (and thus the injected env keys) is identical.
		got, err := vaultresolve.Bindings(ctx, pool, sess, []string{v2, v1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d bindings, want 3", len(got))
		}
	})

	t.Run("placeholders are stable across resolutions of the same session", func(t *testing.T) {
		// The load-bearing property for the gate: the sandbox binds its env at
		// create and keeps it across re-provisions, so a re-resolution must yield
		// the same secret_name→placeholder mapping, not fresh tokens.
		first, err := vaultresolve.Bindings(ctx, pool, sess, []string{v1, v2})
		if err != nil {
			t.Fatal(err)
		}
		second, err := vaultresolve.Bindings(ctx, pool, sess, []string{v1, v2})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, b := range first {
			m[b.SecretName] = b.Placeholder
		}
		for _, b := range second {
			if m[b.SecretName] != b.Placeholder {
				t.Errorf("%s placeholder drifted: %q then %q", b.SecretName, m[b.SecretName], b.Placeholder)
			}
		}
	})

	t.Run("no attached vaults resolves to nothing", func(t *testing.T) {
		got, err := vaultresolve.Bindings(ctx, pool, sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d bindings, want 0", len(got))
		}
	})

	t.Run("an unknown vault id contributes nothing", func(t *testing.T) {
		got, err := vaultresolve.Bindings(ctx, pool, sess, []string{domain.NewID("vlt").String()})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d bindings, want 0", len(got))
		}
	})
}

func TestBindingsCorruptAuthDocErrors(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	// A row whose auth document has a non-string secret_name (only reachable by
	// out-of-band corruption — the API always writes a string) surfaces as a
	// resolution error, not a silently dropped credential.
	v := newVault(t, pool, false)
	id := domain.NewID("vcrd").String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, cred_key)
		 VALUES ($1, $2, 'environment_variable', '{"secret_name":123}'::jsonb, $3)`,
		id, v, "name:corrupt"); err != nil {
		t.Fatalf("insert corrupt cred: %v", err)
	}

	if _, err := vaultresolve.Bindings(ctx, pool, domain.NewID("sesn").String(), []string{v}); err == nil {
		t.Fatal("expected an error resolving a corrupt auth document, got nil")
	}
}

func TestBindingsArchivedVaultExcluded(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	// An archived vault yields nothing — the revocation half the acceptance run
	// asserts (a fresh resolution mints no placeholder). The credential here is
	// left *active*, so exclusion rests on the query's direct vault archived_at
	// guard, not on the archive cascade having archived the credential row: the
	// "archived vault delivers no credential" guarantee holds even against a
	// stale un-cascaded row.
	v := newVault(t, pool, true)
	newEnvCred(t, pool, v, "STALE", false)

	got, err := vaultresolve.Bindings(ctx, pool, domain.NewID("sesn").String(), []string{v})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bindings for an archived vault, want 0", len(got))
	}
}
