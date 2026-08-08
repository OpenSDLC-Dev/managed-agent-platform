package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"go.opentelemetry.io/otel/codes"
)

// The MCP driver: mcp_exec items reach the session's MCP servers from this
// process — no sandbox Provision — for cloud AND self_hosted sessions alike
// (docs/plan/29_mcp-toolset.md). MCP is server-side on every environment kind:
// the SDK says so three times (BetaManagedAgentsSessionToolRunner, "MCP tools
// are server-side"), the work API has no MCP surface to poll, and a BYOC
// worker's contract is agent.tool_use + agent.custom_tool_use alone. That makes
// this the web_exec precedent rather than a new one.
//
// This pass discovers: for each MCP server the session's agent declares, it
// connects, lists the tools, and writes the listing into mcp_catalogs — or
// writes why it could not. Discovery is the whole of the item's work today; the
// brain reads the catalog when it assembles a turn.
//
// The row is the retry state, and its absence means something different from
// its presence. No row means the server has never been reached, a `failed` row
// means an attempt that did not work and is re-attempted on the next turn (the
// reference retries "on the next session.status_idle to session.status_running
// transition"), and a `ready` row is a listing the brain can offer. Nothing here
// disables a server for the rest of a session: the sources document no such
// state, and inventing one would outlive the outage that caused it.
//
// Unlike the web tools, the session's networking policy *does* constrain these
// dials. web_fetch and web_search are documented as unaffected by networking;
// an MCP server is not — `limited` networking's own `allow_mcp_servers` field
// exists to admit exactly these endpoints, which would have nothing to admit if
// the policy did not reach them. See mcpEgressAllowed.

// mcpServerRef is one entry of the resolved agent's mcp_servers array, whose
// wire shape is exactly {type: "url", name, url} (betaagent.go). Only the two
// fields a connection needs are decoded.
type mcpServerRef struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// catalogRow is one server's discovery outcome on its way to mcp_catalogs.
type catalogRow struct {
	name   string
	url    string
	tools  []mcp.Tool
	status string // "ready" or "failed", the column's CHECK
	reason string // why it failed; empty on "ready"
}

// processMCP runs one mcp_exec item to completion. It mirrors processWeb — the
// consumer span, the dead-session drain, the lease keeper, the one-commit
// settlement — minus everything sandbox.
func (e *Executor) processMCP(ctx context.Context, item *queue.Item) (err error) {
	ctx, span := consumerSpan(ctx, item, "mcp_exec")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			e.report(ctx, item, err)
		}
		span.End()
	}()

	sess, live, err := e.sessionForRun(ctx, item)
	if err != nil || !live {
		return err
	}

	pending, err := e.undiscoveredServers(ctx, item.SessionID, sess.mcpServers)
	if err != nil {
		return err
	}

	// Keep the lease across the dials: a server that answers slowly would
	// otherwise outlast a fixed TTL and lose the item mid-listing.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)
	rows, runErr := e.discoverServers(kctx, sess.networking, pending)
	if kerr := keeper.Close(); kerr != nil {
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	// A dead context (lease lost, shutdown) makes the whole pass untrustworthy
	// rather than partial: nothing is committed and the reclaim re-runs it, the
	// same all-or-nothing processWeb settles an interrupted run to. Discovery
	// is idempotent, so re-running costs round trips and nothing else.
	if runErr != nil {
		return runErr
	}
	return e.settleMCP(ctx, item, rows)
}

