// Package mcp is the platform's MCP client: a thin wrapper over the official
// github.com/modelcontextprotocol/go-sdk exposing what the platform needs and
// nothing else.
//
// Connections are per-work-item. A caller connects, does its work, and closes,
// so a crashed executor loses no state a fresh one cannot rebuild. What is
// per-work-item is the MCP session, not the socket: [DefaultClient] is
// package-level and keeps an ordinary net/http connection pool, so two work
// items reaching the same origin may well share a TCP connection, or multiplex
// over one HTTP/2 connection. That is deliberate — the state worth not sharing
// is protocol state, and there is none to share. The 2026-07-28 revision of
// MCP is what makes that affordable: it removed protocol-level sessions, so
// nothing accumulates on the server that a new connection has to re-establish,
// and there is no affinity between discovering a server's tools and later
// calling one. Not free, though — a connection still negotiates, and what that
// costs depends on the server it reaches: against a 2026-07-28 one a single
// `server/discover` round trip (two, where the SDK retries it after an
// unsupported-version answer), and against an older one that discover plus the
// legacy `initialize` and `notifications/initialized`, three requests, before
// Connect returns. What per-work-item connections cost is that handshake; what
// they buy is a client with no state to lose.
//
// The SDK is a dependency of this package alone. Its types do not appear in the
// wrapper's surface — the platform's domain model is Anthropic-native
// (CLAUDE.md design principle 1), and an MCP tool reaches the rest of the
// system as the platform's own [Tool], not as an SDK struct.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DialTimeout bounds a single connection attempt, and is the fallback deadline
// for any request that reaches the transport without one. MCP servers are
// third-party endpoints reached from a work item that holds a queue lease, so an
// unbounded wait would hold the lease rather than fail the item.
//
// The fallback catches the request that reaches the transport with no deadline
// at all — see withResponseLimit for why the SDK makes one of those, and why the
// caller's http.Client.Timeout cannot be relied on to bound it. A request that
// already carries a deadline keeps it; the fallback never shortens one.
//
// That does not make DialTimeout a floor overall, and it would be easy to read
// it that way: DefaultClient also sets it as its own Timeout, which is a
// whole-request cap that beats a longer context deadline. So every request
// through DefaultClient is bounded at 30s, and ListTimeout's two minutes bound
// the listing across its pages rather than any single one of them. Which client
// carries a connection therefore decides this too: on [CallClient] the same cap
// is [CallTimeout], so *every* request that connection makes gets a tool's
// budget rather than a dial's — the handshake, and the session-ending DELETE the
// SDK sends on a context nothing here can cancel, included. That is the price of
// running tools through one client, and it is bounded rather than open: the
// driver's own pass budget is what keeps a server that accepts and never answers
// from holding a work item indefinitely. A caller supplying its own client sets
// that policy itself, and may set none — which is the case the fallback exists
// for.
//
// None of the deadlines here is exact, and the overshoot is the SDK's: when a
// caller's context ends a request that is still outstanding, the SDK tells the
// server so with a `notifications/cancelled`, and it sends that on a context
// deliberately detached from the one that just ended (context.WithoutCancel)
// with a 5-second timeout of its own. Against a server that has stopped
// answering, that notification is itself unanswerable, so every cancelled call
// costs up to five seconds after its deadline — twice over for a connection,
// which may have both a `server/discover` and a legacy `initialize` in flight.
// Callers that bound a whole pass should treat these as budgets that stop the
// work rather than ceilings on the wall clock.
const DialTimeout = 30 * time.Second

// ListTimeout bounds a whole listing rather than one request in it. Pagination
// is server-driven, so without an aggregate budget a server that answers each
// page just inside DialTimeout holds the work item — and the queue lease behind
// it — for maxToolPages requests in a row.
const ListTimeout = 2 * time.Minute

// clientName identifies this platform to MCP servers. Under a 2026-07-28
// negotiation the spec asks a client to put it on every request's `_meta` — a
// SHOULD, unlike the required protocolVersion and clientCapabilities — and
// go-sdk does; under an older one it rides `initialize`'s clientInfo instead.
// Either way a server operator can see which client is calling.
const clientName = "managed-agent-platform"

// Config describes one MCP server connection.
type Config struct {
	// URL is the server's MCP endpoint, from the agent's mcp_servers entry.
	URL string
	// BearerToken, when set, is sent as `Authorization: Bearer`. It comes from
	// a session vault's matching credential; an empty token connects
	// anonymously, which is the reference's documented no-match behavior.
	BearerToken string
	// HTTPClient replaces the guarded client rather than adding to it. Nil
	// selects [DefaultClient], which is what production uses; a test supplies
	// its own to reach an httptest server on loopback. A supplied client gives
	// up everything [DefaultClient] carries — the dial-time address guard, the
	// refusal to follow redirects, and the per-block cap on raw response header
	// bytes (maxHeaderBytesPerResponse, in place of net/http's 10 MiB default) —
	// so a non-test caller that wants its own transport policy installs those
	// itself.
	//
	// The header cap belongs on that list rather than under the bounds below,
	// and the distinction is easy to get backwards: the two bounds this package
	// owns — the cumulative response budget and the fallback request deadline —
	// are wrapped around whatever client is supplied and are not optional, but
	// the response budget can only charge header bytes that survive parsing.
	// The bytes it cannot see are held by the transport setting, which lives on
	// the client, so a supplied client is bounded on bodies and unbounded on
	// padded, informational and trailing header blocks.
	HTTPClient *http.Client
}

// Tool is one tool an MCP server reports, in the shape the platform stores and
// later hands the model. The JSON tags are the Anthropic tool-definition field
// names rather than MCP's, so a catalog row needs no second translation at
// request-assembly time; the name is the bare tool name as the server reports
// it, which is also what a `configs[]` entry addresses.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Conn is one open connection to an MCP server. It is not safe for concurrent
// use and is not meant to be: one work item, one connection, one goroutine.
type Conn struct {
	session *sdk.ClientSession
	// auth records whether this connection was ever answered 401 or 403, so a
	// failure raised on it can be told from one that never got that far. Nil on
	// a Conn built without the transport chain, which marks nothing.
	auth *authWatch
}

