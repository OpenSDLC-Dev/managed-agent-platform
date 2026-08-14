package vaultresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// ErrCredentialUnusable marks a credential that matched and cannot be used: its
// secret purged, its cipher absent, its sealed bytes unreadable, its token
// missing or unsendable. The operator has something to fix and retrying will not
// fix it.
//
// An error without it says nothing about the credential — a failed query, a
// cipher backend that timed out — and is worth a retry rather than an answer.
// The two travel out of one function and the caller's response to them differs,
// so they cannot be told apart by position.
var ErrCredentialUnusable = errors.New("the matched credential cannot be used")

// MCPCredentialFor resolves the bearer token a session's attached vaults
// register for an MCP server at serverURL — static_bearer's `token` or
// mcp_oauth's `access_token` — or "" when none of them register one, in which
// case the caller dials with whatever the URL itself carries and nothing more.
// That is what the reference documents ("When no MCP credential matches by
// mcp_server_url, the connection is attempted unauthenticated and will error if
// the server requires authentication").
//
// The token is plaintext and memory-only, like Credential.Secret — never logged,
// never stored, and never quoted into an error.
//
// Matching is by normalized URL (see normalizeMCPURL), and "the first vault with
// a match wins" — the same rule, in the same vaultIDs order, that winnersFor
// applies to environment_variable credentials by secret_name. The two cannot
// share a query: that one is hard-filtered to environment_variable, and these
// rows are keyed by a URL that has to be normalized in Go before it can be
// compared at all.
//
// Read fresh on every call, no cache, so a rotation or an archive reaches a
// running session's next dial — the property the reference calls re-resolution.
//
// A vault whose winning credential cannot be used — its secret purged, its cipher
// gone — does not fall through to a later vault. The first vault matched, and
// matching is what the rule is about; falling through would authenticate with a
// credential the reference would not have chosen. The dial then goes out
// unauthenticated and the server's refusal is the operator's signal.
func MCPCredentialFor(ctx context.Context, q Querier, cipher secrets.Cipher,
	vaultIDs []string, serverURL string) (string, error) {
	if len(vaultIDs) == 0 || serverURL == "" {
		return "", nil
	}
	// Normalized once. Every candidate row is compared against this same key, and
	// parsing the agent's URL again per row would answer the same question with
	// the same input as many times as the session has MCP credentials.
	wanted, ok := normalizeMCPURL(serverURL)
	if !ok {
		return "", nil
	}
	rows, err := q.Query(ctx,
		`SELECT c.id, c.vault_id, c.auth_type, c.auth, c.secret_ciphertext, c.secret_key_id
		    FROM vault_credentials c
		    JOIN vaults v ON v.id = c.vault_id
		  WHERE c.vault_id = ANY($1) AND c.auth_type IN ('mcp_oauth', 'static_bearer')
		    AND c.archived_at IS NULL AND v.archived_at IS NULL`,
		vaultIDs)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type mcpRow struct {
		id         string
		vaultID    string
		authType   string
		ciphertext []byte
		keyID      *string
	}
	byVault := map[string][]mcpRow{}
	for rows.Next() {
		var r mcpRow
		var authDoc []byte
		if err := rows.Scan(&r.id, &r.vaultID, &r.authType, &authDoc, &r.ciphertext, &r.keyID); err != nil {
			return "", err
		}
		var doc struct {
			MCPServerURL string `json:"mcp_server_url"`
		}
		if err := json.Unmarshal(authDoc, &doc); err != nil {
			return "", fmt.Errorf("vaultresolve: credential %s auth document: %w: %w",
				r.id, ErrCredentialUnusable, err)
		}
		if !matchesNormalized(doc.MCPServerURL, wanted) {
			continue
		}
		byVault[r.vaultID] = append(byVault[r.vaultID], r)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	for _, vid := range vaultIDs {
		matched := byVault[vid]
		if len(matched) == 0 {
			continue
		}
		// mcp_server_url is unique among a vault's active credentials, but only
		// as the literal string it was created with: two spellings that
		// normalize to one URL are two rows the 409 never saw. Sorting by id
		// makes which of them wins a fact rather than a race with the query
		// planner.
		sort.Slice(matched, func(i, j int) bool { return matched[i].id < matched[j].id })
		w := matched[0]
		if w.ciphertext == nil {
			// A matched credential with no sealed bytes is unusable, not absent.
			// Returning "no credential" here would dial anonymously on a session
			// the operator configured a credential for, and a server that happens
			// to allow anonymous access would then hide the anomaly completely.
			// (Archiving purges the secret and sets archived_at, which this query
			// already excludes, so an active row in this state is a repair or
			// restore artefact rather than a shape the API can write.)
			return "", unusable(fmt.Errorf("vaultresolve: credential %s has no sealed secret", w.id))
		}
		if cipher == nil {
			return "", unusable(fmt.Errorf(
				"vaultresolve: a cipher is required to resolve credential %s", w.id))
		}
		token, err := sealedField(ctx, cipher, w.id, w.ciphertext, w.keyID, bearerField(w.authType))
		if err != nil {
			return "", unusable(err)
		}
		// A token is about to become an Authorization header value, and net/http
		// refuses to write one containing a control character — so a token sealed
		// with a stray newline would fail every dial as a transport error, naming
		// the server and never the credential that is actually wrong.
		if !sendableAsHeader(token) {
			return "", unusable(fmt.Errorf(
				"vaultresolve: credential %s cannot be sent as a header", w.id))
		}
		return token, nil
	}
	return "", nil
}

// unusable marks an error as the credential's own fault. See
// [ErrCredentialUnusable] for what turns on the distinction.
func unusable(err error) error {
	return fmt.Errorf("%w: %w", err, ErrCredentialUnusable)
}

// sealedField opens a credential's sealed secret and reads one non-empty field
// from it — the sequence both resolvers need, spelled once.
//
// The three errors name the credential and the failure and nothing else: never
// the cipher's own error, never anything derived from the plaintext. That is
// what makes leak-safety a property of this package rather than of whichever
// backend the deployment configured.
func sealedField(ctx context.Context, cipher secrets.Cipher, id string,
	ciphertext []byte, keyID *string, field string) (string, error) {
	plain, err := cipher.Decrypt(ctx, ciphertext, deref(keyID))
	if err != nil {
		return "", fmt.Errorf("vaultresolve: cannot decrypt credential %s", id)
	}
	var sealed map[string]string
	if err := json.Unmarshal(plain, &sealed); err != nil {
		return "", fmt.Errorf("vaultresolve: malformed sealed secret for credential %s", id)
	}
	v := sealed[field]
	if v == "" {
		return "", fmt.Errorf("vaultresolve: credential %s has no %s", id, field)
	}
	return v, nil
}

// sendableAsHeader admits visible ASCII, space and tab, which is net/http's
// rule for a header field value minus its obs-text arm: httpguts also accepts
// 0x80–0xFF, and this does not. Deliberately the narrower of the two — no
// bearer token the wire defines carries those bytes, and refusing here names the
// credential where letting them through would fail the dial as a transport
// error naming only the server.
//
// Spelled out rather than imported: x/net is an indirect dependency and this is
// six lines of it.
func sendableAsHeader(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

// bearerField names the sealed field each MCP credential type carries its bearer
// token in. The query admits exactly these two types, so there is no third arm.
func bearerField(authType string) string {
	if authType == "static_bearer" {
		return "token"
	}
	return "access_token"
}
