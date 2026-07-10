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
// adapter is near-zero-conversion; lossy mappings are confined to future
// non-Anthropic adapters.
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

// Factory constructs a Provider for one protocol.
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
type Registry struct {
	routes    map[string]Config
	fallback  *Config
	factories map[string]Factory
}

func NewRegistry(routes []Route, factories map[string]Factory) (*Registry, error) {
	r := &Registry{routes: make(map[string]Config, len(routes)), factories: factories}
	for _, route := range routes {
		if route.Model == "" {
			return nil, fmt.Errorf("route model must not be empty (use %q for the default route)", "*")
		}
		if route.Config.Protocol == "" || route.Config.BaseURL == "" {
			return nil, fmt.Errorf("route %q needs a protocol and a base_url", route.Model)
		}
		if _, ok := factories[route.Config.Protocol]; !ok {
			return nil, fmt.Errorf("route %q uses unknown protocol %q", route.Model, route.Config.Protocol)
		}
		if route.Model == "*" {
			if r.fallback != nil {
				return nil, fmt.Errorf("duplicate default (%q) route", "*")
			}
			cfg := route.Config
			r.fallback = &cfg
			continue
		}
		if _, dup := r.routes[route.Model]; dup {
			return nil, fmt.Errorf("duplicate route for model %q", route.Model)
		}
		r.routes[route.Model] = route.Config
	}
	return r, nil
}

// Provider constructs the provider for an agent's model string. When the
// route's config has no upstream model set, the agent's own model string
// passes through (a gateway that understands the platform's model names).
func (r *Registry) Provider(model string) (Provider, error) {
	cfg, ok := r.routes[model]
	if !ok {
		if r.fallback == nil {
			return nil, fmt.Errorf("no provider route for model %q", model)
		}
		cfg = *r.fallback
	}
	if cfg.Model == "" {
		cfg.Model = model
	}
	return r.factories[cfg.Protocol](cfg)
}
