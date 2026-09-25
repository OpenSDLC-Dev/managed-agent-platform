package domain

import (
	"strings"
	"testing"
)

// TestMCPModelName: the frame is the reference's, and a byte outside the tool-name
// class becomes '_' one for one, so the name is as long as its parts and frame.
func TestMCPModelName(t *testing.T) {
	for _, tc := range []struct{ server, tool, want string }{
		{"docs", "search", "mcp__docs__search"},
		{"docs", "Aa0_-", "mcp__docs__Aa0_-"},
		{"git.hub", "create.issue", "mcp__git_hub__create_issue"},
		{"docs", "search files", "mcp__docs__search_files"},
		// One rune, four bytes: four underscores.
		{"docs", "🔎", "mcp__docs__" + strings.Repeat("_", 4)},
	} {
		got := MCPModelName(tc.server, tc.tool)
		if got != tc.want {
			t.Errorf("MCPModelName(%q, %q) = %q, want %q", tc.server, tc.tool, got, tc.want)
		}
		if n := len(MCPModelNamePrefix) + len(tc.server) + len(MCPModelNameSeparator) + len(tc.tool); len(got) != n {
			t.Errorf("MCPModelName(%q, %q) is %d bytes, want %d", tc.server, tc.tool, len(got), n)
		}
	}
}
