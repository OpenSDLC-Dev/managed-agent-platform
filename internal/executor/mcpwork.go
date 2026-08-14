package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
)

// The MCP driver: mcp_exec items reach the session's MCP servers from this
// process — creating no sandbox to do it — for cloud AND self_hosted sessions
// alike (docs/plan/29_mcp-toolset.md). A cloud session that already has one is
// written into, never provisioned: an answer past the tool budget spills its
// text there (mcpspill.go). MCP is server-side on every environment kind:
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
	name string
	url  string
	// tools is the listing, already storable when set — bounded and stripped by
	// storableTools where it was produced, for the reason reason gives below: a
	// server chooses it, one listing is bounded only by a whole connection's
	// response budget, and the pass walks up to maxAgentMCPServers of them in a
	// row before anything settles.
	tools  []mcp.Tool
	status string // "ready" or "failed", the column's CHECK
	// reason is why it failed, empty on "ready", and already storable when set:
	// every producer passes it through storableReason before it lands here, so
	// nothing holds a server's unbounded text and the settlement stores it as it
	// stands.
	reason string
	// notReached marks a row whose failure is this platform's scheduling rather
	// than the server's: the pass's budget ran out before an answer came back.
	// Its reason is a fallback rather than a finding, so the settlement keeps
	// whatever an earlier pass recorded instead — and it says nothing on the
	// wire, because there is no connection for an operator to go and heal.
	notReached bool
	// authentication picks which of the wire's two failures this row is, on the
	// same split the execution path uses (mcpFailure): a refused credential is
	// not a connection that failed, because the connection worked well enough to
	// be refused. Meaningless on a "ready" row.
	authentication bool
}

// processMCP runs one mcp_exec item to completion. It mirrors processWeb — the
// consumer span, the dead-session drain, the lease keeper, the one-commit
// settlement — and runs no tool in a sandbox, reaching only for one the session
// already has when an answer has to spill (mcpspill.go).
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

	// Calls first, and only calls: a turn stopped on an unanswered call is
	// waiting on this pass, while a listing is wanted by a turn that has not
	// started. Spending this pass's budget dialling for a catalog would leave
	// that turn stopped for as long as the dials take.
	calls, err := e.unansweredMCPToolUses(ctx, item.SessionID)
	if err != nil {
		return err
	}
	if len(calls) > 0 {
		return e.answerMCPCalls(ctx, item, sess, calls)
	}

	pending, err := e.undiscoveredServers(ctx, item.SessionID, sess.mcpServers)
	if err != nil {
		return err
	}

	// Keep the lease across the dials: a server that answers slowly would
	// otherwise outlast a fixed TTL and lose the item mid-listing.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)
	rows, runErr := e.discoverServers(kctx, sess.envConfig, sess.vaultIDs, pending)
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

// readyEndpoints is the url each of this session's usable listings was read at,
// by server name. It answers the one question both MCP drivers ask of the
// catalog: which servers have a listing, and where it came from.
func (e *Executor) readyEndpoints(ctx context.Context, sid domain.ID) (map[string]string, error) {
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
	return ready, nil
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
	ready, err := e.readyEndpoints(ctx, sid)
	if err != nil {
		return nil, err
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
// The pass is bounded as a whole (Config.MCPPassTimeout), the way
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
func (e *Executor) discoverServers(ctx context.Context, cfg domain.EnvironmentConfig,
	vaultIDs []string, servers []mcpServerRef) ([]catalogRow, error) {
	budget, cancel := context.WithTimeout(ctx, e.cfg.MCPPassTimeout)
	defer cancel()

	// Every declared server is dialled, and they are dialled at once. Serially,
	// declaration order decided who got reached: one server that accepts a
	// connection and never answers spends the whole budget, and the servers
	// behind it are recorded as unreached on every turn for the life of the
	// session — so a healthy server three entries down could never enter the
	// catalog, and the model was never offered its tools. Position is not a
	// fact about a server, so it must not be what decides.
	//
	// The fan-out needs no width of its own: `mcp_servers` is capped at
	// maxAgentMCPServers entries where it is written (internal/api), and this
	// pass has already dropped the ones a `ready` row covers, so the wire's cap
	// is the bound. Nothing here is shared — each goroutine fills its own slot,
	// and the slot is its declaration's, so the rows the settlement walks are in
	// declaration order however the dials interleave.
	//
	// It bounds the hold as well as the starvation, though it does not remove
	// it: this pass runs on the one goroutine that takes this host's work items,
	// so a tarpit server still holds them — but for one server's dial-and-list
	// rather than the sum of every declared server's, and MCPPassTimeout still
	// caps that.
	rows := make([]catalogRow, len(servers))
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i], errs[i] = e.discoverServer(budget, cfg, vaultIDs, s)
		}()
	}
	wg.Wait()

	// A hard error is the item's, not a server's — a pool that blinked, a
	// listing that would not encode — so it faults the item and throws the pass
	// away rather than storing a verdict it did not earn. The first one wins;
	// they are all the same kind of failure and one is enough to fault on.
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("discover mcp server %q: %w", servers[i].Name, err)
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("discover mcp servers: %w", ctx.Err())
	}
	return rows, nil
}

