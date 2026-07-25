package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5"
)

// getGateConfig serves GET /internal/v1/gate/config to a session's egress gate,
// authenticated by its per-session gate token (requireGateToken has already put
// the session id in the context). It returns the environment's request-level
// networking policy and the session's resolved, decrypted vault credentials —
// the two inputs internal/gate needs. The plaintext secrets exist only in this
// response body and the resolving process's memory; the resolution error carries
// credential ids, never secret bytes (vaultresolve.Credentials).
func (s *server) getGateConfig(r *http.Request) (any, error) {
	ctx := r.Context()
	sessionID := sessionFrom(ctx)

	// The session read and the credential resolution run in one transaction that
	// holds a FOR SHARE lock on the session row, so a session archive (an UPDATE
	// on that row) cannot commit between the archived_at check and the credential
	// read. Without the shared lock the two autocommit reads leave a window in
	// which a session archived mid-request could still be served one last config
	// with live secrets; with it, an archived (or raced-deleted) session fails
	// closed. Mirrors the executor's FOR UPDATE OF s session read. The guard is
	// also re-applied here, not only in requireGateToken's gatetoken.Authenticate.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var configJSON []byte
	var vaultIDs []string
	err = tx.QueryRow(ctx,
		`SELECT e.config, s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 AND s.archived_at IS NULL
		  FOR SHARE OF s`,
		sessionID).Scan(&configJSON, &vaultIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		// Authenticated, but the session was archived or deleted between auth and
		// this read — re-auth (fail-closed), never a partial config.
		return nil, errAuth("gate token no longer valid")
	}
	if err != nil {
		return nil, err
	}

	var cfg domain.EnvironmentConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, err
	}

	creds, err := vaultresolve.Credentials(ctx, tx, s.cipher, sessionID, vaultIDs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return gateconfig.Config{
		Networking:  cfg.Networking,
		Credentials: toGateCredentials(creds),
	}, nil
}

// toGateCredentials projects resolved credentials onto the gate wire shape. The
// credential's networking arm is unrestricted or limited{allowed_hosts}; the
// environment-level widening flags are not a credential concern.
func toGateCredentials(creds []vaultresolve.Credential) []gateconfig.Credential {
	out := make([]gateconfig.Credential, 0, len(creds))
	for _, c := range creds {
		netType := domain.NetLimited
		if c.Unrestricted {
			netType = domain.NetUnrestricted
		}
		out = append(out, gateconfig.Credential{
			CredentialID: c.CredentialID,
			Placeholder:  c.Placeholder,
			Secret:       c.Secret,
			Networking: gateconfig.CredentialNetworking{
				Type:         netType,
				AllowedHosts: c.AllowedHosts,
			},
			InjectionLocation: gateconfig.InjectionLocation{
				Header: c.Header,
				Body:   c.Body,
			},
		})
	}
	return out
}
