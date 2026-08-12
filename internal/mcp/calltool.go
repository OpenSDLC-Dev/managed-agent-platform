package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallResult is one tool call's answer.
//
// IsError is the tool's verdict, never the transport's. MCP asks a server to
// report a tool that ran and failed as an ordinary result with IsError set —
// "otherwise, the LLM would not be able to see that an error occurred and
// self-correct" — and to reserve JSON-RPC errors for the call not happening at
// all. CallTool keeps the two apart on the same line: a failed tool comes back
// as a CallResult with a nil error, and a nil CallResult means the platform,
// not the model, has something to fix.
type CallResult struct {
	// Content is the answer's blocks in the order the server sent them.
	Content []Content
	// IsError reports a tool that ran and failed on its own terms.
	IsError bool
}

// Content is one block of a tool call's answer, in a shape this package owns
// rather than the SDK's content interface (CLAUDE.md design principle 1).
//
// One flat struct covers all five block types a tool result may carry, and no
// field is nonsense for the type that leaves it empty: text fills Text, image
// and audio fill Data and MIMEType, an embedded resource fills URI, MIMEType
// and whichever of Text (a text resource) or Data (a blob) it carries, and a
// resource link fills URI and MIMEType. Rendering these into the block types an
// Anthropic tool result admits is the caller's, not this package's: what a
// client owes its caller is the server's answer intact.
type Content struct {
	// Type is the MCP content type: "text", "image", "audio", "resource" or
	// "resource_link". Those five are exactly what the protocol admits inside a
	// tool result, and a block of any other type is dropped rather than
	// guessed at — the SDK's decoder also accepts the two sampling-only types
	// (tool_use, tool_result) here, which a tool result has no business
	// carrying and this platform has nowhere to put.
	Type string
	// Text is a text block's body, or an embedded text resource's.
	Text string
	// Data is an image's or audio's bytes, or an embedded blob resource's,
	// already base64-decoded.
	Data []byte
	// MIMEType describes Data, or the resource behind URI. Servers are not
	// required to send one.
	MIMEType string
	// URI names the resource, for "resource" and "resource_link" alone.
	URI string
}

// CallTimeout bounds a whole call rather than any one request inside it, the
// way [ListTimeout] bounds a whole listing. A call is one request only for as
// long as the server answers it with the tool's output: under the multi
// round-trip pattern below, the SDK re-sends the call up to ten times, each
// attempt bounded only on its own, so without an aggregate budget a server that
// keeps asking holds the caller — and the queue lease behind it — for ten times
// a single request's cap.
//
// Two minutes because that is what a tool call gets on this platform already
// (toolset.DefaultTimeout), and an MCP tool is a tool: a remote one should not
// outlive a local one by default.
//
// It is a ceiling on the aggregate and not a promise about how long a call may
// run, and under [DefaultClient] it is not the binding constraint at all: that
// client sets Timeout to [DialTimeout], a whole-request cap net/http applies to
// the body read as well, and a `tools/call` response is not complete until the
// tool is. So a call through the production client is bounded at thirty seconds
// and an MCP tool that takes longer fails every time, whatever this constant
// says — the same relationship [ListTimeout] has with that cap, where the
// aggregate bounds the pages and the client bounds each one. Whether an MCP tool
// deserves a client of its own with a longer request cap is a question for the
// slice that first calls one; today nothing does, so nothing here answers it.
const CallTimeout = 2 * time.Minute

