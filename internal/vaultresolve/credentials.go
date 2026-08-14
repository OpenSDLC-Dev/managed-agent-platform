package vaultresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// Credential is one resolved environment_variable credential ready for the
// egress gate: the sandbox-visible Placeholder, the Secret it stands for, the
// credential's own networking half (Unrestricted, or AllowedHosts for limited),
// and the injection locations it is enabled for. It is the gate-side twin of a
// Binding — both resolve from the same winner (see winnersFor), so a placeholder
// injected into the sandbox and the secret substituted at egress always agree.
//
// Secret is plaintext: it lives only in the resolving process's memory and the
// config response body — never logged, never stored (docs/plan/12, D1).
type Credential struct {
	CredentialID string   // vcrd_… — non-secret; the substitution span's credential_id (plan 12)
	VaultID      string   // vlt_… — the containing vault; a credential_host_unreachable_error names both ids
	SecretName   string   // the sandbox env-var name; the winner key
	Placeholder  string   // egress.Placeholder(sessionID, SecretName) — identical to the Binding's token
	Secret       string   // plaintext secret_value; memory-only
	AllowedHosts []string // the limited allow-list (nil when Unrestricted)
	Unrestricted bool     // the credential's networking arm
	Header       bool     // injection_location.header
	Body         bool     // injection_location.body
}

// Credentials resolves the active environment_variable credentials of
// sessionID's attached vaults into their decrypted secrets — the gate's
// substitution set. It shares winnersFor with Bindings, so both halves select
// the same credential per secret_name (first vault in vaultIDs order wins).
//
// A cipher is required once the session has any active environment_variable
// credential (a winner) — even one whose ciphertext was purged: a vault-attached
// session on a cipher-less deployment is a misconfiguration, not a silent no-op.
// A decrypt failure or a sealed document missing its secret_value fails the whole
// call — a cipher outage or tampering deserves a loud, fail-closed error rather
// than a partial set that would let a placeholder egress as a literal with no
// signal. An active credential whose ciphertext is absent (a purge anomaly, not
// reachable through the write path) is skipped: its sandbox placeholder simply
// egresses literally, which is fail-closed. Error messages carry credential ids,
// never secret or ciphertext bytes.
func Credentials(ctx context.Context, q Querier, cipher secrets.Cipher, sessionID string, vaultIDs []string) ([]Credential, error) {
	winners, err := winnersFor(ctx, q, vaultIDs)
	if err != nil {
		return nil, err
	}
	if len(winners) == 0 {
		return nil, nil
	}
	if cipher == nil {
		return nil, errors.New("vaultresolve: a cipher is required to resolve credential secrets")
	}

	var out []Credential
	for _, w := range winners {
		if w.ciphertext == nil {
			continue // purged/absent secret: the placeholder egresses literally
		}
		var doc struct {
			Networking struct {
				Type         string   `json:"type"`
				AllowedHosts []string `json:"allowed_hosts"`
			} `json:"networking"`
			InjectionLocation struct {
				Body   bool `json:"body"`
				Header bool `json:"header"`
			} `json:"injection_location"`
		}
		if err := json.Unmarshal(w.authDoc, &doc); err != nil {
			// authDoc is the non-secret auth document; wrapping its parse error is safe.
			return nil, fmt.Errorf("vaultresolve: credential %s auth document: %w", w.id, err)
		}
		// The cipher error and the decrypted plaintext must NOT flow into a
		// resolution error a caller may log: name the credential and the failure,
		// never wrap the cipher error (%w) or the plaintext-derived parse error.
		// Leak-safety is then a property of sealedField, not of the cipher.
		secret, err := sealedField(ctx, cipher, w.id, w.ciphertext, w.keyID, "secret_value")
		if err != nil {
			return nil, err
		}
		// Reject an unknown networking arm rather than coercing it to limited: a
		// corrupt or future type carried alongside allowed_hosts would otherwise be
		// silently treated as a host allow-list and could substitute the secret.
		// Fail-closed, matching the other corruption paths above.
		unrestricted := doc.Networking.Type == string(domain.NetUnrestricted)
		if !unrestricted && doc.Networking.Type != string(domain.NetLimited) {
			return nil, fmt.Errorf("vaultresolve: credential %s has an unknown networking type", w.id)
		}
		out = append(out, Credential{
			CredentialID: w.id,
			VaultID:      w.vaultID,
			SecretName:   w.secretName,
			Placeholder:  egress.Placeholder(sessionID, w.secretName),
			Secret:       secret,
			AllowedHosts: doc.Networking.AllowedHosts,
			Unrestricted: unrestricted,
			Header:       doc.InjectionLocation.Header,
			Body:         doc.InjectionLocation.Body,
		})
	}
	return out, nil
}

// credRow is one active environment_variable credential that won its secret_name
// (winnersFor already applied first-vault-wins). authDoc is the row's non-secret
// auth document, carried so each caller parses only the fields it needs.
type credRow struct {
	id         string
	vaultID    string
	secretName string
	authDoc    []byte
	ciphertext []byte
	keyID      *string
}

// winnersFor is the single credential-selection rule shared by Bindings and
// Credentials: it reads the active environment_variable credentials of the
// attached (unarchived) vaults and returns the winning row per secret_name, with
// the first vault in vaultIDs order winning a collision. Reading current rows
// each call (no cache) is what makes rotation and archive propagate without a
// session restart (docs/plan/12, D5); the direct v.archived_at guard makes
// "an archived vault contributes nothing" independent of the archive cascade.
func winnersFor(ctx context.Context, q Querier, vaultIDs []string) ([]credRow, error) {
	if len(vaultIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx,
		`SELECT c.id, c.vault_id, c.auth, c.secret_ciphertext, c.secret_key_id
		    FROM vault_credentials c
		    JOIN vaults v ON v.id = c.vault_id
		  WHERE c.vault_id = ANY($1) AND c.auth_type = 'environment_variable'
		    AND c.archived_at IS NULL AND v.archived_at IS NULL`,
		vaultIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group rows by vault so first-vault-wins can be applied in the caller's
	// vault_ids order, which the ANY(...) query does not preserve.
	byVault := map[string][]credRow{}
	for rows.Next() {
		var r credRow
		if err := rows.Scan(&r.id, &r.vaultID, &r.authDoc, &r.ciphertext, &r.keyID); err != nil {
			return nil, err
		}
		var doc struct {
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(r.authDoc, &doc); err != nil {
			return nil, err
		}
		if doc.SecretName == "" {
			continue
		}
		r.secretName = doc.SecretName
		byVault[r.vaultID] = append(byVault[r.vaultID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []credRow
	for _, vid := range vaultIDs {
		vaultRows := byVault[vid]
		sort.Slice(vaultRows, func(i, j int) bool { return vaultRows[i].secretName < vaultRows[j].secretName })
		for _, r := range vaultRows {
			if _, dup := seen[r.secretName]; dup {
				continue // an earlier attached vault already won this secret_name
			}
			seen[r.secretName] = struct{}{}
			out = append(out, r)
		}
	}
	return out, nil
}

// deref returns the pointed-to string, or "" for a nil pointer (a NULL
// secret_key_id column).
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
