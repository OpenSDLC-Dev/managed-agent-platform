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
	sessionID := sessionFrom(r.Context())

	var configJSON []byte
	var vaultIDs []string
	// The archived_at guard is re-applied here, not just in requireGateToken's
	// gatetoken.Authenticate: a session archived in the window between auth and
	// this read must still fail closed rather than serve one last config with
	// live secrets. An archived (or raced-deleted) session yields no row.
	err := s.pool.QueryRow(r.Context(),
		`SELECT e.config, s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 AND s.archived_at IS NULL`,
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

	creds, err := vaultresolve.Credentials(r.Context(), s.pool, s.cipher, sessionID, vaultIDs)
	if err != nil {
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
