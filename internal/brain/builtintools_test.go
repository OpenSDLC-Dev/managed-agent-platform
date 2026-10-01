package brain_test

import (
	"context"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The request names its built-in tools by provenance — the definitions the
// agent_toolset expansion supplied — because that set is the only licence a
// lossy adapter has to rewrite a schema (#682). A custom tool that takes a
// disabled built-in's name is the case a name test would get wrong: it is
// offered, under that name, and stays out of the set.
func TestRequestNamesItsBuiltinsByProvenance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
		want  []string
	}{
		{
			"the whole toolset",
			`[{"type":"agent_toolset_20260401"}]`,
			[]string{"bash", "edit", "glob", "grep", "read", "web_fetch", "web_search", "write"},
		},
		{
			"a custom tool under a disabled built-in's name",
			`[{"type":"agent_toolset_20260401","configs":[{"name":"web_search","enabled":false}]},` +
				`{"type":"custom","name":"web_search","description":"ours",` +
				`"input_schema":{"type":"object","properties":{"q":{"type":"string","minLength":3}},"additionalProperties":false}}]`,
			[]string{"bash", "edit", "glob", "grep", "read", "web_fetch", "write"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, [][]provider.Chunk{agentReply("done")}, nil)
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{tools}', $2::jsonb) WHERE id = $1`,
				h.sessionID.String(), tc.tools); err != nil {
				t.Fatalf("tools fixture: %v", err)
			}
			h.wake(t, "go")
			h.runOnce(t)

			if len(h.provider.calls) != 1 {
				t.Fatalf("%d model calls, want 1", len(h.provider.calls))
			}
			req := h.provider.calls[0]
			var got []string
			for name, builtin := range req.BuiltinTools {
				if builtin {
					got = append(got, name)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("BuiltinTools = %v, want %v", got, tc.want)
			}
			if _, offered := toolsByName(t, req.Tools)["web_search"]; !offered {
				t.Error("no web_search was offered at all, so the case proves nothing")
			}
		})
	}
}
