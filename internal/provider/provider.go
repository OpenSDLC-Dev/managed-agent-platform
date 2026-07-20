// Package provider abstracts model backends behind Anthropic Messages
// semantics. A provider instance is constructed purely from configuration
// (protocol / model / base_url / api_key — CLAUDE.md principle 4): the
// anthropic protocol adapter works against ANY endpoint speaking Anthropic
// Messages — a gateway, a proxy, or a self-hosted model — never just
// api.anthropic.com. The registry resolves an agent's model string to a
// configured provider.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Config constructs one provider instance.
type Config struct {
	Protocol string            // "anthropic" | "openai"
	Model    string            // model id sent to the upstream endpoint
	BaseURL  string            // endpoint base URL; required (no implicit default host)
	APIKey   string            // credential for the endpoint
	Headers  map[string]string // optional extra headers (gateway routing etc.)
}

// Request is one model turn in Anthropic Messages semantics. Content and
// tool definitions stay as raw Anthropic wire JSON so the anthropic-protocol
// adapter is near-zero-conversion; lossy mappings are confined to the
// non-Anthropic adapters (see provider/openai).
type Request struct {
	System    string
	Messages  []Message
	Tools     []json.RawMessage // Anthropic tool definitions, verbatim
	MaxTokens int64
}

// Message is one conversational turn.
type Message struct {
	Role    string          // "user" | "assistant"
	Content json.RawMessage // Anthropic content: a string or an array of blocks
}

// ChunkKind discriminates streaming increments of a turn.
type ChunkKind string

const (
	// KindTextDelta appends text to the content block at Index.
	KindTextDelta ChunkKind = "text_delta"
	// KindThinkingDelta appends thinking text to the block at Index.
	KindThinkingDelta ChunkKind = "thinking_delta"
	// KindToolUse is one complete tool invocation (input fully accumulated).
	KindToolUse ChunkKind = "tool_use"
	// KindDone closes the turn with stop reason and usage.
	KindDone ChunkKind = "done"
)

// Chunk is one streaming increment.
type Chunk struct {
	Kind  ChunkKind
	Index int64  // content block index (text/thinking deltas)
	Text  string // text/thinking fragment

	ToolUse *ToolUse // KindToolUse only

	StopReason string             // KindDone only: end_turn | tool_use | max_tokens | …
	Usage      *domain.ModelUsage // KindDone only
}

// ToolUse is a complete tool call emitted by the model.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Stream yields one turn's chunks in order.
//
//	for stream.Next() { chunk := stream.Chunk(); … }
//	if err := stream.Err(); err != nil { … }
type Stream interface {
	Next() bool
	Chunk() Chunk
	Err() error
	Close() error
}

// Provider runs single model turns against one configured backend.
type Provider interface {
	Generate(ctx context.Context, req Request) (Stream, error)
}

// Factory constructs a Provider for one protocol. It must be cheap and must
// acquire no per-instance resource — no private http.Transport, no connection
// pool, no goroutine — because the registry calls it once per turn rather than
// retaining what it builds (see Registry). Both adapters satisfy this by
// sharing http.DefaultClient.
//
// It must also be safe to call concurrently: the registry holds no lock, so
// turns in flight on different sessions enter the factory at the same time.
//
// An adapter that genuinely cannot be cheap must cache the shared resource
// itself, and must key that cache by the endpoint alone — protocol, base URL,
// credential, headers. NOT by the whole Config: under a pass-through route
// Config.Model is the agent's model string, so a Config-keyed cache would be
// keyed by client input and would rebuild issue #88 inside the adapter.
type Factory func(Config) (Provider, error)

// Route binds an agent-facing model string to a provider config. Model "*"
// is the default route.
type Route struct {
	Model  string
	Config Config
}

// Registry resolves agent model strings to constructed providers. Routing is
// exact-match with an optional "*" default, so an enterprise maps its model
// names onto endpoints purely in configuration.
//
// Nothing is retained, deliberately. Providers used to be cached per model
// string, and under a "*" route that string is whatever a client put on its
// agent — so the map grew without bound in the long-running brain (issue #88).
// Keying it by the route instead would bound it, but the cache was buying too
// little to pay for the branch: a constructed provider is a value struct over
// the shared http.DefaultClient, the connection pool that matters lives in the
// process-global http.DefaultTransport, and Provider is called once per turn
// immediately before a model round trip that dwarfs the construction. With
// nothing to insert into, the bound is a property of the type rather than of a
// policy — and the registry owns copies of everything it was given, so it is
// immutable after NewRegistry and needs no lock.
type Registry struct {
	routes    map[string]Config
	fallback  *Config
	factories map[string]Factory
}

