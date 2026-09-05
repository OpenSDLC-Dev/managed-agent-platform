package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	stdnet "net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
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

	var configJSON, mcpServersJSON []byte
	var vaultIDs []string
	err = tx.QueryRow(ctx,
		// Only the declared servers of the resolved agent, not the whole document:
		// a gate re-fetches this every poll for the life of a session, and the
		// rest of an agent spec — system prompt, tools, skills — is bytes nothing
		// here reads.
		`SELECT e.config, s.resolved_agent->'mcp_servers', s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 AND s.archived_at IS NULL
		  FOR SHARE OF s`,
		sessionID).Scan(&configJSON, &mcpServersJSON, &vaultIDs)
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
	// Every live child thread's declared servers too (plan 35 decision 14):
	// each roster member dials its own agent's list, and a sidecar that knew
	// only the coordinator's would 403 a child's dial.
	declared := [][]byte{mcpServersJSON}
	rows, err := tx.Query(ctx,
		`SELECT agent->'mcp_servers' FROM session_threads
		  WHERE session_id = $1 AND parent_thread_id IS NOT NULL AND archived_at IS NULL`, sessionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		declared = append(declared, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
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
	// The conflict detection is told what the gate is told, or it reports a
	// credential as unreachable on exactly the hosts this handler just widened
	// the gate by — and that error is permanent and deduped, so a working
	// configuration would carry a false one for the life of the session.
	var endpoints []string
	var refused int
	seen := map[string]bool{}
	for _, raw := range declared {
		eps, n := mcpGateEndpoints(cfg.Networking, raw)
		refused += n
		for _, e := range eps {
			if !seen[e] {
				seen[e] = true
				endpoints = append(endpoints, e)
			}
		}
	}
	if refused > 0 {
		// The declaration is dropped silently on the wire — there is no session
		// event for it, and the sandbox only ever sees the ordinary 403 the gate
		// gives any host it does not admit. So the count is said here, where an
		// operator holding a 403 and a server the platform's own catalog listed
		// can find the reason. The urls themselves are not logged: an
		// mcp_servers url may carry a credential in its userinfo or query
		// (internal/mcp redacts one for exactly that reason).
		slog.WarnContext(ctx, "gateconfig: MCP server declarations cannot widen the sandbox's egress",
			"session", sessionID, "refused", refused, "admitted", len(endpoints))
	}
	s.startEmission(ctx, sessionID, cfg.Networking, hostsOf(endpoints), probes)

	return gateconfig.Config{
		Networking:         cfg.Networking,
		MCPServerEndpoints: endpoints,
		Credentials:        toGateCredentials(creds),
	}, nil
}

// mcpGateEndpoints is the sandbox-egress half of allow_mcp_servers: the
// endpoints the session's resolved agent declares MCP servers at, as
// `host:port`, so a process inside the sandbox can reach the same servers the
// platform dials on its behalf. The platform's own dial is gated separately, in
// the executor, because it happens outside the sandbox entirely
// (internal/executor, mcpEgressAllowed).
//
// Host **and** port, unlike `allowed_hosts`: the reference widens by "MCP server
// endpoints configured on the agent", an endpoint is what an agent declares, and
// the two lists have different authors. An operator writing `allowed_hosts`
// opens every port on a host deliberately; an agent author naming one MCP
// endpoint should not thereby open the SSH port beside it.
//
// Nothing is sent for a policy that cannot use it: `unrestricted` admits every
// host already, an unrecognized policy admits none and must keep admitting none,
// and `limited` without the flag is the case the flag exists to distinguish.
// What the gate never receives, it cannot be made to admit.
//
// A declared array that will not decode — including the SQL NULL a resolved
// agent with no mcp_servers projects to — yields nothing rather than an error:
// the gate is blocking on this response for every host it may reach, and the
// same bytes are decoded, and complained about, by the brain and the executor,
// which is where a session with an unreadable agent actually fails. Wholesale
// rather than per-entry, deliberately, because that is what every other reader
// of the array does: the brain fails the turn on an unreadable mcp_servers
// (internal/brain, TestACorruptMCPServerSpecFailsTheTurn) and the executor's
// decode errors, so a document nothing else will act on grants no reach here
// either.
//
// The second return is how many declarations were refused, for the caller to
// say out loud — a refusal is invisible to the sandbox, which sees only the
// ordinary 403.
func mcpGateEndpoints(net domain.Networking, declared []byte) ([]string, int) {
	if net.Type != domain.NetLimited || !net.AllowMCPServers {
		return nil, 0
	}
	var servers []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(declared, &servers); err != nil {
		return nil, 0
	}
	endpoints := make([]string, 0, len(servers))
	for _, s := range servers {
		if e, ok := mcpEndpoint(s.URL); ok {
			endpoints = append(endpoints, e)
		}
	}
	return endpoints, len(servers) - len(endpoints)
}

// mcpEndpoint reduces one declared url to the `host:port` the gate matches on,
// or refuses it. The refusals are the declarations that would widen the gate
// past the server they name — which is the whole risk of this list, since an
// `mcp_servers` url passes no grammar at all (parseMCPServers requires a
// non-empty string and nothing more).
func mcpEndpoint(raw string) (string, bool) {
	// A declaration this platform would refuse to dial is not a promise of reach
	// either: admitting its host would hand the sandbox a host the flag never
	// named.
	//
	// One consequence is worth naming rather than leaving to be rediscovered.
	// The grammar below refuses a non-ASCII host, so a server declared as a
	// U-label never enters the set, while every allowed_hosts list accepts one
	// and stores its A-label (plan 43). Widening what the *gate admits* is a
	// different decision from validating an operator's list, so this stays as it
	// was; #614 tracks it.
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	host := u.Hostname()
	// A wildcard means one thing in a host set and nothing at all in a URL, so
	// `https://*.example.com/` — which an author can write today — would open
	// every subdomain of example.com while the platform's own dial of it merely
	// failed to resolve. It is refused first because the grammar below is an
	// operator's, in which a `*.` prefix is a legitimate entry.
	if strings.Contains(host, "*") {
		return "", false
	}
	// The rest of the grammar is the one an operator's allowed_hosts pass, asked
	// of the same function rather than restated: a bare hostname or an IPv4
	// literal, so an empty host, an empty DNS label, a malformed dotted-numeric
	// address, and an IPv6 literal are all out. The IPv6 refusal matters on its
	// own — the gate cannot match one consistently, since a CONNECT target always
	// carries a port and loses its brackets while a plain-HTTP url on the default
	// port keeps them — and it is worth nothing that the two lists reach it by
	// the same rule.
	if egress.ValidateHostEntry(host) != nil {
		return "", false
	}
	// An address the platform's own MCP client would refuse — loopback,
	// link-local (cloud metadata), the unspecified address, multicast, and the
	// IPv4 targets hidden inside the IPv6 transition forms. Only a literal can be
	// judged here; a *name* is judged where it is resolved, at the gate's dial.
	if ip := stdnet.ParseIP(host); ip != nil && dialguard.IPAllowed(ip) != nil {
		return "", false
	}
	port, ok := endpointPort(u)
	if !ok {
		return "", false
	}
	// The same spelling the gate's endpointKey produces, by the same function:
	// a no-op beyond the fold today, because ValidateHostEntry above refuses a
	// non-ASCII host and an empty label, so neither the conversion nor the
	// de-rooting can move this string. The two sides must agree by construction
	// rather than by both happening to see an ASCII host with no trailing dot.
	return egress.CanonicalLookup(host) + ":" + port, true
}

// endpointPort is the port half of an endpoint: the url's own, or the one a
// client assumes for its scheme. A port no server can listen on is refused
// rather than keyed into the gate's set, and the digits are canonicalized —
// `:0443` is the port Go's dialer will resolve it to, so the entry says 443.
func endpointPort(u *url.URL) (string, bool) {
	raw := u.Port()
	if raw == "" {
		if u.Scheme == "https" {
			return "443", true
		}
		return "80", true
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || n == 0 {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
}

// hostsOf drops the port from each endpoint, for the one consumer that asks a
// host-shaped question: a credential's allowed_hosts carry no ports.
func hostsOf(endpoints []string) []string {
	hosts := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		// mcpEndpoint proved the host colon-free, so the cut always splits.
		host, _, _ := strings.Cut(e, ":")
		hosts = append(hosts, host)
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
func (s *server) startEmission(ctx context.Context, sessionID string, net domain.Networking,
	mcpHosts []string, probes []unreachableProbe) bool {
	if _, busy := s.emitting.LoadOrStore(sessionID, struct{}{}); busy {
		return false
	}
	emitCtx, emitDone := context.WithTimeout(context.WithoutCancel(ctx), emitTimeout)
	go func() {
		defer s.emitting.Delete(sessionID)
		defer emitDone()
		s.emitUnreachableCredentials(emitCtx, sessionID, net, mcpHosts, probes)
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
func (s *server) emitUnreachableCredentials(ctx context.Context, sessionID string, net domain.Networking,
	mcpHosts []string, creds []unreachableProbe) {
	// Only a limited policy refuses hosts. The zero value is the wire default,
	// unrestricted; an unknown type never reaches here (the API validates).
	if net.Type != domain.NetLimited {
		return
	}
	// The gate admits more than allowed_hosts when the policy says so — the
	// agent's declared MCP hosts (mcpGateEndpoints) and the curated package
	// registries — and a credential naming one of those is reachable there.
	// Judged on the host alone: a credential's allowed_hosts carry no port, so
	// this is the coarser of the two questions the gate asks and errs toward not
	// reporting a conflict — the direction that matters for an advisory error
	// that is emitted once and never retracted. The registry set is read from the
	// function the gate builds its own from, so within a build the two cannot
	// disagree. Across a rolling upgrade they can — a live session keeps the gate
	// image it started on while the control plane is replaced — and what that
	// costs is bounded to this advisory arriving when it should not, or not
	// arriving when it should. Never to what the sandbox reaches: the gate
	// enforces the set it compiled and is always self-consistent, so the wire
	// carries the flag and not the list.
	var registries []string
	if net.AllowPackageManagers {
		registries = egress.PackageRegistryHosts()
	}
	policy := egress.NewHostSet(slices.Concat(net.AllowedHosts, mcpHosts, registries))
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
