package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// blockText concatenates the text of every text block of one result, which is
// where both the truncated answer and the spill notice land.
func blockText(t *testing.T, res map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, blk := range blocksOf(t, res) {
		if blk["type"] == "text" {
			b.WriteString(blk["text"].(string))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// An answer past the tool-result budget is not simply lost: the whole of it
// lands in the session's sandbox and the model is told where, the same bargain
// a built-in tool's oversized output already gets — and under the same
// convention, so a model that has learned where its truncated output goes is
// right whichever tool produced it.
func TestMCPOversizedAnswerSpillsToTheSandbox(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	path := "/tmp/tool_outputs/" + useID + ".txt"
	if got := h.prov.sb.files[path]; got != huge {
		t.Errorf("spill file holds %d bytes, want the full %d", len(got), len(huge))
	}
	text := blockText(t, results[0])
	if !strings.Contains(text, path) {
		t.Errorf("the model was not told where its output went: %.200q", text)
	}
	if len(text) > 2*toolset.MaxOutputBytes {
		t.Errorf("answer text is %d bytes, want it truncated to the budget", len(text))
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want a spilled answer to stay an ordinary answer")
	}
}

// The sandbox is provisioned only for an answer that needs it. An MCP session
// need never have one — the driver runs server-side on every environment kind —
// so provisioning per call would put a container behind every MCP session on the
// platform.
func TestMCPAnswerWithinTheBudgetProvisionsNoSandbox(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "small"})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0 — nothing overran the budget", n)
	}
}

// The file holds the whole answer, not its first block: a server may split a
// long document across several, and a spill that kept only the first would hand
// the model a file that looks complete and is not. Order is the server's, since
// that is the order the answer would have read in.
func TestMCPSpillHoldsEveryTextBlockInOrder(t *testing.T) {
	half := strings.Repeat("a", toolset.MaxOutputBytes/2+2_000)
	tail := strings.Repeat("b", toolset.MaxOutputBytes/2+2_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "report", Blocks: []mcptest.Block{
		{Type: "text", Text: half},
		{Type: "resource", URI: "file:///notes.txt", MIMEType: "text/plain", Text: "the middle"},
		{Type: "text", Text: tail},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "report", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	path := "/tmp/tool_outputs/" + useID + ".txt"
	want := half + "\nthe middle\n" + tail
	if got := h.prov.sb.files[path]; got != want {
		t.Errorf("spill file holds %d bytes, want the %d of every block joined in order",
			len(got), len(want))
	}
}

// A sandbox that cannot be provisioned is not a new way for a call to fail — the
// same bargain a failed write makes, one step earlier — and it is not tried
// again for the rest of the pass: a provision costs an image pull and a lock,
// and a pass of twenty oversized calls would pay it twenty times to learn what
// the first call already knew.
func TestMCPSpillWithoutASandboxFallsBackToPlainTruncation(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.prov.provisionErr = errors.New("no capacity")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 1 {
		t.Errorf("sandbox provisions = %d, want 1 — a failed provision is not retried per call", n)
	}
	results := h.mcpResults(t)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both calls answered", results)
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want a missing sandbox to leave the answer alone")
	}
	text := blockText(t, results[0])
	if strings.Contains(text, "/tmp/tool_outputs/"+useID) {
		t.Errorf("the model was pointed at a file that was never written: %.200q", text)
	}
	if !strings.Contains(text, "[output truncated]") {
		t.Errorf("answer does not say it was truncated: %.200q", text)
	}
}

// Two oversized answers in one pass share one sandbox: provisioning is per pass
// and not per call, so a turn whose model made several large MCP calls does not
// pay for a container each.
func TestMCPSecondSpillOfAPassReusesTheSandbox(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	first := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	second := h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 1 {
		t.Errorf("sandbox provisions = %d, want 1 for the whole pass", n)
	}
	for _, id := range []string{first, second} {
		path := "/tmp/tool_outputs/" + id + ".txt"
		if got := h.prov.sb.files[path]; got != huge {
			t.Errorf("%s holds %d bytes, want the full %d", path, len(got), len(huge))
		}
	}
}

// A self_hosted session has no platform-side sandbox at all: the tools run in
// the customer's own, reached only through the work API, which has no MCP
// surface and no way to hand this process a file handle. The answer truncates
// exactly as it did before the spill existed.
func TestMCPSelfHostedOversizedAnswerTruncatesWithoutSpilling(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET kind = 'self_hosted', config = '{"type":"self_hosted"}'::jsonb WHERE id = $1`,
		h.envID.String()); err != nil {
		t.Fatal(err)
	}
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0 — this session has no platform sandbox", n)
	}
	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	text := blockText(t, results[0])
	if strings.Contains(text, "/tmp/tool_outputs/"+useID) {
		t.Errorf("the model was pointed at a file that was never written: %.200q", text)
	}
	if !strings.Contains(text, "[output truncated]") {
		t.Errorf("answer does not say it was truncated: %.200q", text)
	}
}

// A spill that cannot be written is not a new way for a call to fail: the answer
// truncates exactly as it would have, and the model is not pointed at a file
// that is not there.
func TestMCPSpillFailureFallsBackToPlainTruncation(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.prov.sb.writeErr = errors.New("read-only sandbox")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want a failed spill to leave the answer alone")
	}
	text := blockText(t, results[0])
	if strings.Contains(text, "/tmp/tool_outputs/"+useID) {
		t.Errorf("the model was pointed at a file that was never written: %.200q", text)
	}
	if !strings.Contains(text, "[output truncated]") {
		t.Errorf("answer does not say it was truncated: %.200q", text)
	}
}
