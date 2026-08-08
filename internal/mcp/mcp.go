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
// Leaving it on would open a GET a modern server answers with 405.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server URL is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = DefaultClient
	}
	if cfg.BearerToken != "" {
		httpClient = withBearer(httpClient, cfg.BearerToken)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientName}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             cfg.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %s: %w", cfg.URL, err)
	}
	return &Conn{session: session}, nil
}

// ListTools returns every tool the server reports, following pagination to the
// end. A server that reports none is not an error: the docs make an MCP server
// with no tools, or a tool name a config addresses but the server does not
// offer, a warning rather than a failure, so the empty catalog is a fact to
// record and not a reason to fail the work item.
func (c *Conn) ListTools(ctx context.Context) ([]Tool, error) {
	out := []Tool{}
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			// InputSchema arrives as the server's decoded JSON, so this cannot
			// fail in practice; a tool whose schema will not round-trip is
			// still dropped rather than offered to the model with no schema.
			return nil, fmt.Errorf("mcp: tool %q input schema: %w", tool.Name, err)
		}
		out = append(out, Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

// Close ends the connection. It is safe to call on a Conn whose work failed.
func (c *Conn) Close() error { return c.session.Close() }

// withBearer returns a shallow copy of client whose transport adds the
// Authorization header. The copy matters: Config.HTTPClient may be shared (the
// package-level DefaultClient is), and mutating it would leak one server's
// credential onto every later connection.
func withBearer(client *http.Client, token string) *http.Client {
	copied := *client
	copied.Transport = &bearerTransport{base: client.Transport, token: token}
	return &copied
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	// RoundTrippers must not modify the request they are given.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(cloned)
}
