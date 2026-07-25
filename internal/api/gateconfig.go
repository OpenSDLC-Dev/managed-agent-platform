package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

	// Advisory only, so it must never delay the config a live gate is blocking
	// on (the gate client's fetch has a 10-second budget): the emission runs
	// detached from this request — its own goroutine, unhooked from the
	// request's cancellation (the response returning must not kill it) but with
	// its own deadline, restoring the bound the synchronous path had. Unbounded,
	// a stalled events table would accumulate one goroutine per fetch (every
	// 30s per gate), each holding pool connections the whole controlplane
	// shares. The goroutine gets only the non-secret projection of the
	// credentials, so it never retains plaintext secrets past the response.
	probes := make([]unreachableProbe, 0, len(creds))
	for _, c := range creds {
		probes = append(probes, unreachableProbe{
			credentialID: c.CredentialID, vaultID: c.VaultID,
			unrestricted: c.Unrestricted, allowedHosts: c.AllowedHosts,
		})
	}
	emitCtx, emitDone := context.WithTimeout(context.WithoutCancel(ctx), emitTimeout)
	go func() {
		defer emitDone()
		s.emitUnreachableCredentials(emitCtx, sessionID, cfg.Networking, probes)
	}()

	return gateconfig.Config{
		Networking:  cfg.Networking,
		Credentials: toGateCredentials(creds),
	}, nil
}

// emitTimeout bounds one detached advisory emission — matching the gate
// client's own fetch budget, so the async path can never outlive what the
// synchronous path was allowed.
const emitTimeout = 10 * time.Second

// unreachableProbe is the non-secret projection of a resolved credential that
// conflict detection needs. The detached emission goroutine receives these
// instead of vaultresolve.Credential so it never retains a plaintext secret.
type unreachableProbe struct {
	credentialID string
	vaultID      string
	unrestricted bool
	allowedHosts []string
}

// emitUnreachableCredentials surfaces the reference's
// credential_host_unreachable_error (a session.error variant): an
// environment_variable credential whose allowed_hosts includes a host the
// environment's networking policy does not permit — a configuration conflict
// the user should hear about, since the credential can never be substituted
// on those hosts through this environment (SDK betasessionevent.go's
// documented trigger). Detection runs on every config render (resolution is
// read-time, so an edit heals or introduces a conflict without a restart) but
// each (session, credential) conflict is emitted best-effort once —
// check-then-append, so concurrent duplicate fetches could double-emit an
// advisory event; that rarity is not worth a uniqueness constraint.
// Best-effort and asynchronous: the caller runs it in its own goroutine,
// detached from the request's cancellation but bounded by emitTimeout, so the
// config a live gate is waiting for is neither delayed nor failed over an
// advisory event, and a stalled events table cannot accumulate goroutines —
// detection or append errors are logged and swallowed.
func (s *server) emitUnreachableCredentials(ctx context.Context, sessionID string, net domain.Networking, creds []unreachableProbe) {
	// Only a limited policy refuses hosts. The zero value is the wire default,
	// unrestricted; an unknown type never reaches here (the API validates).
	if net.Type != domain.NetLimited {
		return
	}
	policy := egress.NewHostSet(net.AllowedHosts)
	for _, c := range creds {
		if c.unrestricted {
			continue // no allowed_hosts of its own; reach is the environment's call
		}
		var blocked []string
		for _, e := range c.allowedHosts {
			if !policy.CoversEntry(e) {
				blocked = append(blocked, e)
			}
		}
		if len(blocked) == 0 {
			continue
		}
		var already bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM events
			 WHERE session_id = $1 AND type = 'session.error'
			   AND payload->'error'->>'type' = 'credential_host_unreachable_error'
			   AND payload->'error'->>'credential_id' = $2)`,
			sessionID, c.credentialID).Scan(&already)
		if err != nil {
			slog.WarnContext(ctx, "gateconfig: unreachable-credential dedupe check failed", "credential", c.credentialID, "error", err)
			continue
		}
		if already {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"error": map[string]any{
				"type":          "credential_host_unreachable_error",
				"credential_id": c.credentialID,
				"vault_id":      c.vaultID,
				// Hostnames only — never a secret, never a placeholder.
				"message": "credential allowed_hosts include hosts the environment's networking policy does not permit: " +
					strings.Join(blocked, ", "),
				"retry_status": map[string]any{"type": "retrying"},
			},
		})
		if err != nil {
			slog.WarnContext(ctx, "gateconfig: unreachable-credential payload", "credential", c.credentialID, "error", err)
			continue
		}
		if _, err := s.log.Append(ctx, domain.ID(sessionID), []events.NewEvent{{
			Type: domain.EventSessionError, Payload: payload,
		}}); err != nil {
			slog.WarnContext(ctx, "gateconfig: unreachable-credential event append failed", "credential", c.credentialID, "error", err)
		}
	}
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