// DefaultClient is the guarded HTTP client used when a Config supplies none.
//
// Two protections, both of which the platform needs because the URL is
// customer-supplied. The dial-time address guard (internal/dialguard) refuses
// loopback, link-local, the unspecified address and multicast on the resolved
// IP of every dial, so neither a hostile MCP server URL nor a DNS rebind
// reaches the platform's own surfaces or a cloud metadata endpoint. And
// redirects are never followed: following one would replay the request — with
// its Authorization header — to a target the per-hop guard vets but never
// approved as a destination.
//
// The transport is spelled out rather than cloned from http.DefaultTransport,
// so three of its settings are decisions rather than omissions.
//
// No Proxy. http.DefaultTransport reads HTTP_PROXY/HTTPS_PROXY from the
// environment; this deliberately does not, because a proxy moves the dial off
// the target and onto the proxy, and the address guard would then be vetting the
// proxy's address while the proxy fetched whatever the URL named. That is the
// guard removed rather than satisfied. A deployment whose egress genuinely
// requires a proxy supplies its own client and owns the consequence.
//
// ForceAttemptHTTP2, because setting DialContext at all turns HTTP/2 off unless
// it is set — an MCP server reached over https would otherwise be spoken to in
// HTTP/1.1 only, which is a downgrade nobody chose.
//
// Idle-connection settings, because this Transport is package-level: it outlives
// every connection made through it, and with the zero values an idle connection
// to a server never reached again is held until the process ends.
//
// And MaxResponseHeaderBytes, which is the *only* bound on a response's header
// bytes and not a second line behind the cumulative budget below. That budget
// charges what it can reconstruct from resp.Header, and net/http normalizes the
// block before handing it over: a value's padding whitespace is trimmed and a
// folded continuation is joined, so the bytes are read, allocated, and then
// unaccountable. Measured against a raw listener answering Content-Length and an
// X-Pad value of one character followed by 200,000 spaces: 200,048 header bytes
// on the wire, reconstructed to 49 — a factor of 4,083, which is a distortion no
// arithmetic on the parsed map can correct. So
// the raw block is bounded here instead — 64 KiB rather than net/http's 10 MiB
// default, per header block, which is a bound on the peak one block reaches and
// deliberately not a total: how many blocks a connection answers is not this
// package's to know, and MaxResponseBytes says why.
var DefaultClient = guardedClient(DialTimeout)

// CallClient is DefaultClient's twin for running tools, and it exists because
// http.Client.Timeout bounds the whole request including the body read: a
// `tools/call` response is not complete until the tool is, so a client whose cap
// is the dial budget cannot run a tool that takes longer than a dial. That is
// the right cap for a handshake and a listing, where the server is answering
// from what it already knows, and the wrong one for a tool that goes and does
// something — a query, a build, a fetch of its own.
//
// The cap here is [CallTimeout], which is what a tool call gets on this platform
// already: a remote tool should not outlive a local one by default. Everything
// else is DefaultClient's, the address guard included — the URL is
// customer-supplied on this path exactly as it is on that one.
var CallClient = guardedClient(CallTimeout)

// guardedClient builds one of the two, differing only in how long a single
// request may take. Everything a customer-supplied URL makes necessary is here
// rather than at the call sites, so neither client can be built without it.
func guardedClient(requestTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: DialTimeout,
				Control: dialguard.Control(dialguard.IPAllowed),
			}).DialContext,
			ForceAttemptHTTP2:      true,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  time.Second,
			MaxResponseHeaderBytes: maxHeaderBytesPerResponse,
		},
	}
}

// maxHeaderBytesPerResponse bounds a raw header block before net/http parses
// it. See DefaultClient for why this is the only bound those bytes get.
//
// A *block*, not a response, and the difference is not pedantry. Over HTTP/1.1
// the setting very nearly does bound a response: net/http resets the read limit
// per 1xx block only when a request carries an httptrace with Got1xxResponse set
// (transport.go, persistConn.readResponse), the SDK installs no httptrace, so
// informational blocks and the final block draw on one limit and trailers are
// bounded separately by the read buffer. Over HTTP/2 — which this client
// explicitly enables, so this is not a hypothetical path — the limit becomes the
// connection's maxHeaderListSize and applies to each decoded block on its own.
// Measured with a one-off probe rather than a retained fixture, though each
// figure follows from the constants: a single 80 KiB final block is
// refused, while two 30 KiB Early Hints plus a 60 KiB final block (~120 KiB) is
// accepted, and with a 60 KiB trailer on top (~180 KiB) it is still accepted.
// What the suite does retain is the boundary itself, driven either side of it.
// One response cycle therefore carries up to three separately-capped blocks — the
// informational blocks in aggregate, the final block, and the trailers — of which
// only the final block is ever charged, so what this constant bounds is the peak
// any one block reaches and not what a connection delivers in total.
//
// Three is a ceiling on that cycle rather than a sample, because each of the
// three is capped in aggregate rather than per frame: 1xx blocks accumulate into
// one running total that is never reset, and CONTINUATION frames are bounded
// inside the block they continue. The 1xx total covers the informational phase
// alone — net/http says so where it keeps it, "This differs a bit from the
// HTTP/1 implementation, which limits the size of all 1xx headers plus the final
// response" — which is why the final block and the trailers are positions of
// their own rather than more of the same one.
//
// 64 KiB is generous rather than tight, deliberately: it is a hard ceiling on
// what a *legitimate* server may send, and the cost of setting it too low is a
// working server this client cannot talk to. Real response header blocks run a
// few hundred bytes to a few kilobytes; the heavy realistic cases — SSO
// Set-Cookie chains, dense tracing and rate-limit headers — reach the low tens
// of kilobytes. The referent worth naming is nginx's proxy_buffer_size, the cap
// it puts on an *upstream's* response header block, which defaults to one memory
// page — 4k or 8k depending on the platform, so this is eight to sixteen times
// it — and any MCP server behind a default reverse proxy is already held below
// this limit by the proxy rather than by us. (Comparing
// it to what nginx *emits* of its own accord, a few hundred bytes, would make
// this ~200x and is the wrong comparison; so is anything about cookie jars,
// which govern outbound request headers and cannot affect the block a server
// sends.)
const maxHeaderBytesPerResponse = 64 << 10

// http2HeaderListOverhead is the slack net/http adds when it turns
// MaxResponseHeaderBytes into an HTTP/2 SETTINGS_MAX_HEADER_LIST_SIZE, and it
// is here because the arithmetic is wrong without it.
//
// h2's limit counts len(name)+len(value)+32 per field rather than raw bytes, so
// net/http pads the HTTP/1 figure by an assumed ten fields:
// http2adjustHTTP1MaxHeaderSize adds typicalHeaders(10) * perFieldOverhead(32).
// The framer therefore admits maxHeaderBytesPerResponse+320 per block, and a
// single field can carry nearly all of it. Bisected against a real HTTP/2
// fixture rather than reasoned about: the largest trailer value accepted is
// 65,856 bytes and the first refused is 65,857 — landing exactly on the sum,
// which is what makes this constant a description of net/http rather than of
// itself. The fixtures either side of the cap sit 128 bytes below it and 64
// above for the same reason; rounder margins would pass under any overhead
// value, so they would assert nothing about net/http.
const http2HeaderListOverhead = 320

