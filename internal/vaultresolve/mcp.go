package vaultresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// MCPCredential is the bearer credential one MCP dial authenticates with: the
// token, and the ids of the credential and vault it came from, which are
// non-secret and are what a log or an error may name.
//
// Token is plaintext and memory-only, like Credential.Secret — never logged,
// never stored, and never quoted into an error.
type MCPCredential struct {
	CredentialID string // vcrd_…
	VaultID      string // vlt_… — the vault that won the match
	Token        string // static_bearer.token, or mcp_oauth.access_token
}

// MCPCredentialFor resolves the credential a session's attached vaults register
// for an MCP server at serverURL, or nil when none of them do — in which case
// the caller dials unauthenticated, which is what the reference documents ("When
// no MCP credential matches by mcp_server_url, the connection is attempted
// unauthenticated and will error if the server requires authentication").
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
	vaultIDs []string, serverURL string) (*MCPCredential, error) {
	if len(vaultIDs) == 0 || serverURL == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx,
		`SELECT c.id, c.vault_id, c.auth_type, c.auth, c.secret_ciphertext, c.secret_key_id
		    FROM vault_credentials c
		    JOIN vaults v ON v.id = c.vault_id
		  WHERE c.vault_id = ANY($1) AND c.auth_type IN ('mcp_oauth', 'static_bearer')
		    AND c.archived_at IS NULL AND v.archived_at IS NULL`,
		vaultIDs)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		var doc struct {
			MCPServerURL string `json:"mcp_server_url"`
		}
		if err := json.Unmarshal(authDoc, &doc); err != nil {
			return nil, fmt.Errorf("vaultresolve: credential %s auth document: %w", r.id, err)
		}
		if !matchesMCPServer(doc.MCPServerURL, serverURL) {
			continue
		}
		byVault[r.vaultID] = append(byVault[r.vaultID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
			return nil, nil // purged secret: this vault won and has nothing to send
		}
		if cipher == nil {
			return nil, errors.New("vaultresolve: a cipher is required to resolve credential secrets")
		}
		// As in Credentials: name the credential and the failure, never wrap the
		// cipher error or anything derived from the plaintext.
		plain, err := cipher.Decrypt(ctx, w.ciphertext, deref(w.keyID))
		if err != nil {
			return nil, fmt.Errorf("vaultresolve: cannot decrypt credential %s", w.id)
		}
		var sealed map[string]string
		if err := json.Unmarshal(plain, &sealed); err != nil {
			return nil, fmt.Errorf("vaultresolve: malformed sealed secret for credential %s", w.id)
		}
		token := sealed[bearerField(w.authType)]
		if token == "" {
			return nil, fmt.Errorf("vaultresolve: credential %s has no %s", w.id, bearerField(w.authType))
		}
		return &MCPCredential{CredentialID: w.id, VaultID: w.vaultID, Token: token}, nil
	}
	return nil, nil
}

// bearerField names the sealed field each MCP credential type carries its bearer
// token in. The query admits exactly these two types, so there is no third arm.
func bearerField(authType string) string {
	if authType == "static_bearer" {
		return "token"
	}
	return "access_token"
}
