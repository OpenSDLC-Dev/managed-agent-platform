package vaultresolve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mcpServer = "https://mcp.example.com/mcp"

// newSealedMCPCred inserts an ACTIVE mcp_oauth or static_bearer credential whose
// bearer token is sealed through the cipher, the way the API writes one. serverURL
// is stored verbatim, so a test can register a credential under a spelling that
// only matches after normalization.
func newSealedMCPCred(t *testing.T, pool *pgxpool.Pool, cipher secrets.Cipher,
	vaultID, authType, serverURL, token string) string {
	t.Helper()
	field := "access_token"
	if authType == "static_bearer" {
		field = "token"
	}
	sealed, err := json.Marshal(map[string]string{field: token})
	if err != nil {
		t.Fatalf("marshal sealed: %v", err)
	}
	ct, keyID, err := cipher.Encrypt(context.Background(), sealed)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	return insertMCPCred(t, pool, vaultID, authType, serverURL, ct, keyID, false)
}

// insertMCPCred inserts an MCP credential with the given sealed material
// verbatim, so a test can hand it a purged (NULL) or foreign ciphertext.
func insertMCPCred(t *testing.T, pool *pgxpool.Pool, vaultID, authType, serverURL string,
	ciphertext []byte, keyID string, archived bool) string {
	t.Helper()
	id := domain.NewID("vcrd").String()
	auth, err := json.Marshal(map[string]string{"type": authType, "mcp_server_url": serverURL})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	archivedAt := "NULL"
	if archived {
		archivedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO vault_credentials
		   (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key, archived_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, `+archivedAt+`)`,
		// The API's own key (internal/api/vaultcredauth.go), so the partial
		// unique index over a vault's active credentials rejects here exactly
		// what it rejects in production — a fixture free to write two active
		// rows for one URL would let a test pin a state the platform cannot
		// reach.
		id, vaultID, authType, auth, ciphertext, keyID, "url:"+serverURL); err != nil {
		t.Fatalf("insert mcp cred: %v", err)
	}
	return id
}

// Each credential type carries its bearer token in a field of its own, and the
// resolver has to read the one its type names — reading the other would resolve
// an empty token and dial unauthenticated with a credential in hand.
func TestMCPCredentialReadsTheTokenFieldItsTypeNames(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	for _, row := range []struct{ authType, token string }{
		{"static_bearer", "lin_api_static"},
		{"mcp_oauth", "xoxp-access"},
	} {
		t.Run(row.authType, func(t *testing.T) {
			v := newVault(t, pool, false)
			id := newSealedMCPCred(t, pool, cipher, v, row.authType, mcpServer, row.token)

			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
			if err != nil {
				t.Fatal(err)
			}
			if got != row.token {
				t.Errorf("credential %s in vault %s resolved %q, want %q", id, v, got, row.token)
			}
		})
	}
}

// "When multiple vaults contain a matching credential, the first vault with a
// match wins." Asserted by reversing the order and watching the winner change,
// so a resolver that ignored the order and returned whichever row came back
// first could not pass both halves.
func TestMCPCredentialFirstVaultWithAMatchWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	v1, v2 := newVault(t, pool, false), newVault(t, pool, false)
	newSealedMCPCred(t, pool, cipher, v1, "static_bearer", mcpServer, "first-vault")
	newSealedMCPCred(t, pool, cipher, v2, "static_bearer", mcpServer, "second-vault")

	for _, row := range []struct {
		name   string
		vaults []string
		want   string
	}{
		{"v1 first", []string{v1, v2}, "first-vault"},
		{"v2 first", []string{v2, v1}, "second-vault"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, row.vaults, mcpServer)
			if err != nil {
				t.Fatal(err)
			}
			if got != row.want {
				t.Fatalf("resolved %v, want the token %q", got, row.want)
			}
		})
	}
}

// The normalization is wired into the lookup, not merely unit-tested beside it:
// a credential registered under a spelling that differs only in the four ways
// the reference normalizes away still authenticates the dial, and one differing
// in a way it does not still does not.
func TestMCPCredentialMatchesTheServerAcrossNormalizedSpellings(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	for _, row := range []struct {
		name     string
		stored   string
		resolves bool
	}{
		{"cased host, default port and a trailing slash", "HTTPS://MCP.Example.com:443/mcp/", true},
		// net/http removes an empty port before the request goes out, so this is
		// the same origin written a way url.Parse keeps and the wire does not.
		{"an empty port", "https://mcp.example.com:/mcp", true},
		{"a different path", "https://mcp.example.com/other", false},
		{"a non-default port", "https://mcp.example.com:8443/mcp", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := newVault(t, pool, false)
			newSealedMCPCred(t, pool, cipher, v, "static_bearer", row.stored, "tok")

			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != row.resolves {
				t.Fatalf("a credential stored for %q resolved %v for %q, want %v",
					row.stored, got != "", mcpServer, row.resolves)
			}
		})
	}
}

// An archived credential, and every credential of an archived vault, is gone:
// archiving purges the secret, so resolving one would dial with nothing.
func TestMCPCredentialSkipsArchivedCredentialsAndVaults(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	archivedVault := newVault(t, pool, true)
	newSealedMCPCred(t, pool, cipher, archivedVault, "static_bearer", mcpServer, "in-an-archived-vault")

	liveVault := newVault(t, pool, false)
	insertMCPCred(t, pool, liveVault, "static_bearer", mcpServer, []byte("ct"), "test-key", true)

	got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{archivedVault, liveVault}, mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("resolved %q from an archived vault or credential, want nothing", got)
	}
}

// A session whose vaults register nothing for this server dials unauthenticated
// — the reference's documented no-match behaviour — rather than erroring or
// borrowing another server's token.
func TestMCPCredentialResolvesNothingForAnUnregisteredServer(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	v := newVault(t, pool, false)
	newSealedMCPCred(t, pool, cipher, v, "static_bearer", "https://other.example.com/mcp", "not-yours")

	got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("resolved %q for a server it is not registered for", got)
	}
}

// The first vault matched, so it won — even though its secret is gone. Falling
// through to the next vault would authenticate the dial with a credential the
// documented rule did not choose, and answering "no credential" would send the
// dial out anonymously on a session an operator configured a credential for.
func TestMCPCredentialAWinningVaultWithAPurgedSecretFailsRatherThanFallsThrough(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	purged, live := newVault(t, pool, false), newVault(t, pool, false)
	insertMCPCred(t, pool, purged, "static_bearer", mcpServer, nil, "", false)
	newSealedMCPCred(t, pool, cipher, live, "static_bearer", mcpServer, "the-later-vaults-token")

	got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{purged, live}, mcpServer)
	if got != "" {
		t.Fatalf("resolved %q, want nothing: the first vault matched and had no secret", got)
	}
	if err == nil {
		t.Fatal("a matched credential with no sealed secret must fail, not read as absent")
	}
	if !strings.Contains(err.Error(), "sealed secret") {
		t.Errorf("error = %v, want it to name the missing secret", err)
	}
}

// A cipher-less deployment holding a matching credential is a misconfiguration,
// not a silent unauthenticated dial — the same fail-closed stance Credentials
// takes. A session with no matching credential needs no cipher at all.
func TestMCPCredentialNilCipherFailsClosedOnlyOnAMatch(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	v := newVault(t, pool, false)
	newSealedMCPCred(t, pool, cipher, v, "static_bearer", mcpServer, "tok")
	if _, err := vaultresolve.MCPCredentialFor(ctx, pool, nil, []string{v}, mcpServer); err == nil {
		t.Fatal("expected an error resolving a matching credential with a nil cipher")
	}
	if _, err := vaultresolve.MCPCredentialFor(ctx, pool, nil, []string{v},
		"https://elsewhere.example.com/mcp"); err != nil {
		t.Fatalf("no match must need no cipher: %v", err)
	}

	// No attached vaults: no query at all, whatever the server URL says.
	cq := &countingQuerier{}
	got, err := vaultresolve.MCPCredentialFor(ctx, cq, nil, nil, mcpServer)
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want no token and no error for no attached vaults", got, err)
	}
	if cq.calls != 0 {
		t.Fatalf("querier called %d times for empty vaultIDs, want 0", cq.calls)
	}
}

// The cipher's error text may quote its own inputs, so it must not reach an
// error a caller can log — the same guarantee Credentials makes.
func TestMCPCredentialErrorNeverEchoesCipherError(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	v := newVault(t, pool, false)
	insertMCPCred(t, pool, v, "static_bearer", mcpServer, []byte("ciphertext"), "test-key", false)

	const marker = "MCP-CIPHER-MARKER"
	_, err := vaultresolve.MCPCredentialFor(ctx, pool, leakyCipher{marker: marker}, []string{v}, mcpServer)
	if err == nil {
		t.Fatal("expected a decrypt failure")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("resolution error echoed the cipher's message: %v", err)
	}
}

// The query admits two credential types, which is what lets bearerField decide
// the token's field name from the type alone. A row of any other type is not an
// MCP credential however its auth document reads — asserted with the corruption
// the type filter is the guard against, since the API cannot write one.
func TestMCPCredentialIgnoresARowOfAnotherType(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	sealed, err := json.Marshal(map[string]string{"secret_value": "an-env-var-secret"})
	if err != nil {
		t.Fatal(err)
	}
	ct, keyID, err := cipher.Encrypt(ctx, sealed)
	if err != nil {
		t.Fatal(err)
	}
	v := newVault(t, pool, false)
	id := domain.NewID("vcrd").String()
	auth, err := json.Marshal(map[string]any{
		"type": "environment_variable", "secret_name": "MCP_TOKEN",
		"mcp_server_url":     mcpServer,
		"networking":         map[string]string{"type": "unrestricted"},
		"injection_location": map[string]bool{"header": true, "body": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'environment_variable', $3::jsonb, $4, $5, $6)`,
		id, v, auth, ct, keyID, "name:MCP_TOKEN"); err != nil {
		t.Fatalf("insert env cred: %v", err)
	}

	got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("resolved an environment_variable row as an MCP credential: %q", got)
	}
}

