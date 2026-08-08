// Package mcp is the platform's MCP client: a thin wrapper over the official
// github.com/modelcontextprotocol/go-sdk exposing what the platform needs and
// nothing else.
//
// Connections are per-work-item. A caller connects, does its work, and closes;
// nothing is pooled and nothing is shared, so a crashed executor loses no state
// a fresh one cannot rebuild. The 2026-07-28 revision of MCP makes that cheap:
// it removed protocol-level sessions, so there is no handshake to amortize and
// no affinity between the discovery of a server's tools and a later call to
// one. Everything below is deliberately stateless for the same reason.
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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DialTimeout bounds a single connection attempt and each request on it. MCP
// servers are third-party endpoints reached from a work item that holds a
// queue lease, so an unbounded wait would hold the lease rather than fail the
// item.
const DialTimeout = 30 * time.Second

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
	if cfg.BearerToken != "" {
		httpClient = withBearer(httpClient, cfg.BearerToken, endpoint)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientName}, nil)
	session, connErr := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             cfg.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if connErr != nil {
		return nil, fmt.Errorf("mcp: connect to %s: %w", cfg.URL, connErr)
	}
	return &Conn{session: session}, nil
}

// maxToolPages bounds how many pages one listing will fetch. Pagination is
// driven entirely by the server — it decides when to stop by omitting the next
// cursor — so an unbounded loop here is a server-controlled loop, and it runs
// inside a work item holding a queue lease. The bound is a safety valve, not a
// product limit: no real catalog comes close, and a server that legitimately
// needs more pages than this is one whose page size is the actual problem.
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
			if tool == nil || tool.Name == "" {
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

// inputSchema renders one tool's declared input schema as the JSON an Anthropic
// tool definition carries, reporting false for a tool that cannot be offered to
// a model at all.
//
// The SDK types this field as `any` and, client-side, fills it with whatever
// the server's JSON decoded to. MCP requires an object; a server is free to
// break that. An absent schema is read as its faithful equivalent — an object
// with no declared parameters, which is what a no-argument tool means and what
// most servers send explicitly — while a schema that is present and is not an
// object is refused, because there is no honest translation of it and inventing
// one would misdescribe the tool to the model.
//
// A refused tool is skipped rather than failing the listing: one malformed
// entry must not deny an agent the rest of a server's tools, which matches the
// reference's treatment of an unresolvable tool name as a warning.
//
// What comes back is the schema's meaning, not its bytes. The SDK hands over a
// value already decoded into `any`, so re-marshaling sorts the object's keys
// and renders every number through float64 — an integer bound above 2^53 does
// not survive that. Reaching the original bytes would mean not using the SDK's
// typed result at all, and a JSON Schema whose correctness depends on key order
// or on a 17-digit integer is not one this platform needs to carry.
func inputSchema(declared any) (json.RawMessage, bool) {
	if declared == nil {
		return json.RawMessage(`{"type":"object"}`), true
	}
	raw, err := json.Marshal(declared)
	if err != nil {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	return raw, true
}

// Close ends the connection. It is safe to call on a Conn whose work failed.
func (c *Conn) Close() error { return c.session.Close() }

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

type bearerTransport struct {
	base   http.RoundTripper
	token  string
	scheme string
	host   string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.URL.Scheme != t.scheme || !strings.EqualFold(req.URL.Host, t.host) {
		return base.RoundTrip(req)
	}
	// RoundTrippers must not modify the request they are given.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(cloned)
}