// Connect opens a connection to one MCP server over Streamable HTTP.
//
// Only Streamable HTTP is spoken. The SDK negotiates every protocol version
// from 2024-11-05 up over it, so an older server is reached as long as it hosts
// the modern endpoint; the separate HTTP+SSE transport of 2024-11-05 is
// deprecated ("New implementations SHOULD NOT adopt it" — spec 2026-07-28,
// transports/streamable-http) and a client-side fallback to it is deliberately
// not wired here.
//
// The standalone SSE stream is disabled. It is how a server pushed unsolicited
// notifications in revisions 2025-03-26 through 2025-11-25; 2026-07-28 removed
// the GET endpoint it used, and a per-work-item connection has no use for
// server-initiated messages in either era — it asks one question and closes.
//
// The setting therefore does nothing against a modern server: go-sdk v1.7.0
// returns before opening the GET whenever the negotiated version is 2026-07-28
// or later (streamable.go, sessionUpdated), so it only takes effect on an older
// negotiation, where the spec makes the GET optional and a tools/list answer
// comes back on the POST regardless.
//
// That older negotiation is what the contract suite actually runs against, which
// is easy to get backwards. go-sdk serves 2026-07-28 only from a stateless
// handler, so a default sdk.NewStreamableHTTPHandler does not answer
// server/discover at all and every fixture here falls back to the legacy
// initialize and negotiates 2025-11-25 — precisely the era where this line is
// load-bearing. It is asserted rather than assumed:
// TestConnectOpensNoStandaloneStream counts the GETs.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server URL is required")
	}
	endpoint, err := url.Parse(cfg.URL)
	if err != nil {
		// url.Error renders the URL it could not parse through %q, so wrapping
		// it whole would put the configured string — userinfo and query
		// included — into the message. Nothing here can redact that string:
		// redaction needs a parsed URL, and this is the branch where there is
		// none. So the cause travels and the URL does not, which costs the
		// reader the offset the message would have pointed at and keeps a
		// credential out of every log line that renders this error.
		var parseErr *url.Error
		if errors.As(err, &parseErr) {
			return nil, fmt.Errorf("mcp: the server url could not be parsed: %w", parseErr.Err)
		}
		return nil, fmt.Errorf("mcp: the server url could not be parsed")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = DefaultClient
	}
	// Innermost, so it reads a status straight off the wire. The response limit
	// sits above it and answers (nil, error) for a response whose headers alone
	// exceed what the budget has left, discarding that response — a 401 read
	// anywhere above the limit would be a 401 the limit can swallow.
	httpClient, auth := withAuthWatch(httpClient)
	httpClient = withResponseLimit(httpClient)
	httpClient = withTraceContext(httpClient)
	if cfg.BearerToken != "" {
		httpClient = withBearer(httpClient, cfg.BearerToken, endpoint)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientName}, nil)
	// The SDK closes the session it built on most failure paths but not on all
	// of them — an initialize answered with a protocol version it does not
	// support returns before Close, where the two adjacent branches call it —
	// and Connect hands back no session for the caller to close. Reported
	// upstream as modelcontextprotocol/go-sdk#1154; this stays until a release
	// carries the fix, since it costs one wrapper and removing it early would
	// leak on every attempt against a server that answers that way. Capturing the
	// transport's connection is the only handle left, and it keeps a failed
	// Connect from leaving the reader goroutine and its HTTP connection alive on
	// a server that answers that way to every attempt.
	//
	// The wrapper hands back the SDK's own Connection unchanged. Substituting
	// one is not forbidden — the SDK ships LoggingTransport, which does exactly
	// that, and a substituted connection still negotiates — but the SDK reaches
	// sessionUpdated through a type assertion to an unexported interface, so a
	// substitute silently loses it, and with it the Mcp-Protocol-Version header
	// on every post-handshake request of a legacy negotiation. Since a legacy
	// negotiation is what this client mostly gets, the wrapper stays transparent.
	transport := &capturingTransport{inner: &sdk.StreamableClientTransport{
		Endpoint:             cfg.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}}
	session, connErr := client.Connect(ctx, transport, nil)
	if connErr != nil {
		if conn := transport.conn; conn != nil {
			_ = conn.Close()
		}
		// Scheme and host, not the URL: an mcp_servers URL may carry a
		// credential in three places and this error is a stored column by the
		// time the executor's discovery pass is done with it. url.URL.Redacted
		// is not enough and reads as though it were — it masks the password
		// alone, leaving a token-as-username (`https://ghp_…@host`, a common MCP
		// convention) and a `?api_key=` query in full. What survives here is
		// what an operator needs to know which server failed; net/http's own
		// half of the message redacts its password and keeps the rest, which is
		// why the executor redacts this string again by value before storing it.
		return nil, auth.mark(
			fmt.Errorf("mcp: connect to %s://%s: %w", endpoint.Scheme, endpoint.Host, connErr))
	}
	return &Conn{session: session, auth: auth}, nil
}

// capturingTransport keeps the Connection its inner transport produced so a
// failed Client.Connect can still close it. Connect is called exactly once by
// the SDK, so a single field needs no synchronisation.
type capturingTransport struct {
	inner sdk.Transport
	conn  sdk.Connection
}

func (t *capturingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	t.conn = conn
	return conn, err
}

// maxToolPages bounds how many pages one listing will fetch. Pagination is
// driven entirely by the server — it decides when to stop by omitting the next
// cursor — so an unbounded loop here is a server-controlled loop, and it runs
// inside a work item holding a queue lease. It is a compatibility limit as well
// as a safety valve, and calling it only the latter would understate it: a
// spec-compliant server that genuinely paginates past 100 pages fails the
// listing outright rather than being truncated. That is the right failure — a
// silently short catalog is worse than a loud one — but it is a real ceiling,
// reached by page size rather than by catalog size, since a server choosing a
// page size of 10 hits it at 1,000 tools while one choosing 1,000 (the SDK's
// own default) does not hit it until 100,000.
const maxToolPages = 100

// ListTools returns every tool the server reports, following pagination to the
// end. A server that reports none is not an error, and neither is an entry this
// refuses. The nearest documented case is adjacent rather than identical:
// Anthropic's *Messages API* MCP connector states that a `configs` entry naming
// a tool the server does not offer logs a backend warning and returns no error,
// because MCP servers may have dynamic tool availability. The managed-agents
// pages say nothing about either an empty listing or a malformed entry, so
// treating both as facts to record rather than failures is consistent with that
// posture by analogy — it is not the reference stating it.
//
// Pagination is driven here rather than through the SDK's Tools iterator
// because the iterator hides the cursor, and the cursor is the only thing that
// says whether a server is paginating or looping: the iterator follows
// `nextCursor` until a server omits it, which a server that keeps returning the
// same cursor never does. Everything a server sends is treated as hostile until
// checked — this endpoint is customer-supplied, and the SDK decodes rather than
// validates it.
func (c *Conn) ListTools(ctx context.Context) ([]Tool, error) {
	return c.listTools(ctx, ListTimeout)
}