// undiscoveredServers narrows the agent's declared servers to those this
// session still needs reached: no catalog row, a row that failed, or a row whose
// url no longer matches what the agent declares.
//
// That last case is why the url is compared rather than only the name. A
// mid-session agent patch may repoint a server (mcp_servers is one of two
// mid-session-mutable agent fields) and the patch deletes the rows it
// invalidates in its own transaction — but a listing attributed to the wrong
// endpoint is the kind of error that would surface as a model calling tools
// that do not exist, so the driver does not depend on that delete having
// happened. An entry missing either field is skipped: there is nothing to dial
// and nothing to key a row on.
func (e *Executor) undiscoveredServers(ctx context.Context, sid domain.ID, declared []mcpServerRef) ([]mcpServerRef, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT server_name, url FROM mcp_catalogs WHERE session_id = $1 AND status = 'ready'`,
		sid.String())
	if err != nil {
		return nil, fmt.Errorf("read mcp catalog: %w", err)
	}
	defer rows.Close()
	ready := map[string]string{}
	for rows.Next() {
		var name, u string
		if err := rows.Scan(&name, &u); err != nil {
			return nil, fmt.Errorf("read mcp catalog: %w", err)
		}
		ready[name] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mcp catalog: %w", err)
	}

	var out []mcpServerRef
	for _, s := range declared {
		if s.Name == "" || s.URL == "" {
			continue
		}
		if have, ok := ready[s.Name]; ok && have == s.URL {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// discoverServers lists each server's tools, oldest declaration first. Every
// server yields a row: a server that cannot be reached is a fact about that
// server, not a fault of the work item, and faulting the item would reclaim-loop
// it against an endpoint that is simply down. The error return is the dead
// context alone.
func (e *Executor) discoverServers(ctx context.Context, net domain.Networking, servers []mcpServerRef) ([]catalogRow, error) {
	var rows []catalogRow
	for _, s := range servers {
		rows = append(rows, e.discoverServer(ctx, net, s))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("discover mcp server %q: %w", s.Name, ctx.Err())
		}
	}
	return rows, nil
}

// discoverServer reaches one server. Neither the url nor its own error text is
// echoed into the row's reason: an mcp_servers entry may carry userinfo, and the
// reason is a stored column.
func (e *Executor) discoverServer(ctx context.Context, net domain.Networking, s mcpServerRef) catalogRow {
	row := catalogRow{name: s.Name, url: s.URL, status: "failed"}

	parsed, err := url.Parse(s.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		row.reason = "the server's url is not an http or https URL"
		return row
	}
	if !mcpEgressAllowed(net, parsed.Hostname()) {
		row.reason = fmt.Sprintf("host %q is outside this session's limited networking policy "+
			"(add it to allowed_hosts, or set allow_mcp_servers)", parsed.Hostname())
		return row
	}

	conn, err := mcp.Connect(ctx, mcp.Config{URL: s.URL, HTTPClient: e.mcpHTTP})
	if err != nil {
		row.reason = err.Error()
		return row
	}
	defer func() { _ = conn.Close() }()

	tools, err := conn.ListTools(ctx)
	if err != nil {
		row.reason = err.Error()
		return row
	}
	row.status, row.reason, row.tools = "ready", "", tools
	return row
}

// mcpEgressAllowed reports whether the session's networking policy admits a dial
// to host.
//
// Only `limited` constrains. The zero value is the wire default, unrestricted,
// and it is also what a self_hosted environment presents: its config normalizes
// to exactly {"type":"self_hosted"} with no networking block at all, so a BYOC
// session's MCP egress is unconstrained — the reference documents networking on
// cloud environments only, and has nothing to say about the other kind.
//
// allow_mcp_servers "allows access to MCP server endpoints configured on the
// agent" on top of allowed_hosts, and this is only ever asked about a server the
// agent declared — discoverServers walks that array and nothing else — so under
// the flag the host is admitted without a second list to check it against.
func mcpEgressAllowed(net domain.Networking, host string) bool {
	if net.Type != domain.NetLimited {
		return true
	}
	if net.AllowMCPServers {
		return true
	}
	return egress.NewHostSet(net.AllowedHosts).Match(host)
}

// settleMCP commits the catalog rows, the follow-on turn, and the item's fate
// together, mirroring processWeb's settlement. The model turn is chained
// whatever the rows say: a session whose server is down proceeds without that
// server's tools, which is the documented behaviour, and a discovery pass that
// settled silently would leave the session waiting on a turn nothing else
// enqueues.
func (e *Executor) settleMCP(ctx context.Context, item *queue.Item, rows []catalogRow) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, r := range rows {
		tools := r.tools
		if tools == nil {
			tools = []mcp.Tool{}
		}
		toolsJSON, err := json.Marshal(tools)
		if err != nil {
			return fmt.Errorf("encode mcp catalog tools for %q: %w", r.name, err)
		}
		var reason any
		if r.reason != "" {
			reason = r.reason
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status, error, fetched_at)
			 VALUES ($1, $2, $3, $4::jsonb, $5, $6, now())
			 ON CONFLICT (session_id, server_name) DO UPDATE SET
			     url = EXCLUDED.url, tools = EXCLUDED.tools, status = EXCLUDED.status,
			     error = EXCLUDED.error, fetched_at = EXCLUDED.fetched_at`,
			item.SessionID.String(), r.name, r.url, string(toolsJSON), r.status, reason); err != nil {
			return fmt.Errorf("write mcp catalog for %q: %w", r.name, err)
		}
	}
	if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
		return err
	}
	if err := e.queue.Complete(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