// A sealed document that does not carry the field its type names leaves nothing
// to send. Refusing is fail-closed: handing back an empty token would dial with
// a bare "Authorization: Bearer" and read as a credential that was applied.
func TestMCPCredentialRefusesASealedSecretMissingItsTokenField(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	// A static_bearer sealed under mcp_oauth's field name, and the reverse.
	for _, row := range []struct{ authType, sealedField string }{
		{"static_bearer", "access_token"},
		{"mcp_oauth", "token"},
	} {
		t.Run(row.authType+" sealed as "+row.sealedField, func(t *testing.T) {
			sealed, err := json.Marshal(map[string]string{row.sealedField: "wrong-field"})
			if err != nil {
				t.Fatal(err)
			}
			ct, keyID, err := cipher.Encrypt(ctx, sealed)
			if err != nil {
				t.Fatal(err)
			}
			v := newVault(t, pool, false)
			insertMCPCred(t, pool, v, row.authType, mcpServer, ct, keyID, false)

			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
			if err == nil {
				t.Fatalf("resolved %v, want a refusal: the sealed secret carries only %s",
					got, row.sealedField)
			}
			if strings.Contains(err.Error(), "wrong-field") {
				t.Errorf("the refusal echoed the sealed value: %v", err)
			}
		})
	}
}