// listTools takes the whole-listing budget as a parameter rather than reading
// ListTimeout directly, so a test can drive one short enough to observe.
//
// That is the entire reason for the split, and it is worth stating: with the two
// minutes inlined here, replacing the deadline with a plain cancel left the
// whole suite green. A bound no suite can wait out is a bound that rests on
// review, and this package has already shipped two guards no test could fail on.
func (c *Conn) listTools(ctx context.Context, budget time.Duration) ([]Tool, error) {
	if c == nil || c.session == nil {
		// Before the recover below, so this package's own misuse is not
		// reported as a server that crashed the client library.
		return nil, fmt.Errorf("mcp: list tools on a connection that was never opened")
	}
	// One budget for the whole listing, not one per request. A server that
	// answers every page just inside DialTimeout would otherwise hold a work
	// item — and its queue lease — for maxToolPages × DialTimeout, which is
	// most of an hour.
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	c.auth.reset()

	out := []Tool{}
	// Every cursor the server has already handed out, and every name already
	// accepted. Both exist because a server is free to repeat itself and this
	// package is the only thing that notices.
	seenCursor, seenName := map[string]bool{}, map[string]bool{}
	var cursor string
	for page := 0; page < maxToolPages; page++ {
		res, err := c.listPage(ctx, cursor)
		if err != nil {
			return nil, c.auth.mark(fmt.Errorf("mcp: list tools: %w", err))
		}
		for _, tool := range res.Tools {
			// A nil element is what `"tools": [null]` decodes to. With go-sdk
			// v1.7.0 the page never gets this far — the SDK dereferences the
			// element first and panics, which listPage turns into an error —
			// so this is the guard for the release that fixes that panic and
			// starts handing nils through. It is deliberately kept: the cost
			// is one condition, and the alternative is a nil dereference on
			// data a customer-named server chose.
			if tool == nil || !usableName(tool.Name) {
				continue
			}
			// A name is how a configs[] entry and a model's tool_use address a
			// tool, so a second entry claiming one already taken is not a second
			// tool — it is an ambiguity, and one that would reach the model as
			// two tool definitions sharing a name in a single request. First
			// wins: the order is the server's own, so the first entry is the
			// one it led with. What matters for the ordering is where the name
			// is *recorded* rather than where it is checked — the recording
			// happens below, after the schema check, so a malformed first entry
			// never consumes the name a valid later one could use.
			if seenName[tool.Name] {
				continue
			}
			schema, ok := inputSchema(tool.InputSchema)
			if !ok {
				continue
			}
			seenName[tool.Name] = true
			out = append(out, Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		// Every cursor, not just the one immediately before. A server that
		// alternates between two cursors never repeats one in consecutive
		// answers, so a check against `cursor` alone walks the full hundred
		// pages, appending each page's tools again on every lap. A server can
		// also make those laps free: under a 2026-07-28 negotiation the SDK
		// serves an already-requested cursor out of its own per-cursor cache
		// (mcp/client.go ListTools -> cachedListResult) whenever it sent a
		// positive `ttlMs` with the page, and a page that never reaches the wire
		// never draws on the cumulative byte budget either. Catching the repeat
		// on its second sighting rather than the hundredth also keeps a wedged
		// server from holding the queue lease for the round trips in between.
		if seenCursor[res.NextCursor] {
			return nil, fmt.Errorf("mcp: list tools: server repeated a pagination cursor")
		}
		seenCursor[res.NextCursor] = true
		cursor = res.NextCursor
	}
	return nil, fmt.Errorf("mcp: list tools: server did not stop paginating within %d pages", maxToolPages)
}

// listPage fetches one page of the listing, converting a panic inside the SDK
// into an error.
//
// Recovering around a library call is not something to do lightly, and this is
// the case that earns it: go-sdk v1.7.0 dereferences every element of the
// decoded `tools` array without a nil check (filterValidTools →
// validateParamHeaderAnnotations, mcp/streamable_headers.go), so a server
// answering tools/list with `"tools": [null]` panics the client. The endpoint
// is customer-supplied and the caller is an executor shared by every session
// on the host, where a Go panic is not confined to the goroutine that raised
// it — so the choice is between one failed work item and a process that takes
// every concurrent tool call down with it. The recover is scoped to the single
// SDK call so it cannot mask a panic in this package's own code.
func (c *Conn) listPage(ctx context.Context, cursor string) (res *sdk.ListToolsResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("the MCP client library panicked on this server's response: %v", r)
		}
	}()
	return c.session.ListTools(ctx, &sdk.ListToolsParams{Meta: requestMeta(ctx), Cursor: cursor})
}

// requestMeta is the `_meta` this platform puts on the MCP requests it issues
// itself — the listing and the call.
//
// It carries W3C trace context, so a trace that starts in the brain reaches the
// MCP server's own spans rather than stopping at this process's edge (CLAUDE.md
// design principle 3). Not every request on the wire, and the difference is the
// SDK's: it builds the handshake itself (`server/discover`, and a legacy
// negotiation's `initialize` and `notifications/initialized`) with no hook for a
// caller's metadata, so no `_meta` reaches them. What reaches them instead is
// the HTTP header the transport writes ([withTraceContext]) — the transport
// being this package's, unlike the handshake bodies — so a traced connection is
// traced from its first packet after all, by the other of the two routes. MCP 2026-07-28 documents the convention and pins the key
// names to the bare `traceparent`, `tracestate` and `baggage` — deliberately not
// namespaced like the protocol's own `io.modelcontextprotocol/*` keys, so that a
// carrier written by any OpenTelemetry SDK drops in unrenamed (SEP-414).
// internal/telemetry propagates trace context alone, so `baggage` never appears
// and the carrier's keys are already the spec's; nothing here translates
// anything, which is the point of the convention.
//
// The SDK fills the protocol's own `_meta` keys itself and only where they are
// absent (injectRequestMeta), so a map supplied here is added to rather than
// replaced. Nil when no span is active, which says "nothing to add" rather than
// changing what goes out: `_meta` is `omitempty`, so an empty map would be
// omitted from the request just the same.
func requestMeta(ctx context.Context) sdk.Meta {
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	meta := make(sdk.Meta, len(carrier))
	for k, v := range carrier {
		meta[k] = v
	}
	return meta
}

