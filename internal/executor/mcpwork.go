package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
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
	rows, runErr := e.discoverServers(kctx, sess.envConfig, pending)
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
// context alone — the item's own, not the pass's budget.
//
// The pass is bounded as a whole (Config.MCPDiscoveryTimeout), the way
// mcp.ListTimeout bounds one listing across its pages. Without an aggregate
// bound the worst case is the per-server bound times the servers an agent may
// declare: twenty (maxAgentMCPServers), each costing a handshake at
// mcp.DialTimeout plus a listing at mcp.ListTimeout, is the better part of an
// hour — and the lease keeper renews throughout, so nothing reclaims the item
// and cuts it short.
//
// It bounds the work rather than the wall clock: the budget stops the pass
// dialling anything further, and the call already in flight when it expires
// takes as long as the client's own cancellation does to unwind (see
// mcp.DialTimeout on why that is seconds rather than immediate).
func (e *Executor) discoverServers(ctx context.Context, cfg domain.EnvironmentConfig, servers []mcpServerRef) ([]catalogRow, error) {
	budget, cancel := context.WithTimeout(ctx, e.cfg.MCPDiscoveryTimeout)
	defer cancel()

	var rows []catalogRow
	for _, s := range servers {
		// The budget is spent, but the item's own context is alive: the servers
		// still unreached are this pass's failures, not the item's. They get the
		// ordinary failed row, which the next turn re-attempts.
		if budget.Err() != nil && ctx.Err() == nil {
			rows = append(rows, catalogRow{name: s.Name, url: s.URL, status: "failed",
				reason: "this discovery pass ran out of time before reaching the server"})
			continue
		}
		rows = append(rows, e.discoverServer(budget, cfg, s))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("discover mcp server %q: %w", s.Name, ctx.Err())
		}
	}
	return rows, nil
}

