// Package mcp is the platform's MCP client: a thin wrapper over the official
// github.com/modelcontextprotocol/go-sdk exposing what the platform needs and
// nothing else.
//
// Connections are per-work-item. A caller connects, does its work, and closes;
// nothing is pooled and nothing is shared, so a crashed executor loses no state
// a fresh one cannot rebuild. The 2026-07-28 revision of MCP is what makes that
// affordable: it removed protocol-level sessions, so nothing accumulates on the
// server that a new connection has to re-establish, and there is no affinity
// between discovering a server's tools and later calling one. Not free, though —
// a connection still negotiates, and this client's negotiation is a `server/
// discover` round trip (two, where the SDK retries it) before Connect returns.
// What per-work-item connections cost is that round trip; what they buy is a
// client with no state to lose.
//
// The SDK is a dependency of this package alone. Its types do not appear in the
// wrapper's surface — the platform's domain model is Anthropic-native
// (CLAUDE.md design principle 1), and an MCP tool reaches the rest of the
// system as the platform's own [Tool], not as an SDK struct.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
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
// whole-request cap that beats a longer context deadline. So under the
// production client every request is bounded at 30s, and ListTimeout's two
// minutes bound the listing across its pages rather than any single one of them.
// A caller supplying its own client sets that policy itself, and may set none —
// which is the case the fallback exists for.
const DialTimeout = 30 * time.Second

// ListTimeout bounds a whole listing rather than one request in it. Pagination
// is server-driven, so without an aggregate budget a server that answers each
// page just inside DialTimeout holds the work item — and the queue lease behind
// it — for maxToolPages requests in a row.
const ListTimeout = 2 * time.Minute

// clientName identifies this platform to MCP servers. The protocol carries it
// on every request's `_meta` (2026-07-28) so a server operator can see which
// client is calling.
const clientName = "managed-agent-platform"