// A token that cannot be written into a header is refused here, where the
// credential can be named, rather than at the dial — net/http rejects a header
// value with a control character in it, and that error names the server and says
// nothing about the credential that is actually wrong.
func TestMCPCredentialRefusesATokenNoHeaderCanCarry(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	for _, row := range []struct {
		name, token string
		usable      bool
	}{
		{name: "a trailing newline", token: "lin_api_secret\n"},
		{name: "an embedded CRLF", token: "a\r\nX-Injected: 1"},
		{name: "a raw NUL", token: "lin\x00secret"},
		// Every shape a bearer token actually takes still resolves. The guard is
		// narrower than net/http's rule by its obs-text arm alone (see
		// sendableAsHeader), and these rows are what stops it narrowing further.
		{name: "punctuation and case", token: "sk-Live_1234.abc/xyz+=", usable: true},
		{name: "a tab", token: "tok\ttok", usable: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := newVault(t, pool, false)
			newSealedMCPCred(t, pool, cipher, v, "static_bearer", mcpServer, row.token)

			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
			if row.usable {
				if err != nil || got != row.token {
					t.Fatalf("resolved %q, %v; want the token back", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolved %q, want a refusal: no header can carry it", got)
			}
			if !errors.Is(err, vaultresolve.ErrCredentialUnusable) {
				t.Errorf("error = %v, want it marked as the credential's own fault", err)
			}
			if strings.Contains(err.Error(), row.token) {
				t.Errorf("the refusal echoed the token: %v", err)
			}
		})
	}
}

// mcp_server_url is unique among a vault's active credentials only as the
// literal string it was created with, so two spellings of one server are two
// rows the 409 never saw. Which of them wins must be a fact rather than a race
// with the query planner — asserted by resolving repeatedly.
func TestMCPCredentialBreaksAnIntraVaultTieDeterministically(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	v := newVault(t, pool, false)
	a := newSealedMCPCred(t, pool, cipher, v, "static_bearer", mcpServer, "token-a")
	b := newSealedMCPCred(t, pool, cipher, v, "static_bearer", mcpServer+"/", "token-b")
	winner := "token-b"
	if a < b {
		winner = "token-a"
	}

	for i := 0; i < 5; i++ {
		got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
		if err != nil {
			t.Fatal(err)
		}
		if got != winner {
			t.Fatalf("resolve %d returned %q, want the lower credential id to win with %q", i, got, winner)
		}
	}
}