// CallTool runs one tool on the connected server and returns its answer.
//
// arguments is sent verbatim: json.RawMessage marshals as itself, so the bytes
// the model produced reach the server unaltered rather than through a
// map[string]any round trip that would reorder keys and re-render numbers.
// Empty arguments become the empty object the SDK sends in place of a nil.
//
// One thing a fresh connection does not do, which matters when a server uses it:
// the SDK lifts a tool parameter annotated `x-mcp-header` (SEP-2243) out of the
// arguments and onto an HTTP header only for a tool it already has cached from a
// `tools/list` on this same session (ClientSession.lookupTool). Connections here
// are per-work-item and a caller that only calls never lists, so such a
// parameter travels in the JSON body instead.
//
// One request is also not always one round trip. A server may answer with
// `resultType: "input_required"` instead of the tool's output — the multi
// round-trip pattern that replaced roots, sampling and elicitation in MCP
// 2026-07-28 (SEP-2322). This platform offers no interactive surface to fulfil
// such a request with, so every shape of it ends the call, and the three shapes
// end it differently because the SDK's client middleware handles them
// differently:
//
//   - `inputRequests` with entries: the middleware answers them itself and
//     re-sends the call, up to ten attempts, then fails. The result never
//     reaches here still carrying them.
//   - `inputRequests` present and empty — which the spec gives servers as a
//     load-shedding signal: the middleware retries a few times and fails.
//   - `inputRequests` absent altogether: the middleware's retry loop turns on a
//     non-nil map, so this one is handed straight back, and it is the shape
//     that would otherwise pass for success. The tool did not run and the
//     answer carries no output, so a caller reading only Content would show the
//     model an empty result from a tool that was never executed. It is refused
//     here instead.
func (c *Conn) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*CallResult, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("mcp: call tool on a connection that was never opened")
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	var args any
	if len(arguments) > 0 {
		args = arguments
	}
	res, err := c.callTool(ctx, &sdk.CallToolParams{
		Meta:      requestMeta(ctx),
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}
	if res.NeedsInput() {
		return nil, fmt.Errorf("mcp: call tool %q: the server asked for further input, "+
			"which this platform has no surface to supply", name)
	}
	out := &CallResult{IsError: res.IsError, Content: convertContent(res.Content)}
	// A tool that declares an outputSchema answers with structuredContent, and
	// the spec only SHOULDs that it also duplicate the value as a text block.
	// Where a server takes it at its word, the content array is empty and the
	// model would be handed an answer with nothing in it, so the structured
	// value becomes the text block the server did not send. Only as a fallback:
	// appending it unconditionally would double every answer from the servers
	// that do follow the recommendation.
	if len(out.Content) == 0 && res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("mcp: call tool %q: structured content: %w", name, err)
		}
		out.Content = append(out.Content, Content{Type: "text", Text: string(b)})
	}
	// An answer whose every block was dropped, with no structured value behind
	// it, is not an empty answer — it is an answer this platform could not read.
	// The two are indistinguishable to a model, and the wrong one of them is the
	// one it can act on, so the difference is kept here: a tool that returned
	// nothing succeeds with no content, and a tool whose whole answer was
	// untranslatable fails loudly enough to be fixed.
	if len(out.Content) == 0 && len(res.Content) > 0 {
		return nil, fmt.Errorf("mcp: call tool %q: the answer's %d content block(s) are all of "+
			"types a tool result cannot carry", name, len(res.Content))
	}
	return out, nil
}

// callTool sends the call, converting a panic inside the SDK into an error —
// the same containment [Conn.listPage] gives the listing, at the other of this
// package's two SDK call sites, for a second and unrelated nil dereference.
//
// A result's `inputRequests` decodes through InputRequestMap.UnmarshalJSON
// (mcp/protocol.go), which unmarshals the wire into a map[string]*raw, checks
// only that the map itself is non-nil, and then reads a field off every value
// in it. A server answering `"inputRequests": {"x": null}` therefore panics the
// client, and it panics *during* the decode, on this goroutine, inside this
// frame — so a recover here contains it and nothing further out is needed.
// (The listing's panic is a different bug in a different place: a nil element
// of a `[]*Tool` dereferenced after the decode by post-decode validation. The
// content blocks are safe; the SDK nil-checks those, and this platform's own
// guard against a nil embedded resource sits after the decode in
// convertContent.)
func (c *Conn) callTool(ctx context.Context, params *sdk.CallToolParams) (res *sdk.CallToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("the MCP client library panicked on this server's response: %v", r)
		}
	}()
	return c.session.CallTool(ctx, params)
}

// convertContent translates the SDK's content blocks into this package's.
func convertContent(blocks []sdk.Content) []Content {
	out := make([]Content, 0, len(blocks))
	for _, block := range blocks {
		switch v := block.(type) {
		case *sdk.TextContent:
			out = append(out, Content{Type: "text", Text: v.Text})
		case *sdk.ImageContent:
			out = append(out, Content{Type: "image", Data: v.Data, MIMEType: v.MIMEType})
		case *sdk.AudioContent:
			out = append(out, Content{Type: "audio", Data: v.Data, MIMEType: v.MIMEType})
		case *sdk.ResourceLink:
			out = append(out, Content{Type: "resource_link", URI: v.URI, MIMEType: v.MIMEType})
		case *sdk.EmbeddedResource:
			// `{"type": "resource"}` with no resource decodes to this, and the
			// SDK does not check it: the block is the resource, so one without
			// it carries nothing to hand on.
			if v.Resource == nil {
				continue
			}
			out = append(out, Content{
				Type:     "resource",
				Text:     v.Resource.Text,
				Data:     v.Resource.Blob,
				MIMEType: v.Resource.MIMEType,
				URI:      v.Resource.URI,
			})
		}
	}
	return out
}
