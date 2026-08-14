package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
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

	var configJSON, agentJSON []byte
	var vaultIDs []string
	err = tx.QueryRow(ctx,
		`SELECT e.config, s.resolved_agent, s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 AND s.archived_at IS NULL
		  FOR SHARE OF s`,
		sessionID).Scan(&configJSON, &agentJSON, &vaultIDs)
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
	// request's cancellation (the response returning must not kill it) but
	// bounded in lifetime (emitTimeout) and in count (startEmission coalesces
	// per session), so a stalled events table can hold neither this response
	// nor an accumulating stack of the shared pool's connections. The
	// goroutine gets only the non-secret projection of the credentials, so it
	// never retains plaintext secrets past the response.
	probes := make([]unreachableProbe, 0, len(creds))
	for _, c := range creds {
		probes = append(probes, unreachableProbe{
			credentialID: c.CredentialID, vaultID: c.VaultID,
			unrestricted: c.Unrestricted, allowedHosts: c.AllowedHosts,
		})
	}
	s.startEmission(ctx, sessionID, cfg.Networking, probes)

	return gateconfig.Config{
		Networking:     cfg.Networking,
		MCPServerHosts: mcpGateHosts(cfg.Networking, agentJSON),
		Credentials:    toGateCredentials(creds),
	}, nil
}

// mcpGateHosts is the sandbox-egress half of allow_mcp_servers: the hosts the
// session's resolved agent declares MCP servers at, so a process inside the
// sandbox can reach the same servers the platform dials on its behalf. The
// platform's own dial is gated separately, in the executor, because it happens
// outside the sandbox entirely (internal/executor, mcpEgressAllowed).
//
// Nothing is sent for a policy that cannot use it: `unrestricted` admits every
// host already, an unrecognized policy admits none and must keep admitting none,
// and `limited` without the flag is the case the flag exists to distinguish.
// What the gate never receives, it cannot be made to admit.
//
// A malformed resolved agent yields no hosts rather than an error: the gate is
// blocking on this response for every host it may reach, and the same bytes are
// decoded — and complained about — by the brain and the executor, which is where
// a session with an unreadable agent actually fails.
func mcpGateHosts(net domain.Networking, resolvedAgent []byte) []string {
	if net.Type != domain.NetLimited || !net.AllowMCPServers {
		return nil
	}
	var agent struct {
		MCPServers []struct {
			URL string `json:"url"`
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal(resolvedAgent, &agent); err != nil {
		return nil
	}
	hosts := make([]string, 0, len(agent.MCPServers))
	for _, s := range agent.MCPServers {
		// The scheme is checked because a declaration this platform would refuse
		// to dial is not a promise of reach either: admitting its host would hand
		// the sandbox a host the flag never named.
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			continue
		}
		// A wildcard means one thing in a host set and nothing at all in a URL,
		// and the agent grammar constrains an `mcp_servers` url in no way beyond
		// being a non-empty string (parseMCPServers). So `https://*.example.com/`
		// is a declaration an author can write today, whose host the platform's
		// own dial would merely fail to resolve — while a host *set* reads it as
		// a suffix rule and would open the sandbox to every subdomain of
		// example.com. Skipped rather than escaped: nothing is listening on the
		// host it names either way.
		if strings.Contains(u.Hostname(), "*") {
			continue
		}
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// emitTimeout bounds one detached advisory emission — matching the gate
// client's own fetch budget, so the async path can never outlive what the
// synchronous path was allowed.
const emitTimeout = 10 * time.Second

// startEmission launches the detached advisory emission unless one is already
// in flight for the session. The timeout bounds each emission's lifetime and
// this coalesces their count: a client fetching faster than emissions drain
// (the interval is client-configured) gets at most one goroutine per session,
// never a stack of them contending for the shared pool. Skipping is lossless —
// detection re-runs on the next fetch. Returns whether it launched, for the
// white-box tests; production ignores it.
func (s *server) startEmission(ctx context.Context, sessionID string, net domain.Networking, probes []unreachableProbe) bool {
	if _, busy := s.emitting.LoadOrStore(sessionID, struct{}{}); busy {
		return false
	}
	emitCtx, emitDone := context.WithTimeout(context.WithoutCancel(ctx), emitTimeout)
	go func() {
		defer s.emitting.Delete(sessionID)
		defer emitDone()
		s.emitUnreachableCredentials(emitCtx, sessionID, net, probes)
	}()
	return true
}

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