// Config describes one MCP server connection.
type Config struct {
	// URL is the server's MCP endpoint, from the agent's mcp_servers entry.
	URL string
	// BearerToken, when set, is sent as `Authorization: Bearer`. It comes from
	// a session vault's matching credential; an empty token connects
	// anonymously, which is the reference's documented no-match behavior.
	BearerToken string
	// HTTPClient overrides the guarded client. Nil selects [DefaultClient],
	// which is what production uses; a test supplies its own to reach an
	// httptest server on loopback.
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
var DefaultClient = &http.Client{
	Timeout: DialTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: DialTimeout,
			Control: dialguard.Control(dialguard.IPAllowed),
		}).DialContext,
	},
}

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
// or later (streamable.go, sessionUpdated), so this only takes effect on an
// older negotiation, where the spec makes the GET optional and a tools/list
// answer comes back on the POST regardless. That is also why the contract suite
// cannot assert it behaviorally: every fixture here negotiates 2026-07-28, so
// removing this line leaves the suite green. It is kept as the explicit
// statement of what this client wants — one request, one answer, no listener —
// on the revisions where the SDK still asks.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server URL is required")
	}
	endpoint, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp: server URL %q: %w", cfg.URL, err)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = DefaultClient
	}
	httpClient = withResponseLimit(httpClient)
	if cfg.BearerToken != "" {
		httpClient = withBearer(httpClient, cfg.BearerToken, endpoint)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientName}, nil)
	// The SDK closes the session it built on most failure paths but not on all
	// of them — an initialize answered with a protocol version it does not
	// support returns before Close, where the two adjacent branches call it —
	// and Connect hands back no session for the caller to close. Capturing the
	// transport's connection is the only handle left: the wrapper returns the
	// SDK's own Connection unchanged (the SDK type-asserts it to reach
	// sessionUpdated, so substituting anything would break negotiation) and
	// keeps a reference so a failed Connect does not leave the reader goroutine
	// and its HTTP connection alive on a server that answers that way to every
	// attempt.
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
		return nil, fmt.Errorf("mcp: connect to %s: %w", cfg.URL, connErr)
	}
	return &Conn{session: session}, nil
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
// end. A server that reports none is not an error: the docs make an MCP server
// with no tools, or a tool name a config addresses but the server does not
// offer, a warning rather than a failure, so the empty catalog is a fact to
// record and not a reason to fail the work item.
//
// Pagination is driven here rather than through the SDK's Tools iterator
// because the iterator hides the cursor, and the cursor is the only thing that
// says whether a server is paginating or looping: the iterator follows
// `nextCursor` until a server omits it, which a server that keeps returning the
// same cursor never does. Everything a server sends is treated as hostile until
// checked — this endpoint is customer-supplied, and the SDK decodes rather than
// validates it.
func (c *Conn) ListTools(ctx context.Context) ([]Tool, error) {
	if c == nil || c.session == nil {
		// Before the recover below, so this package's own misuse is not
		// reported as a server that crashed the client library.
		return nil, fmt.Errorf("mcp: list tools on a connection that was never opened")
	}
	// One budget for the whole listing, not one per request. A server that
	// answers every page just inside DialTimeout would otherwise hold a work
	// item — and its queue lease — for maxToolPages × DialTimeout, which is
	// most of an hour.
	ctx, cancel := context.WithTimeout(ctx, ListTimeout)
	defer cancel()

	out := []Tool{}
	var cursor string
	for page := 0; page < maxToolPages; page++ {
		res, err := c.listPage(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
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
			schema, ok := inputSchema(tool.InputSchema)
			if !ok {
				continue
			}
			out = append(out, Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if res.NextCursor == cursor {
			// Distinct from the page bound on purpose: this is the shape a
			// broken cursor implementation actually takes, and catching it on
			// the second request rather than the hundredth keeps a wedged
			// server from holding the lease for ninety-eight round trips.
			return nil, fmt.Errorf("mcp: list tools: server repeated its pagination cursor")
		}
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
	return c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
}

// usableName reports whether a server's tool name can be offered to a model.
//
// The rule is the MCP SDK's own — non-empty, at most 128 bytes, and the runes
// [a-zA-Z0-9_.-] (validateToolName in mcp/tool.go; the 128 is a len(), so bytes,
// whatever its error message calls them). Note what the SDK does *not* do with
// it: AddTool logs a violation and registers the tool anyway, and nothing checks
// it on the client side at all, so a name that breaks the rule reaches a client
// as a perfectly ordinary listing entry. That makes this the first place it can
// be caught, not a redundant second one. A name that fails it
// reaches the model as a tool definition the Messages API refuses, and refusing
// one tool is better than a request that fails carrying every other tool with
// it. Deliberately the SDK's rule and not a guessed-at Anthropic one: if the
// reference is stricter, the place to enforce that is where the request is
// assembled and the constraint can be checked against the reference, not here.
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
// value already decoded into `any`, so re-marshaling sorts the object's keys
// and renders every number through float64 — an integer bound above 2^53 does
// not survive that. Reaching the original bytes would mean not using the SDK's
// typed result at all, and a JSON Schema whose correctness depends on key order
// or on a 17-digit integer is not one this platform needs to carry.
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

// MaxResponseBytes bounds the total a connection will read from a server across
// every response, not one response at a time.
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
// retain it: the SDK caches every modern tools/list result under its cursor
// (mcp/client.go ListTools → toolsCache.put) and never evicts an entry until the
// same cursor is asked for again, while ListTools re-marshals each accepted
// schema into its own result. A hundred unique cursors therefore hold a hundred
// pages live, twice over. Cumulative makes the bound mean what it says.
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
// context with xcontext.Detach, which reports no deadline and a nil Done
// channel, and sends the session-ending DELETE on it (mcp/streamable.go,
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
// it wraps draws from one counter. One connection is used by one work item at a
// time, but the SDK is free to have a request in flight while a body is still
// being read, so the counter is mutex-guarded rather than assumed serial.
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
		// so the deadline has to outlive RoundTrip. context.AfterFunc keeps the
		// timer from leaking past it either way.
		context.AfterFunc(ctx, cancel)
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, transport: t}
	return resp, nil
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
// The **copy** is because Config.HTTPClient may be shared — the package-level
// DefaultClient is — so installing the transport on the caller's own client
// would put this token on every later connection made through it.
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
// dot) drop the token rather than send it, and the request fails as
// unauthenticated instead of leaking. A normalizing comparison would trade that
// direction for the other one.
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
// case-insensitively, except for a scoped address's zone identifier, which is
// locally significant and may distinguish two interfaces that differ only in
// case. Folding the whole string is the one way this comparison could match
// two origins that are not the same one — the direction that leaks rather than
// withholds — so the zone is split off and compared exactly.
func sameHost(a, b string) bool {
	az, bz := strings.IndexByte(a, '%'), strings.IndexByte(b, '%')
	if az < 0 && bz < 0 {
		return strings.EqualFold(a, b)
	}
	if az < 0 || bz < 0 {
		return false
	}
	return strings.EqualFold(a[:az], b[:bz]) && a[az:] == b[bz:]
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
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(cloned)
}