// usableName reports whether a server's tool name can be offered to a model.
//
// The rule is the MCP SDK's own — non-empty, at most 128 bytes, and the runes
// [a-zA-Z0-9_.-] (validateToolName in mcp/tool.go; the 128 is a len(), so bytes,
// whatever its error message calls them). Note what the SDK does *not* do with
// it: AddTool logs a violation and registers the tool anyway, and nothing checks
// it on the client side at all, so a name that breaks the rule reaches a client
// as a perfectly ordinary listing entry. That makes this the first place it can
// be caught, not a redundant second one. Refusing one tool is better than a
// request that fails carrying every other tool with it.
//
// It is deliberately the SDK's rule and not a guessed-at Anthropic one, and it
// is a floor rather than the whole constraint. The reference states a charset
// only for a *custom* tool — "1-128 characters; letters, digits, underscores,
// and hyphens" (anthropic-sdk-go betaagent.go, BetaManagedAgentsCustomToolParams)
// — and states none at all for the field that names an MCP tool, which
// documents length alone. So this admits '.', which that custom-tool charset
// excludes. Where a reference-checked rule belongs is where the request is
// assembled and the constraint can be diffed against the reference, not here.
func usableName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// inputSchema renders one tool's declared input schema as the JSON an Anthropic
// tool definition carries, reporting false for a tool that cannot be offered to
// a model at all.
//
// The SDK types this field as `any` and, client-side, fills it with whatever
// the server's JSON decoded to. MCP requires an object whose `type` is
// "object"; a server is free to break that, and what breaks is refused rather
// than repaired. A schema that is absent, null, or not a JSON object has no
// honest translation, and one whose `type` is anything but "object" describes a
// tool this platform cannot call.
//
// The `type` is required, not merely checked when present. An earlier shape
// accepted a schema that simply omitted it, which let {} through as an
// "unconstrained" contract — the same fabrication as the absent-schema case
// below, arrived at from the other direction. Both MCP's schema and the pinned
// SDK's own server require the root type: AddTool panics unless the decoded
// schema's type is "object", and reads an absent type as not-"object" rather
// than as a default (mcp/server.go). Anthropic's input_schema requires it too,
// so a schema without it could not be offered to a model anyway.
//
// Substituting {"type":"object"} for an absent schema was the first shape of
// this and was wrong: it reads as "this tool takes no arguments", which is a
// contract the server never offered, so a tool that in fact requires arguments
// would be published to the model as one that takes none and called with none.
// Fabricating a contract to keep a broken tool is worse than dropping it — the
// SDK's own server refuses the same substitution for the same reason.
//
// Validation stops there. A full JSON Schema check is not this package's job
// and inventing one would be guessing at what the reference accepts; what is
// enforced here is only what MCP itself states.
//
// A refused tool is skipped rather than failing the listing: one malformed
// entry must not deny an agent the rest of a server's tools, which matches the
// reference's treatment of an unresolvable tool name as a warning. The entry
// and the reason are lost — this signature has nowhere to put a warning — which
// is the catalog slice's problem to give them somewhere.
//
// What comes back is the schema's meaning, not its bytes. The SDK hands over a
// value already decoded into `any`, so re-marshaling sorts the object's keys and
// renders every number through float64, which makes a large integer bound
// unreliable rather than uniformly lost: 2^53+1 comes back as 2^53 and 2^60 as
// 1152921504606847000, anything from 1e21 up comes back in exponent form, and
// some even values just above 2^53 survive untouched. Reaching the original
// bytes would mean not using the SDK's typed result at all, and a JSON Schema
// whose correctness depends on key order or on a 17-digit integer is not one
// this platform needs to carry.
func inputSchema(declared any) (json.RawMessage, bool) {
	if declared == nil {
		return nil, false
	}
	raw, err := json.Marshal(declared)
	if err != nil {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	var kind string
	if json.Unmarshal(obj["type"], &kind) != nil || kind != "object" {
		return nil, false
	}
	return raw, true
}

// Close ends the connection. It is safe to call on a Conn whose work failed.
//
// It does not guard the zero value, where ListTools does, and the asymmetry is
// the point rather than an oversight: ListTools sits in front of a recover that
// would catch a nil dereference and report it as a server crashing the client
// library, which is a lie about whose fault it is. Nothing catches a panic here,
// so calling Close on a Conn that was never opened panics with a stack pointing
// at the caller — already an accurate account of the misuse, and not worth a
// branch to restate.
func (c *Conn) Close() error { return c.session.Close() }

// MaxResponseBytes bounds what a connection accounts for across every response
// rather than one response at a time — bodies in full, header blocks only as far
// as they survive parsing to be counted, which is a distinction this comment
// draws out below rather than one to take on trust from this line.
//
// The bound is not optional politeness: go-sdk v1.7.0 reads a response with
// io.ReadAll before it decodes anything (mcp/streamable.go, handleJSON), so a
// server that answers a tools/list with a chunked, multi-gigabyte description
// grows the executor's heap until the process dies — and it takes every other
// session on that host with it. Neither the request timeout nor the page bound
// stops it, because both count requests rather than bytes, and no recover
// catches an out-of-memory. The only place to refuse is before the SDK sees the
// body.
//
// It is cumulative because a per-response cap does not actually bound a listing.
// maxToolPages responses of MaxResponseBytes each is 800 MiB, and both ends
// retain it. Under a 2026-07-28 negotiation the SDK puts every tools/list result
// into a per-cursor cache unconditionally (mcp/client.go ListTools →
// toolsCache.put, gated only on usesNewProtocol), and an entry stays there until
// that cursor is asked for again or the server sends
// notifications/tools/list_changed, which clears the cache outright
// (mcp/cache.go invalidate). So a hundred unique cursors hold a hundred pages
// live. This package retains its own copy alongside: names and descriptions are
// shared string backings rather than second copies, but every accepted schema
// exists twice, once as the SDK's decoded map and once as the bytes re-marshaled
// here. Cumulative makes the bound mean what it says.
//
// (Whether a *read* of that cache avoids the wire is a separate question with a
// different answer, and worth not conflating: mcp/cache.go get treats an entry
// as a miss and deletes it unless the server sent a positive, unexpired `ttlMs`
// hint, which nothing defaults — so a modern server that omits the field caches
// nothing usefully and every repeat goes back on the wire. Retention does not
// depend on that; serving does.)
//
// What it covers is bodies in full and header blocks only as far as they can be
// counted after parsing — see headerBytes. Header fields that never reach the
// parsed map are outside it, held per block by maxHeaderBytesPerResponse
// instead, and *per block* is where the arithmetic stops rather than continues.
//
// No cumulative bound on delivered header bytes is worth publishing. Several
// revisions of this comment published one anyway and every one was falsified: by
// whitespace padding, by mistaking a block for a response, by claiming a socket
// total that no header arithmetic can produce, and by multiplying a per-block cap
// by a response count. That last is the one that matters, because it is what all
// of them needed — a bound on how many responses one connection answers, which
// go-sdk v1.7.0 does not have. Counting the handshake, a hundred pages and the
// session-ending DELETE reaches 104, and two paths walk past it. A server that
// answers the first server/discover with CodeUnsupportedProtocolVersion and a
// supported-version list is probed a second time (mcp/client.go, `for range 2`).
// And any response delivered as text/event-stream may end carrying a fresh `id:`
// and no call response, which sends handleSSE round its reconnect loop again —
// the no-progress retry cap it grew for exactly this resets on every id that
// advances, the server picks the delay through the SSE `retry:` field, and the
// SDK's own TODO beside it records that a limit on total attempts for one logical
// request is still missing (mcp/streamable.go, handleSSE/connectSSE).
// TestAConnectionAnswersMoreResponsesThanItHasPages drives it: around two
// thousand responses to a single listing in three seconds, in three of the 120
// seconds ListTimeout allows, against the 104 that arithmetic multiplied by. It
// is written to go red if that upstream limit ever lands, which is how this
// comment would learn it may tighten again. It counts responses and says nothing
// about their bytes, deliberately — see the note there on the figure that came
// of multiplying that count by a per-response size instead of measuring one.
//
// One thing here is bounded and worth saying: headerBytes charges every response
// at least the twenty-odd bytes of its status line and terminator, so this budget
// caps the response count too, at a few hundred thousand. Its product with a
// maximal block is left unstated on purpose, under the rule this comment kept
// failing: do not multiply by a quantity nothing bounds. Deriving a figure is
// fine where both terms are enforced somewhere — 800 MiB above is maxToolPages
// times MaxResponseBytes, and the few hundred thousand just above is this budget
// over a floor headerBytes guarantees — and the withdrawn product was not that.
// It multiplied an enforced per-block cap by a response count with no bound at
// all, which is why every attempt to publish it has been wrong, including one
// that named the rule and broke it in the same sentence.
//
// What is bounded usefully is the peak and not the sum: one header block at a
// time, plus this cumulative figure for everything that can be accounted. The
// unaccounted blocks are parsed and dropped per response rather than retained,
// so they cost bandwidth rather than memory, and the loop that produces them
// ends on whichever of this budget or ListTimeout arrives first. Nothing here
// bounds socket traffic either, which a hostile server inflates at will — DATA
// frames may carry one byte each, and PING or WINDOW_UPDATE floods are outside
// every count in this package. ListTimeout is what bounds a hostile
// connection's cost in that direction. These bounds are about memory, which is
// what an io.ReadAll of a multi-gigabyte response actually threatens.
//
// 8 MiB is far above any real catalog — the whole point of a tool definition is
// to fit in a model request, and a catalog that cannot be sent to a model is not
// a catalog — and small enough that a host running many sessions is not one
// hostile server away from an out-of-memory.
const MaxResponseBytes = 8 << 20

// withResponseLimit returns a copy of client that reads at most
// MaxResponseBytes in total and gives any request the SDK detached from the
// caller's context a deadline of its own. It wraps whatever client the caller
// supplied, so neither bound depends on the caller having thought about it.
//
// The deadline exists because http.Client.Timeout is the caller's to set and a
// supplied client may leave it zero, while the SDK deliberately detaches some
// requests from every context we control: it builds a connection's lifecycle
// context as context.WithCancel(xcontext.Detach(ctx)) — no deadline, and cut off
// from the caller's cancellation, so the only thing that ends it is the
// connection's own Close, and only after the DELETE has returned — then sends
// the session-ending DELETE on it (mcp/streamable.go,
// streamableClientConn.Close). A server that accepts that DELETE and never
// answers would otherwise hang Close — and the work item's queue lease with it —
// with nothing left to interrupt it. A request that already carries a deadline
// keeps it, so this never shortens ListTimeout.
func withResponseLimit(client *http.Client) *http.Client {
	copied := *client
	copied.Transport = newLimitedTransport(client.Transport, MaxResponseBytes)
	return &copied
}

// newLimitedTransport is the one place a budget is funded. It exists as a
// function so the test seam draws on the same wiring: with the two spelled out
// separately, a mutant that funded the production budget with a spare byte —
// reintroducing exactly the off-by-one this package already shipped once — left
// the boundary tests green, because they were funding their own.
func newLimitedTransport(base http.RoundTripper, limit int64) *limitedTransport {
	return &limitedTransport{base: base, limit: limit, budget: limit}
}

// limitedTransport holds the connection's whole byte budget, so every response
// it wraps draws from one counter. The counter is mutex-guarded, which keeps it
// from being corrupted; what it does not do is make the *policy* concurrency-safe,
// and the difference is worth naming. A body draws before it reads and refunds
// what it did not use, so two bodies read at once could see a transient zero and
// latch themselves refused over a budget that was only borrowed. Nothing here
// produces that: one connection belongs to one work item and one goroutine (see
// [Conn]), the standalone SSE stream is disabled, and the SDK reads each response
// to completion before issuing the next. The reserve-then-refund order is kept
// because its failure direction under a concurrency this package does not have is
// an over-refusal rather than an over-delivery.
type limitedTransport struct {
	base  http.RoundTripper
	limit int64 // what the budget started as, for the error text

	mu     sync.Mutex
	budget int64
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := req.Context().Deadline(); !ok {
		ctx, cancel := context.WithTimeout(req.Context(), DialTimeout)
		req = req.Clone(ctx)
		// Cancelling on return would kill the body before the caller reads it,
		// so the deadline has to outlive RoundTrip. context.AfterFunc discharges
		// the lost-cancel rule instead — the CancelFunc is guaranteed to be
		// called — and costs nothing; what releases the timer is the deadline
		// firing or the parent being cancelled, either way no later than
		// DialTimeout.
		context.AfterFunc(ctx, cancel)
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	// Headers draw on the same budget as bodies, for the part of them that
	// survives to be counted. net/http has already read and allocated the block
	// by the time RoundTrip returns, so charging it cannot stop the first
	// oversized response — what it stops is the hundredth, since net/http bounds
	// a single response's block and nothing bounds their sum. What it does not
	// stop is a block padded with whitespace, which normalization removes before
	// resp.Header exists; those bytes are bounded by maxHeaderBytesPerResponse
	// instead, and only on the client this package builds.
	if n := headerBytes(resp); t.take(n) < n {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("mcp: server responses exceed %d bytes in total", t.limit)
	}
	if resp.Body == nil {
		return resp, nil
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, transport: t}
	return resp, nil
}

// headerBytes reconstructs, from the parsed map, a lower bound on what net/http
// read for a response's status line and header block: the status line, then
// "Key: value\r\n" per value, then the blank line that ends the block.
//
// A lower bound and not an approximation, which is the distinction that matters
// and that an earlier comment here got wrong by calling the gap a "bounded
// distortion". The bytes are gone by the time this runs, and normalization does
// not merely perturb their count — textproto trims a value's leading and
// trailing whitespace and joins an obsolete folded continuation, so a server
// that pads deliberately spends header bytes this cannot see at all. Measured
// against a raw listener whose only other header is Content-Length: a value of
// one character followed by 200,000 spaces reconstructs to 49 bytes against
// 200,048 on the wire, a factor of 4,083. Both halves move with whatever else
// the response carries, which is why the fixture is named rather than just the
// pair.
//
// Three routes miss this map. A 1xx informational block — 103 Early Hints, say
// — never enters resp.Header at all, and a server may send many before the final
// response; a one-off probe under net/http's 10 MiB default, not a fixture the
// suite retains, measured 91.8 MB on the wire across 100 pages for 3,900 bytes
// charged, so it sizes the gap this package then closes rather than one that
// survives the cap below.
// Padding whitespace is the second. Trailers are the third: they arrive after
// the body, so they are not in the map when this runs. All three are held by
// maxHeaderBytesPerResponse, one block at a time — over HTTP/2 that cap applies
// to each block separately, so a response cycle admits three of them rather than
// one, and no total follows from that.
//
// Charging what is visible is still worth doing: a server sending large
// *legitimate* headers on every page is charged for them, and splitting one
// large value across many headers or repeating a key is not a way out, both
// measuring within one byte of the wire.
func headerBytes(resp *http.Response) int64 {
	n := int64(len(resp.Proto) + len(resp.Status) + 4)
	for key, values := range resp.Header {
		for _, value := range values {
			n += int64(len(key) + len(value) + 4)
		}
	}
	return n + 2
}

// take draws up to n bytes from the connection's budget, reporting how many are
// available. Zero means the budget is spent.
func (t *limitedTransport) take(n int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > t.budget {
		n = t.budget
	}
	t.budget -= n
	return n
}

// limitedBody is io.LimitedReader with an error instead of a clean EOF: a
// truncated JSON-RPC body that ended quietly would be reported as a parse
// failure, which reads like a broken server rather than a refused one.
//
// A body that ends exactly on the budget is not over it, and telling those two
// apart is the whole difficulty: the reader that ends on the limit and the one
// with a byte more look identical until someone asks for the next byte. So when
// the budget runs out this asks — into a scratch byte of its own, never into the
// caller's buffer. (Reading into the caller's first byte would work too, since
// the refusal returns n = 0 and nothing is delivered either way — the scratch
// array is hygiene, not the thing enforcing the bound.)
// Nothing arrives, the body ended on the limit and is accepted; a byte arrives,
// the server had more and the response is refused.
//
// Funding the budget with a spare byte instead would be simpler and is wrong: it
// makes the headroom byte *deliverable*, so a body of exactly limit+1 whose
// final bytes arrive with io.EOF attached is accepted while the same body split
// one byte earlier is refused. That is the chunking dependence this is here to
// remove, moved one byte along rather than fixed — and net/http attaches EOF
// exactly that way, so the permissive case is the common one, not the exotic one.
type limitedBody struct {
	io.ReadCloser
	transport *limitedTransport
	spent     bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.spent {
		return 0, b.tooBig()
	}
	// A zero-length read must not consult the budget: take(0) returns 0, which
	// is indistinguishable from an exhausted budget and would latch this body
	// spent forever over a read that asked for nothing.
	if len(p) == 0 {
		return 0, nil
	}
	room := b.transport.take(int64(len(p)))
	if room == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		switch {
		case n > 0:
			b.spent = true
			return 0, b.tooBig()
		case err != nil:
			// EOF included: the body ended exactly on the budget, which is
			// within it. Report what the server reported.
			return 0, err
		default:
			// No byte and no error is legal and means "ask again".
			return 0, nil
		}
	}
	n, err := b.ReadCloser.Read(p[:room])
	if unused := room - int64(n); unused > 0 {
		b.transport.give(unused)
	}
	return n, err
}

func (b *limitedBody) tooBig() error {
	return fmt.Errorf("mcp: server responses exceed %d bytes in total", b.transport.limit)
}

// give returns bytes drawn but not used, so a short read does not spend budget a
// later response needs.
func (t *limitedTransport) give(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.budget += n
}

// withBearer returns a shallow copy of client that sends the Authorization
// header to endpoint's origin and to nothing else.
//
// Two separate things keep one server's credential away from another, and each
// answers a leak the other does not.
//
// The **copy** is defence in depth on a client that may be shared — the
// package-level DefaultClient is — where installing the transport in place would
// put this token on every later connection made through it. Today it is a second
// copy rather than the protecting one: Connect runs withResponseLimit first and
// that already returns a copy, so this function never receives the caller's own
// client. It is kept because the ordering it depends on is one line away.
//
// The **origin check** is because net/http's own protection does not apply
// here. net/http strips Authorization when a redirect changes origin, but only
// from headers set on the outbound request; a header a RoundTripper adds is
// invisible to that logic, so there is nothing for it to strip and on a 307 to
// another host the wrapper simply runs again and puts the token on the new
// request. Injecting only for the origin the credential was resolved for closes
// that without depending on the caller's redirect policy — which stays the
// caller's: DefaultClient refuses redirects for its own reasons (replaying a
// request to a target the per-dial guard vets but never approved), and a
// caller supplying its own client owns that decision. What this package
// guarantees either way is narrower and is the part that matters: a credential
// resolved for one server is offered to that server and to no other.
//
// The comparison is textual — scheme, and host case-insensitively — which every
// way of being wrong fails closed. url.Parse lowercases the scheme and keeps
// userinfo out of Host, so those cannot cause a false match; the forms that do
// differ textually while naming the same origin (an explicit :443, a trailing
// dot, a capital in a non-ASCII label) drop the token rather than send it, and
// the request fails as
// unauthenticated instead of leaking. A normalizing comparison would trade that
// direction for the other one.
// withTraceContext puts W3C trace context on every request this client sends,
// as HTTP headers, which is the one propagation route that does not depend on
// the SDK giving a caller somewhere to put it.
//
// [requestMeta] carries the same context in `_meta` on the two requests this
// package issues itself, per the convention MCP 2026-07-28 documents (SEP-414),
// and that is the route a spec-aware MCP server reads. It cannot reach the
// handshake: the SDK builds `server/discover` — and a legacy negotiation's
// `initialize` and `notifications/initialized` — with no hook for a caller's
// metadata. Concluding from that that a connection is untraceable before its
// first request was a mistake in the reasoning rather than in the SDK: the
// transport is this package's, so the headers are.
//
// Design principle 3 is what makes the difference worth a round tripper. Every
// cross-process call propagates OTel context, and a handshake is a cross-process
// call; the case that needs it most is the one with no later request to carry it,
// a connection that fails, whose spans on the server would otherwise correlate
// with nothing. The two routes are complementary rather than redundant — a
// server reading `_meta` sees SEP-414's carrier, a server or gateway reading
// headers sees the same trace — and both are the propagator's own output, so
// neither translates anything.
//
// It rides above the response limit and below the bearer wrapper for no reason
// other than order-independence: none of the three reads what the others write.
func withTraceContext(client *http.Client) *http.Client {
	copied := *client
	copied.Transport = &traceTransport{base: client.Transport}
	return &copied
}

type traceTransport struct{ base http.RoundTripper }

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	carrier := map[string]string{}
	telemetry.Inject(req.Context(), carrier)
	if len(carrier) > 0 {
		// Cloned rather than mutated: a RoundTripper does not own the request it
		// is handed, and net/http may retry it.
		req = req.Clone(req.Context())
		for k, v := range carrier {
			req.Header.Set(k, v)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func withBearer(client *http.Client, token string, endpoint *url.URL) *http.Client {
	copied := *client
	copied.Transport = &bearerTransport{
		base:   client.Transport,
		token:  token,
		scheme: endpoint.Scheme,
		host:   endpoint.Host,
	}
	return &copied
}

// sameHost compares two URL hosts the way DNS and IPv6 respectively require:
// case-insensitively — in ASCII only, for the reason asciiEqualFold argues
// below — except for a scoped address's zone identifier, which is locally
// significant and may distinguish two interfaces that differ only in case. Folding the whole string is the one way this comparison could match
// two origins that are not the same one — the direction that leaks rather than
// withholds — so the zone is split off and compared exactly.
//
// An empty port is dropped from both sides first, because net/http drops it
// from the request: http.NewRequest normalizes "host:" to "host", so an
// endpoint written that way would be compared against a request that no longer
// spells it that way and the header would be withheld from the very server it
// was resolved for.
func sameHost(a, b string) bool {
	a, b = trimEmptyPort(a), trimEmptyPort(b)
	az, bz := strings.IndexByte(a, '%'), strings.IndexByte(b, '%')
	if az < 0 && bz < 0 {
		return asciiEqualFold(a, b)
	}
	if az < 0 || bz < 0 {
		return false
	}
	return asciiEqualFold(a[:az], b[:bz]) && a[az:] == b[bz:]
}

// asciiEqualFold is strings.EqualFold restricted to ASCII: a deliberately
// conservative origin predicate, not a full hostname comparison. It exists for
// the reason the zone identifier is split off above, applied to the host
// itself. strings.EqualFold folds by Unicode, and the Greek sigmas share a fold
// orbit IDNA keeps apart — Go's non-transitional lookup punycodes "σ.example"
// to "xn--4xa.example" and "ς.example" to "xn--3xa.example", two names that can
// belong to two people — so folding by Unicode calls two domains one origin and
// the bearer goes to the second.
//
// Size the cost honestly: it is every cased non-ASCII letter, not a handful of
// exotic ones. "Ä.example" and "ä.example" are one domain after IDNA
// ("xn--4ca.example" either way) and this calls them two, as it does for U+212A
// against "k" and U+017F against "s". It also stops calling two different
// invalid UTF-8 bytes equal, which EqualFold does by decoding both to U+FFFD.
//
// Both the leak and the cost need an origin the caller did not spell: Connect
// parses cfg.URL and hands that same string to the transport, so only a
// redirect or a caller's own client can make the two sides differ, and the
// clients built here refuse redirects. On that path a withheld bearer costs a
// 401 and a wrongly-shared one costs the secret, which is the direction to err
// in. It is not the only way to err less: comparing idna.Lookup.ToASCII on both
// sides separates the sigmas *and* folds every legitimate pair, which is what
// net/http itself does to decide whether Authorization survives a redirect.
// #609 carries that question, because the same choice settles what an
// environment's allowed_hosts should be validated against.
//
// Comparing bytes is safe because ASCII folding preserves length, unlike a
// Unicode one, and because no non-ASCII code point's UTF-8 encoding contains a
// byte in 0x41-0x5A — lead bytes are 0xC2-0xF4 and continuation bytes 0x80-0xBF
// — so the fold cannot reach inside a multi-byte character. Note also that the
// vulnerable pairs are fold *orbits*, not lowercase mappings: EqualFold does not
// merge U+0130 with "i" the way strings.ToLower does, so the example that
// motivates internal/egress's NormalizeHost is not one here.
//
// internal/egress's NormalizeHost and internal/vaultresolve's lowerHost fold the
// same way, for the same reason; those three are what #609 audited, and it rules
// internal/identity's isLoopbackHost out separately — that one folds by Unicode
// too, but only against a fixed localhost/loopback allowlist no mapping aliases
// into. The zoned call above needs the fold as much as the unzoned one, which is
// not obvious: a scoped IPv6 address is hex before the "%" and would fold the
// same either way, but a percent-escaped host reaches that branch too —
// url.Parse reads "http://ex%CF%83%25z.test/" as the host "exσ%z.test" — so its
// address half can be an ordinary name, sigma and all.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// trimEmptyPort is net/http's removeEmptyPort, which is unexported there. The
// trailing colon is the only shape it removes: "[::1]" keeps its brackets and
// "host:0" keeps a port that was actually written.
func trimEmptyPort(host string) string {
	return strings.TrimSuffix(host, ":")
}

type bearerTransport struct {
	base   http.RoundTripper
	token  string
	scheme string
	host   string
}

// RoundTrip needs no nil-base fallback, unlike limitedTransport's: withBearer is
// only ever applied to the client withResponseLimit has just returned, whose
// Transport is the limitedTransport it set. A nil base here would be a
// construction this package does not make.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.scheme || !sameHost(req.URL.Host, t.host) {
		return t.base.RoundTrip(req)
	}
	// RoundTrippers must not modify the request they are given.
	cloned := req.Clone(req.Context())
	// Set, so the vault's credential wins over anything already there. The one
	// header that can be is the Basic net/http derives from an endpoint's own
	// userinfo (client.go, send), and a vault credential is the configured,
	// rotatable one — an endpoint that carries both is a misconfiguration, and
	// sending two credentials or picking the URL's would be the worse answer.
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(cloned)
}
