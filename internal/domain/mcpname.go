package domain

// The frame of the name an MCP tool is offered to the model under. The reference
// composes the name it shows the model and decomposes it on the event it
// commits, which carries the server and the bare tool in fields of their own
// (docs/DIVERGENCES.md, "Offering an MCP tool to the model").
const (
	MCPModelNamePrefix    = "mcp__"
	MCPModelNameSeparator = "__"
)

// MCPModelName composes the name an MCP tool is offered to the model under:
// the tool named by the event's mcp_server_name and name. Anything that shows
// the model a call it made — replay rebuilding the assistant block, a denial's
// text — names it this way, so the model reads the name it called.
//
// Bytes outside the Messages tool-name class [a-zA-Z0-9_-] become '_', one for
// one, so the name is exactly as long as its parts and frame — the length a
// caller bounding it can settle before composing. Mangling rather than dropping
// is what keeps a whole server's tools from vanishing over a naming convention:
// internal/mcp admits the SDK's own tool-name class, which includes '.', so a
// server publishing `github.create_issue` would otherwise offer nothing at all.
// A mangled name costs nothing a reader of the log can see, since the event
// keeps the pair; two tools that mangle to one name contest it exactly as two
// that composed to one do.
func MCPModelName(server, tool string) string {
	b := make([]byte, 0, len(MCPModelNamePrefix)+len(server)+len(MCPModelNameSeparator)+len(tool))
	b = append(b, MCPModelNamePrefix...)
	b = appendModelName(b, server)
	b = append(b, MCPModelNameSeparator...)
	b = appendModelName(b, tool)
	return string(b)
}

func appendModelName(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return b
}