// passRanOutOfTime is the reason a server the pass could not finish with
// carries. It is a fallback rather than a verdict — the settlement's upsert
// keeps a reason an earlier pass earned, because "ran out of time" says what
// this pass did and would otherwise make a policy refusal read as a scheduling
// artifact.
const passRanOutOfTime = "this discovery pass ran out of time before reaching the server"

// discoverServer reaches one server.
//
// The reason it writes is a stored column and an mcp_servers entry may carry a
// credential in its userinfo or its query, so the reasons written here name the
// host and nothing more. The ones that come from elsewhere are not covered at
// their source, and it would be easy to assume they were: the MCP client
// redacts the endpoint in the prefix *it* writes, but the transport error it
// wraps is net/http's, which names the URL it dialled with only the password
// masked — the username and query ride along. That is why storableReason
// redacts by value here rather than trusting what arrives.
func (e *Executor) discoverServer(ctx context.Context, cfg domain.EnvironmentConfig,
	vaultIDs []string, s mcpServerRef) (row catalogRow, err error) {
	// The reason is made storable here, where it is produced, rather than at
	// settlement. A server chooses this text and a JSON-RPC error message is
	// bounded only by a whole connection's response budget, so deferring the cap
	// to the end of the pass would keep every declared server's megabytes alive
	// in the executor's heap at once — and the redaction has to happen before
	// anything holds the string anyway.
	// Declared ahead of the defer so whatever line produced the reason, the
	// token that line's dial carried is scrubbed out of it.
	var token string
	defer func() {
		if row.reason != "" {
			row.reason = storableReason(row.reason, row.url, token)
		}
	}()
	row = catalogRow{name: s.Name, url: s.URL, status: "failed"}

	host, herr := mcpEndpointHost(s.URL)
	if herr != nil {
		row.reason = herr.Error()
		return row, nil
	}
	if !mcpEgressAllowed(cfg, host) {
		row.reason = egressRefusal(cfg, host)
		return row, nil
	}

	token, cerr := e.mcpBearer(ctx, vaultIDs, s.URL)
	// The clock is read first, and whether or not the credential produced a
	// verdict of its own. Resolving one can now include an OAuth refresh, which is
	// seconds of third-party I/O rather than a query, plus a cipher backend that
	// is a network call — so this is where a discovery budget realistically runs
	// out, and it runs out the same way on the resolutions that succeed. That is
	// this pass's failure, not the item's, so it settles like the budget branch
	// above rather than faulting and throwing away the rows every server before
	// this one earned.
	//
	// Reading it before the credential's verdict, because a resolution cut short
	// marks some of its failures against the credential — a decrypt that never
	// returned reads the same as one that was refused — and blaming a healthy
	// credential for the clock is a verdict this row keeps. Reading it after a
	// success, because the dial below would otherwise go out on a spent context
	// and store the timeout as a server that was reached and failed.
	if ctx.Err() != nil {
		row.reason = passRanOutOfTime
		row.notReached = true
		return row, nil
	}
	if cerr != nil {
		if credentialUnusable(cerr) {
			// A credential this platform could not resolve is an authentication
			// failure that never reached the server — the reference's third arm.
			row.reason, row.authentication = mcpDialReason(cerr), true
			return row, nil
		}
		// The lookup failed, not the credential. A failed row would blame the
		// credential for a pool that blinked; faulting the item retries the pass.
		return catalogRow{}, fmt.Errorf("mcp credential for %q: %w", s.Name, cerr)
	}

	conn, derr := mcp.Connect(ctx, mcp.Config{URL: s.URL, HTTPClient: e.mcpHTTP, BearerToken: token})
	if derr != nil {
		return failedDial(row, derr, ctx), nil
	}
	defer func() { _ = conn.Close() }()

	tools, lerr := conn.ListTools(ctx)
	if lerr != nil {
		return failedDial(row, lerr, ctx), nil
	}
	row.status, row.reason, row.tools = "ready", "", storableTools(tools, token)
	return row, nil
}

