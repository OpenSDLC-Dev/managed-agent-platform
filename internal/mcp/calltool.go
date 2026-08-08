package mcp

import (
	"context"
	"encoding/json"
	"fmt"

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

// CallTool runs one tool on the connected server and returns its answer.
//
// arguments is sent verbatim: json.RawMessage marshals as itself, so the bytes
// the model produced reach the server unaltered rather than through a
// map[string]any round trip that would reorder keys and re-render numbers.
// Empty arguments become the empty object the SDK sends in place of a nil.
//
// A call is one request, so it is bounded by whatever bounds a request on the
// connection's HTTP client — under [DefaultClient], [DialTimeout] as a
// whole-request cap; under a client that sets no Timeout, the same duration as
// the fallback deadline this package's transport installs. There is no
// listing-style aggregate budget because there is nothing to aggregate.
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
// 2026-07-28 (SEP-2322) — and the SDK's own client middleware answers those
// requests and re-sends the call, up to ten attempts, before giving up with an
// error. So a server that keeps asking costs ten round trips and then fails the
// call; the result never reaches here still carrying the requests, which is why
// nothing below looks for them. This platform offers no interactive surface to
// fulfil such a request with, and a bounded failure is the right end for a call
// it cannot complete — what matters is that it is a failure the caller sees
// rather than an empty answer the model cannot account for.
func (c *Conn) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*CallResult, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("mcp: call tool on a connection that was never opened")
	}
	var args any
	if len(arguments) > 0 {
		args = arguments
	}
	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{
		Meta:      requestMeta(ctx),
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
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
	return out, nil
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