func NewRegistry(routes []Route, factories map[string]Factory) (*Registry, error) {
	// The registry owns its copies — of the factory table as much as of each
	// route's Headers below. Aliasing the caller's map would leave a live
	// handle on the dispatch table the lock-free Provider path reads.
	r := &Registry{
		routes:    make(map[string]Config, len(routes)),
		factories: make(map[string]Factory, len(factories)),
	}
	for protocol, f := range factories {
		r.factories[protocol] = f
	}
	for _, route := range routes {
		if route.Model == "" {
			return nil, fmt.Errorf("route model must not be empty (use %q for the default route)", "*")
		}
		if route.Config.Protocol == "" || route.Config.BaseURL == "" {
			return nil, fmt.Errorf("route %q needs a protocol and a base_url", route.Model)
		}
		// Validated against the registry's own copy, and by nil rather than
		// presence: Provider dispatches through this table without a nil
		// check, so a protocol mapped to a nil Factory would pass a
		// comma-ok test and panic a turn instead of failing construction.
		if r.factories[route.Config.Protocol] == nil {
			return nil, fmt.Errorf("route %q uses unknown protocol %q", route.Model, route.Config.Protocol)
		}
		// The registry owns its config copies: Headers is a reference
		// type, and sharing it with the caller's Route slice would let a
		// later mutation reach every constructed provider.
		cfg := route.Config
		cfg.Headers = cloneHeaders(route.Config.Headers)
		if route.Model == "*" {
			if r.fallback != nil {
				return nil, fmt.Errorf("duplicate default (%q) route", "*")
			}
			r.fallback = &cfg
			continue
		}
		if _, dup := r.routes[route.Model]; dup {
			return nil, fmt.Errorf("duplicate route for model %q", route.Model)
		}
		r.routes[route.Model] = cfg
	}
	return r, nil
}

// Descriptor is what may be said out loud about the backend a model routes to:
// the protocol its endpoint speaks and the model id sent upstream. It exists so
// telemetry can name the backend without being handed a Config, which carries
// the credential — the redaction is the type's shape, not a caller's discipline.
type Descriptor struct {
	Protocol string
	Model    string
}

// Describe resolves a model string to its backend's Descriptor, reporting
// whether a route exists. It answers from configuration alone and never
// constructs a provider, so telemetry about an unroutable model costs nothing.
func (r *Registry) Describe(model string) (Descriptor, bool) {
	cfg, ok := r.route(model)
	if !ok {
		return Descriptor{}, false
	}
	return Descriptor{Protocol: cfg.Protocol, Model: cfg.Model}, true
}

// route resolves a model string to its config, applying the pass-through of the
// agent's own model name when the route names no upstream one. That the
// passed-through string is client-controlled — it reaches here from an agent's
// spec or from a session's agent_with_overrides, and so, via Describe, sets the
// gen_ai.request.model metric attribute's cardinality — is a deliberate
// operator responsibility, not an oversight: see deploy/compose/README.md and
// issue #88.
func (r *Registry) route(model string) (Config, bool) {
	cfg, ok := r.routes[model]
	if !ok {
		if r.fallback == nil {
			return Config{}, false
		}
		cfg = *r.fallback
	}
	if cfg.Model == "" {
		cfg.Model = model
	}
	return cfg, true
}

// Provider constructs the provider for an agent's model string. When the
// route's config has no upstream model set, the agent's own model string
// passes through (a gateway that understands the platform's model names).
// Every call constructs a fresh instance; see the Registry doc for why.
func (r *Registry) Provider(model string) (Provider, error) {
	cfg, ok := r.route(model)
	if !ok {
		return nil, fmt.Errorf("no provider route for model %q", model)
	}
	cfg.Headers = cloneHeaders(cfg.Headers)
	return r.factories[cfg.Protocol](cfg)
}

func cloneHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}