// failedDial fills in a row for a dial or a listing that did not come back.
//
// A budget spent while the request was in flight is this pass's failure and not
// the server's, so it reads as the scheduling artifact it is rather than as a
// timeout the server earned — the same verdict a server the pass could not
// start on gets, and for the same reason: the row is what an operator reads,
// and "ran out of time" must never displace a finding. The item's own
// cancellation lands here too and is indistinguishable from the budget's, which
// costs nothing: discoverServers throws every row away when the item is gone.
func failedDial(row catalogRow, err error, ctx context.Context) catalogRow {
	if ctx.Err() != nil {
		row.reason, row.notReached = passRanOutOfTime, true
		return row
	}
	row.reason, row.authentication = mcpDialReason(err), mcpAuthFailure(err)
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
// where a NUL is not a byte but the six characters `\u0000` — and those same
// six characters, written literally, are how a JSON Schema says "no control
// characters" (`"pattern": "^[^\\u0000-\\u001f]*$"`). Searching the bytes for them
// cannot tell the two apart, and reading the escape as a NUL costs a tool whose
// schema uses a routine idiom — so the schema is decoded instead and its strings
// are examined for the code point itself. A schema that does carry one costs its
// tool, the way listTools already drops an entry it cannot faithfully hand to a
// model, and so does one that will not decode at all. A raw 0x00 byte cannot
// arrive: JSON forbids it inside a string, and the decode that produced these
// bytes would have rejected it.
//
// The listing is capped as well, which the sibling text column has needed from
// the start and this one no less: `tools` is server-chosen text with no length of
// its own, bounded only by a whole connection's mcp.MaxResponseBytes budget, and
// at maxAgentMCPServers servers that is a hundred and sixty megabytes of jsonb
// for one session — copied again into every other session the same agent starts,
// since the catalog is deliberately per-session rather than shared. Tools are
// kept in the order the server reported them until the next one would not fit,
// and the rest are dropped rather than truncated: half a schema is a contract the
// server never published, and dropping is already what this function does with a
// tool it cannot store.
func storableTools(tools []mcp.Tool, secrets ...string) []mcp.Tool {
	out := make([]mcp.Tool, 0, len(tools))
	budget := maxCatalogTools
	for _, t := range tools {
		if schemaCarriesNUL(t.InputSchema) {
			continue
		}
		t.Name = scrubSecrets(toolset.SanitizeText(t.Name), secrets...)
		t.Description = scrubSecrets(toolset.SanitizeText(t.Description), secrets...)
		t.InputSchema = json.RawMessage(scrubSecrets(string(t.InputSchema), secrets...))
		size := len(t.Name) + len(t.Description) + len(t.InputSchema)
		if size > budget {
			break
		}
		budget -= size
		out = append(out, t)
	}
	return out
}

// schemaCarriesNUL reports whether a decoded input schema holds the NUL code
// point in a string or a key — the thing Postgres refuses on jsonb — as opposed
// to the escape sequence spelling it, which is ordinary schema text. A schema
// that will not decode is reported as carrying one: it cannot be vouched for,
// and the caller's answer to both is the same.
func schemaCarriesNUL(schema json.RawMessage) bool {
	var decoded any
	if err := json.Unmarshal(schema, &decoded); err != nil {
		return true
	}
	return valueCarriesNUL(decoded)
}

func valueCarriesNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.ContainsRune(t, 0)
	case []any:
		for _, e := range t {
			if valueCarriesNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range t {
			if strings.ContainsRune(k, 0) || valueCarriesNUL(e) {
				return true
			}
		}
	}
	return false
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

// maxCatalogTools caps one row's listing, counted over the names, descriptions
// and schemas a server chose — the bytes it controls — rather than over the
// marshaled row, so the cap means the same thing whatever the encoder does with
// it. A quarter of a megabyte is far past any catalog meant to be offered to a
// model: a hundred tools averaging a kilobyte of schema and prose each fit
// inside it, and a request carrying more would be an unusable one. It exists for
// the listing that is not meant to be offered at all.
const maxCatalogTools = 256 << 10

// urlInText matches a URL as it appears inside an error message.
//
// It stops only at whitespace and at the angle brackets, which are not legal
// in a URL unescaped. Notably it does *not* stop at a quote: `'` is an RFC 3986
// sub-delim and legal in a query, so a class that excluded it would end the
// match early and leave whatever followed — `?note='&api_key=SECRET` — sitting
// in the text beside a host that looks redacted. Quotes are handled by the
// trailing trim instead, which is the safe direction: over-matching costs a
// URL that reads less precisely, while under-matching costs the credential.
var urlInText = regexp.MustCompile(`(?i)https?://[^\s<>]+`)

// storableReason is the catalog's gate for the text half of a row, as
// storableTools is for the tools half.
//
// It redacts first, by value and then by shape. An mcp_servers entry is
// customer-supplied and may carry a credential in its userinfo or its query —
// `?api_key=` is a common MCP server convention — and a failure reason is
// derived text that reaches a stored column, which later slices surface to the
// model and the API. The client redacts the endpoint in its own messages, but
// that covers only the password (url.URL.Redacted), and only the messages it
// writes: net/http names the URL in a transport error, and a server's own error
// text may quote it back.
//
// By value is what makes this reliable, and shape alone is what cannot be. The
// endpoint is a string this driver holds, so the renderings a message can
// contain are enumerable — the declared bytes, url.URL's own String and
// Redacted, net/http's `***` variant, and any of those escaped through %q — and
// each is replaced outright. A pattern cannot reach the same place: a URL is
// recognized in prose by where it *ends*, and a space has to end it, yet
// `?agent=my agent&api_key=…` is a URL this platform accepts and dials, so a
// matcher would stop mid-query and leave the rest of it in the text beside a
// host that reads as redacted. The pattern pass stays as the second one, for
// the URLs this platform did not supply and cannot enumerate — a redirect
// target, or one a server names in its own message.
//
// What neither pass covers is a server quoting its own copy of the credential
// back in text of its own writing, which no rule over text could catch and
// which tells that server nothing it did not already receive. Nor is this the
// credential's first arrival at rest: the same row stores the endpoint verbatim
// in `url`, as `sessions.resolved_agent` already does. Keeping it out of
// *derived* text is the point — that is what a later slice hands to a model.
//
// A vault's bearer token is the one secret both of those arguments fail for: it
// is not in the endpoint, so no rendering of the endpoint covers it, and it is
// nowhere at rest in the clear — the vault holds it sealed. A server that reads
// its own Authorization header can quote it back in a JSON-RPC error message,
// which the SDK preserves. So callers pass it in `secrets`.
//
// It is replaced *last* of the three redactions, because the two before it
// substitute text in: a rendering's safe form is a URL, and a token that happens
// to be a substring of one — `https`, a host name — would be put back by the
// very pass that redacts the endpoint. Nothing after it inserts anything, so
// last is the only position that holds. What no ordering reaches is a server
// that transforms the token before quoting it — percent-encoded, base64'd, cut
// in half — which is the same residue the endpoint's own pass leaves, and no
// rule over text closes it.
//
// Then NUL, for the reason storableTools gives, and then UTF-8 twice, for two
// different reasons. Postgres rejects an invalid byte sequence on a text column
// exactly as it rejects a NUL, and faults the item the same way — so the text
// is validated once as it arrives, because a server chooses it and need not
// send UTF-8 at all (Go's HTTP parser accepts obs-text in a header value, and
// an SDK error that quotes such a header back carries those bytes into here),
// and once more after the cap, because cutting a byte slice mid-rune produces
// the same invalid sequence out of input that was clean.
func storableReason(reason, endpoint string, secrets ...string) string {
	for _, form := range endpointRenderings(endpoint) {
		reason = strings.ReplaceAll(reason, form.text, form.safe)
	}
	reason = urlInText.ReplaceAllStringFunc(reason, redactURL)
	reason = scrubSecrets(reason, secrets...)
	reason = toolset.SanitizeText(reason)
	reason = strings.ToValidUTF8(reason, "")
	if len(reason) > maxCatalogReason {
		reason = strings.ToValidUTF8(reason[:maxCatalogReason], "") + "…"
	}
	return reason
}

// scrubSecrets replaces each secret wherever it appears. Callers pass the token
// the dial carried, which a server may quote back in any text it chooses: an
// error message, a tool's description, a tool result. Unlike the endpoint, it is
// nowhere else at rest in the clear, and unlike the endpoint it is what the next
// dial authenticates with.
//
// A substring replacement, and that is its limit: a server that percent-encodes,
// base64s or splits the token before quoting it leaves something no rule over
// text recognises. What it does close is the ordinary case, which is a server
// echoing the header it received.
func scrubSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// endpointRenderings lists the ways the declared endpoint can appear in a
// failure message, each paired with what it should be replaced by.
//
// Four writers put it there, and they do not agree on a spelling. This package
// uses url.URL.Redacted, whose password is `xxxxx`; net/http writes the same
// URL with `***` instead; the declared bytes reach a message unaltered wherever
// something echoes the configuration rather than the parsed URL; and url.Error
// renders through %q, which escapes an embedded quote or backslash and so
// matches none of the first three. Enumerating them is possible only because
// the endpoint is known here — which is the whole reason this pass exists.
//
// Those four spell one URL, and the URL itself has a second spelling. An empty
// port — `https://host:/mcp`, which url.Parse accepts and keeps — is deleted by
// http.NewRequest before any request is sent (Go issue 14836), so a transport
// error names a host the agent never declared and every rendering built from
// the declared bytes misses it by one character. The normalized URL is
// therefore enumerated alongside the declared one, each with its own four
// forms. That is the whole of the divergence: net/http rewrites nothing else
// about a URL on the way to an error, and this client follows no redirect, so
// there is no third spelling to chase.
func endpointRenderings(endpoint string) []struct{ text, safe string } {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil
	}
	variants := []*url.URL{u}
	if normalized := strings.TrimSuffix(u.Host, ":"); normalized != u.Host {
		withPortRemoved := *u
		withPortRemoved.Host = normalized
		variants = append(variants, &withPortRemoved)
	}

	var out []struct{ text, safe string }
	seen := map[string]bool{"": true}
	for i, v := range variants {
		safe := v.Scheme + "://" + v.Host
		seen[safe] = true
		var written []string
		if i == 0 {
			// The declared bytes belong to the URL as parsed: url.Parse
			// normalizes, so they may differ from everything it renders, and
			// whatever echoes the configuration rather than the parsed URL
			// writes them unaltered.
			written = append(written, endpoint)
		}
		written = append(written, v.String(), v.Redacted())
		if _, hasPassword := v.User.Password(); hasPassword {
			written = append(written, strings.Replace(v.String(), v.User.String()+"@", v.User.Username()+":***@", 1))
		}
		// Each of those again as %q renders it, without the surrounding quotes:
		// those belong to the sentence around the URL, not to the URL.
		for _, form := range written[:len(written):len(written)] {
			if quoted := strconv.Quote(form); len(quoted) > 2 {
				written = append(written, quoted[1:len(quoted)-1])
			}
		}
		for _, form := range written {
			if seen[form] {
				continue
			}
			seen[form] = true
			out = append(out, struct{ text, safe string }{form, safe})
		}
	}
	return out
}