// discoverServer reaches one server.
//
// The reason it writes is a stored column and an mcp_servers entry may carry
// userinfo, so the reasons written here name the host and nothing more. The two
// that come from elsewhere are covered at their source rather than here: an
// error out of the MCP client names the endpoint with its userinfo redacted,
// and net/http redacts its own half of a transport message.
func (e *Executor) discoverServer(ctx context.Context, cfg domain.EnvironmentConfig, s mcpServerRef) catalogRow {
	row := catalogRow{name: s.Name, url: s.URL, status: "failed"}

	parsed, err := url.Parse(s.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		row.reason = "the server's url is not an http or https URL"
		return row
	}
	if !mcpEgressAllowed(cfg, parsed.Hostname()) {
		row.reason = egressRefusal(cfg, parsed.Hostname())
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

// storableTools is the catalog's last gate before Postgres: it strips NUL from
// the strings a server controls, and drops a tool whose input schema carries one.
//
// Postgres `jsonb` cannot store `\u0000` inside a string value - the same limit
// toolset.SanitizeText exists for on the event log - and here the consequence is
// worse than a rejected write. A failed INSERT faults the item *before*
// queue.Complete, so the lease lapses, the reclaim re-lists the same server, and
// the settlement fails again: one NUL in one tool description, from a server the
// platform does not control, wedges that session's MCP discovery for good.
//
// Names and descriptions are Go strings and are stripped. A schema is raw JSON,
// where a NUL is not a byte but the six characters `\u0000` - stripping that
// textually would corrupt a schema whose own text happens to contain a literal
// backslash-u-0000, so a schema carrying one costs its tool instead, the way
// listTools already drops an entry it cannot faithfully hand to a model. A raw
// 0x00 byte cannot arrive: JSON forbids it inside a string, and the decode that
// produced these bytes would have rejected it.
func storableTools(tools []mcp.Tool) []mcp.Tool {
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if bytes.Contains(t.InputSchema, []byte(`\u0000`)) {
			continue
		}
		t.Name = toolset.SanitizeText(t.Name)
		t.Description = toolset.SanitizeText(t.Description)
		out = append(out, t)
	}
	return out
}

// maxCatalogReason caps a stored failure reason. A `failed` row is re-attempted
// on every turn and rewritten each time, and the text on it is a server's to
// choose: MCP response bodies are bounded at mcp.MaxResponseBytes, so a
// JSON-RPC error message arrives able to carry megabytes into a column with no
// length of its own. Nothing reading a reason needs more than the first lines
// of it. (A `ready` row's tools need no such cap: they are written once per
// (session, server, url) — undiscoveredServers skips a server already ready at
// the url the agent declares — and each entry is validated on the way out of
// the client.)
const maxCatalogReason = 2000

// urlInText matches a URL as it appears inside an error message. The trailing
// class is deliberately wide: it is trimmed below, and matching too much and
// trimming is safe where matching too little would leave a credential behind.
var urlInText = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// storableReason is the catalog's gate for the text half of a row, as
// storableTools is for the tools half.
//
// It redacts first. An mcp_servers entry is customer-supplied and may carry a
// credential in its userinfo or its query — `?api_key=` is a common MCP server
// convention — and a failure reason is derived text that reaches a stored
// column, which later slices surface to the model and the API. The client
// redacts the endpoint in its own messages, but that covers only the password
// (url.URL.Redacted), and only the messages it writes: net/http names the full
// URL in a transport error, and a server's own error text may quote it back.
// So every URL-shaped substring is cut down to scheme://host here, at the
// boundary where the text becomes storage, rather than at each of the places it
// can come from. That covers the credential as this platform put it on the
// wire, since a URL is the only shape it travels in; what it does not cover is a
// server quoting its own copy back in an error message of its own writing,
// which no text rule could catch and which tells that server nothing it did not
// already receive.
//
// Then NUL, for the reason storableTools gives, and then the cap. Truncation is
// re-validated as UTF-8: cutting a byte slice mid-rune leaves a sequence
// Postgres rejects on a text column, which is the same wedge the NUL strip
// exists to prevent, arriving by the door opened to prevent it.
func storableReason(reason string) string {
	reason = urlInText.ReplaceAllStringFunc(reason, redactURL)
	reason = toolset.SanitizeText(reason)
	if len(reason) > maxCatalogReason {
		reason = strings.ToValidUTF8(reason[:maxCatalogReason], "") + "…"
	}
	return reason
}

// redactURL reduces one matched URL to its scheme and host, keeping whatever
// sentence punctuation the match swept up so the surrounding text still reads.
func redactURL(match string) string {
	var trailing string
	for len(match) > 0 && strings.ContainsRune(`.,:;!?)]}"'`, rune(match[len(match)-1])) {
		trailing, match = match[len(match)-1:]+trailing, match[:len(match)-1]
	}
	u, err := url.Parse(match)
	if err != nil || u.Host == "" {
		return "[redacted url]" + trailing
	}
	return u.Scheme + "://" + u.Host + trailing
}

// egressRefusal says why the dial was refused, and there are exactly two
// reasons because mcpEgressAllowed has exactly two ways to say no.
//
// They are told apart because the advice differs and one of them is advice an
// operator cannot act on: a config whose networking names no recognized policy
// refuses every host, so telling its owner to add this one to `allowed_hosts`
// sends them to a list that is not being consulted — and they may well have put
// it there already, since a malformed block can carry both admitting fields and
// still be refused.
func egressRefusal(cfg domain.EnvironmentConfig, host string) string {
	if cfg.Networking.Type == domain.NetLimited {
		return fmt.Sprintf("host %q is not admitted by this environment's `limited` networking policy "+
			"(add it to allowed_hosts, or set allow_mcp_servers)", host)
	}
	return fmt.Sprintf("this environment's networking policy is %q, which the platform does not recognize, "+
		"so it admits no host at all — %q included", cfg.Networking.Type, host)
}

// mcpEgressAllowed reports whether the environment's networking policy admits a
// dial to host.
//
// It decides on the environment **kind** first and treats an unrecognized
// networking type as no policy at all, which is `gate.newPolicy`'s shape and is
// the safe direction: a self_hosted environment has no networking block by
// construction — its config normalizes to exactly {"type":"self_hosted"}, and
// the reference documents networking on cloud environments only — while a cloud
// environment always has one through the API, so a cloud config that names no
// recognized policy is malformed rather than permissive. Reading a missing
// discriminator as "unrestricted" would make that malformed row the one that
// reaches everything: the schema constrains `config->>'type'` against the kind
// and nothing constrains `config->'networking'`, and a config-preserving update
// (packages, description) carries a stored value forward without revalidating
// it.
//
// allow_mcp_servers "allows access to MCP server endpoints configured on the
// agent" on top of allowed_hosts, and this is only ever asked about a server the
// agent declared — discoverServers walks that array and nothing else — so under
// the flag the host is admitted without a second list to check it against.
func mcpEgressAllowed(cfg domain.EnvironmentConfig, host string) bool {
	if cfg.Type == domain.EnvSelfHosted {
		return true
	}
	switch cfg.Networking.Type {
	case domain.NetUnrestricted:
		return true
	case domain.NetLimited:
		if cfg.Networking.AllowMCPServers {
			return true
		}
		return egress.NewHostSet(cfg.Networking.AllowedHosts).Match(host)
	default:
		return false
	}
}

// settleMCP commits the catalog rows, the follow-on turn, and the item's fate
// together, mirroring processWeb's settlement. The model turn is chained
// whatever the rows say: a session whose server is down proceeds without that
// server's tools, which is the documented behaviour, and a discovery pass that
// settled silently would leave the session waiting on a turn nothing else
// enqueues.
//
// It settles under the session row lock, and re-reads what the agent declares
// from inside it, because a mid-session agent patch can land while the dials
// are in flight — `mcp_servers` is one of two agent fields a running session may
// patch. Without the lock, a patch that removes a server between the pass's read
// and this write finds no row to invalidate and then has one written behind it,
// for a server the agent no longer declares and that nothing will ever revisit.
// With it, both orders are correct: a patch that commits first is reflected in
// what is read here, and one that commits after runs its own delete over what
// this wrote.
func (e *Executor) settleMCP(ctx context.Context, item *queue.Item, rows []catalogRow) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var agentJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT resolved_agent FROM sessions WHERE id = $1 FOR UPDATE`,
		item.SessionID.String()).Scan(&agentJSON); err != nil {
		return fmt.Errorf("read session agent: %w", err)
	}
	var agent struct {
		MCPServers []mcpServerRef `json:"mcp_servers"`
	}
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		return fmt.Errorf("decode session agent: %w", err)
	}
	declared := make(map[string]string, len(agent.MCPServers))
	for _, s := range agent.MCPServers {
		declared[s.Name] = s.URL
	}

	for _, r := range rows {
		if declared[r.name] != r.url {
			continue
		}
		toolsJSON, err := json.Marshal(storableTools(r.tools))
		if err != nil {
			return fmt.Errorf("encode mcp catalog tools for %q: %w", r.name, err)
		}
		var reason any
		if r.reason != "" {
			reason = storableReason(r.reason)
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
	// The turn is chained on the same condition every other settlement uses.
	// Discovery is not an answer to a tool call, so ordinarily nothing is
	// outstanding and the brain is woken here — but mcp_exec and tool_exec are
	// different queue kinds, so nothing stops one being in flight while the
	// other settles, and a model_turn sent with an agent.tool_use still
	// unanswered is a request the Messages API rejects. Whichever settlement
	// finishes last finds the set answered and wakes the brain, so the gate
	// costs no wakeup.
	unanswered, err := events.HasUnansweredToolUse(ctx, tx, item.SessionID, nil)
	if err != nil {
		return err
	}
	if !unanswered {
		if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
			return err
		}
	}
	if err := e.queue.Complete(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