// redactURL reduces one matched URL to its scheme and host, keeping whatever
// sentence punctuation the match swept up so the surrounding text still reads.
func redactURL(match string) string {
	var trailing string
	for len(match) > 0 && strings.ContainsRune(`.,:;!?)]}"'`, rune(match[len(match)-1])) {
		trailing, match = match[len(match)-1:]+trailing, match[:len(match)-1]
	}
	// The trim is greedy because sentence punctuation is, and two of the
	// characters it eats also end a URL: `]` closes an IPv6 literal, and `:`
	// precedes a port. `https://[fd00::1]:` trims down to `https://[fd00::1`,
	// which no longer parses — so what was trimmed is handed back one character
	// at a time until it does. Giving back rather than never trimming is what
	// keeps the ordinary case working: `https://host/x).` still parses with the
	// punctuation attached, so a parse-first rule would never trim at all.
	u, err := url.Parse(match)
	for (err != nil || u.Host == "") && trailing != "" {
		match, trailing = match+trailing[:1], trailing[1:]
		u, err = url.Parse(match)
	}
	if err != nil || u.Host == "" {
		return "[redacted url]" + trailing
	}
	return u.Scheme + "://" + u.Host + trailing
}

// mcpEndpointHost validates a declared endpoint and returns the host the egress
// check judges. Both halves of this driver ask it — discovery before it lists,
// execution before it calls — so what counts as a usable MCP endpoint has one
// definition: a scheme this client speaks and a host to dial. Its error is
// already a reason a catalog row or a model can be shown.
func mcpEndpointHost(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the server's url is not an http or https URL")
	}
	return u.Hostname(), nil
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
// What decides is the policy's type, not which kind of environment carries it
// — and type rather than presence, because a value struct cannot tell an
// absent networking block from one that names no type: both decode to an
// empty type, and both read here as no policy. A self_hosted environment has no networking block by
// construction — its config normalizes to exactly {"type":"self_hosted"}, the
// REST surface rejecting every other field, and the reference documents
// networking on cloud environments only — so with no block there is nothing to
// apply and the dial goes through. A cloud environment always has one through
// the API. Either way a block that *is* present is a policy somebody meant, and
// one naming no recognized type is malformed rather than permissive: it admits
// nothing, which is `gate.newPolicy`'s shape and the safe direction.
//
// Reading a missing discriminator as "unrestricted" would make the malformed
// row the one that reaches everything: the schema constrains `config->>'type'`
// against the kind and nothing constrains `config->'networking'`, and a
// config-preserving update (packages, description) carries a stored value
// forward without revalidating it. Deciding on the kind first would do the same
// thing one arm at a time — a `self_hosted` row is exactly the one the API
// cannot have written a block onto, so a block found there came from an import,
// a restore or a hand-written UPDATE, and skipping it because of the kind
// would leave the refusal reachable on cloud rows alone.
//
// allow_mcp_servers "allows access to MCP server endpoints configured on the
// agent" on top of allowed_hosts, and this is only ever asked about a server the
// agent declared — discoverServers walks that array and nothing else — so under
// the flag the host is admitted without a second list to check it against.
func mcpEgressAllowed(cfg domain.EnvironmentConfig, host string) bool {
	if cfg.Type == domain.EnvSelfHosted && cfg.Networking.Type == "" {
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
//
// The same read re-checks that the session is still live, for the same reason
// and over a much wider window: sessionForRun's check is minutes old by the time
// a pass ends, and a session archived in between must not have rows written for
// it or a turn chained on its behalf. Every other executor settlement gets this
// from events.AppendInTx, which refuses an archived session under this very
// lock; discovery appends no event, so it asks directly. The item is completed
// either way — the work is done and there is nothing to retry.
func (e *Executor) settleMCP(ctx context.Context, item *queue.Item, rows []catalogRow) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var agentJSON []byte
	var status string
	var archivedAt *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT resolved_agent, status, archived_at FROM sessions WHERE id = $1 FOR UPDATE`,
		item.SessionID.String()).Scan(&agentJSON, &status, &archivedAt); err != nil {
		return fmt.Errorf("read session agent: %w", err)
	}
	if status != string(domain.SessionRunning) || archivedAt != nil {
		if err := e.queue.Complete(ctx, tx, item); err != nil {
			return err
		}
		return tx.Commit(ctx)
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
	// What each server's row said before this pass, so a failure is announced
	// when it starts rather than on every turn that re-attempts it. Read here,
	// under the session row lock this transaction already holds, so no other
	// pass for this session can be between the read and the upsert.
	failing, err := failingServers(ctx, tx, item.SessionID)
	if err != nil {
		return err
	}

	var failures []events.NewEvent
	for _, r := range rows {
		if declared[r.name] != r.url {
			continue
		}
		// A server the platform could not reach is said out loud, once. Not the
		// rows this pass simply did not finish with: those are its own
		// scheduling, and there is no connection behind them for an operator to
		// go and heal. Not a repeat, either — the pass runs every turn, so a
		// server that has been down for an hour would otherwise fill the log
		// with the same line; the row carries the current reason for anyone who
		// looks. A row that healed is never re-dialled (undiscoveredServers
		// skips a `ready` one), so the only transition this can miss is a
		// failure that changes kind mid-run, which the row still records.
		if r.status == "failed" && !r.notReached && !failing[r.name] {
			ev, err := mcpFailureEvent(r.name, mcpFailure{
				message: r.reason, authentication: r.authentication})
			if err != nil {
				return err
			}
			failures = append(failures, ev)
		}
		toolsJSON, err := json.Marshal(r.tools)
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
			     error = CASE WHEN $7::boolean
			                  THEN COALESCE(mcp_catalogs.error, EXCLUDED.error)
			                  ELSE EXCLUDED.error END,
			     fetched_at = EXCLUDED.fetched_at`,
			item.SessionID.String(), r.name, r.url, string(toolsJSON), r.status, reason,
			r.notReached); err != nil {
			return fmt.Errorf("write mcp catalog for %q: %w", r.name, err)
		}
	}
	if len(failures) > 0 {
		// In this transaction, so a server's row and the line about it commit
		// together or not at all, and the stream notify fires on the same commit.
		if _, err := e.log.AppendInTx(ctx, tx, item.SessionID, failures,
			events.AppendOptions{}); err != nil {
			return fmt.Errorf("append mcp discovery failures: %w", err)
		}
	}
	// An MCP call outstanding takes this item back rather than completing it,
	// the same first arm the four other settlements have and for the same
	// reason: only this driver answers an agent.mcp_tool_use. A call committed
	// while these dials were in flight — the pass runs for minutes — cannot have
	// been queued behind them, since Enqueue is keyed (session_id, kind) over
	// the live states and this very item is one; so completing here would leave
	// the call with nothing scheduled to answer it, the session running, and
	// archive and delete both refused. Handing the item back is what makes the
	// next pass find the call and answer it (processMCP answers before it
	// discovers).
	mcpPending, err := events.HasUnansweredMCPToolUse(ctx, tx, item.SessionID, nil)
	if err != nil {
		return err
	}
	if mcpPending {
		if err := e.queue.Requeue(ctx, tx, item); err != nil {
			return err
		}
		return tx.Commit(ctx)
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

// failingServers is the set of a session's MCP servers whose catalog row already
// records a failure, which is what keeps a discovery failure from being
// announced again on every turn that re-attempts it.
func failingServers(ctx context.Context, tx pgx.Tx, sid domain.ID) (map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT server_name FROM mcp_catalogs WHERE session_id = $1 AND status = 'failed'`,
		sid.String())
	if err != nil {
		return nil, fmt.Errorf("read mcp catalog statuses: %w", err)
	}
	defer rows.Close()
	failing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read mcp catalog statuses: %w", err)
		}
		failing[name] = true
	}
	return failing, rows.Err()
}
