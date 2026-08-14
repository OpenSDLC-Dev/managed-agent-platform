package executor

import (
	"context"
	"errors"
	"fmt"
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

// lastBlockText is the text of a result's final block, which is where a notice
// this platform adds on top of the budget belongs.
func lastBlockText(t *testing.T, res map[string]any) string {
	t.Helper()
	blocks := blocksOf(t, res)
	last := blocks[len(blocks)-1]
	if last["type"] != "text" {
		t.Fatalf("last block is %v, want text", last["type"])
	}
	return last["text"].(string)
}

// hasSandbox puts the session in the set this endpoint already holds a sandbox
// for — what an earlier tool_exec would have left behind, and the only state in
// which an MCP answer spills.
func (h *harness) hasSandbox() {
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	h.prov.owned = append(h.prov.owned, h.sid)
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
	h.hasSandbox()
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
	if got := lastBlockText(t, results[0]); got != "[the full text of this answer was written to "+path+"]" {
		t.Errorf("last block = %q, want the spill notice naming %s", got, path)
	}
	if n := len(blocksOf(t, results[0])); n != 2 {
		t.Errorf("blocks = %d, want the truncated answer plus the notice", n)
	}
	if text := blockText(t, results[0]); len(text) > 2*toolset.MaxOutputBytes {
		t.Errorf("answer text is %d bytes, want it truncated to the budget", len(text))
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want a spilled answer to stay an ordinary answer")
	}
}

// The spill uses the sandbox the session already has and never creates one. An
// MCP session need never have a sandbox — this driver is server-side on every
// environment kind — so provisioning one here would put a container behind every
// cloud session that declares an MCP server, and point its model at a file it
// has no tool to open: a session with no sandbox is a session whose agent never
// ran a built-in tool.
func TestMCPSpillOnASessionWithNoSandboxProvisionsNone(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0 — this session has no sandbox to spill into", n)
	}
	if n := h.prov.ownedLookups(); n != 1 {
		t.Errorf("sandbox lookups = %d, want 1 for the pass — the answer is the same for every call", n)
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

// An answer inside the budget does not go looking for a sandbox at all: the
// lookup is a container listing against the endpoint, and paying for one per
// answer would charge every MCP call for a file almost none of them need.
func TestMCPAnswerWithinTheBudgetLooksForNoSandbox(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "small"})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.ownedLookups(); n != 0 {
		t.Errorf("sandbox lookups = %d, want 0 — nothing overran the budget", n)
	}
	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0", n)
	}
}

// The file holds the whole answer, not its first block: a server may split a
// long document across several, and a spill that kept only the first would hand
// the model a file that looks complete and is not. Order is the server's, since
// that is the order the answer would have read in — and each resource body is
// named by the URI its inline document block is titled with, so a model reading
// three files back can still tell which bytes came from which.
func TestMCPSpillHoldsEveryBlockInOrderAndNamesEachResource(t *testing.T) {
	half := strings.Repeat("a", toolset.MaxOutputBytes/2+2_000)
	tail := strings.Repeat("b", toolset.MaxOutputBytes/2+2_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "report", Blocks: []mcptest.Block{
		{Type: "text", Text: half},
		{Type: "resource", URI: "file:///notes.txt", MIMEType: "text/plain", Text: "the middle"},
		{Type: "text", Text: tail},
	}})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "report", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	path := "/tmp/tool_outputs/" + useID + ".txt"
	want := half + "\nThe tool returned the resource file:///notes.txt (text/plain):\nthe middle\n" + tail
	if got := h.prov.sb.files[path]; got != want {
		t.Errorf("spill file holds %d bytes, want the %d of every block joined in order,\n"+
			"each resource named: %.300q", len(got), len(want), got)
	}
}

// An answer can lose blocks while its text is small: the budget charges each
// block its *marshalled* size, and a few thousand short text blocks cost far
// more in JSON than they hold in text. Spilling on the text length alone would
// let exactly that answer lose its tail with nothing written — which is the case
// the spill exists to prevent.
func TestMCPAnswerThatLosesBlocksSpillsEvenWhenItsTextIsSmall(t *testing.T) {
	const n = 5_000
	blocks := make([]mcptest.Block, n)
	var want strings.Builder
	for i := range blocks {
		text := fmt.Sprintf("line %04d", i)
		blocks[i] = mcptest.Block{Type: "text", Text: text}
		if i > 0 {
			want.WriteString("\n")
		}
		want.WriteString(text)
	}
	// Well under the budget in text, and far over it once each block is JSON.
	if got := want.Len(); got > toolset.MaxOutputBytes {
		t.Fatalf("fixture text is %d bytes, want it under the %d-byte budget", got, toolset.MaxOutputBytes)
	}
	url := mcptest.Server(t, mcptest.Tool{Name: "lines", Blocks: blocks})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "lines", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	text := blockText(t, results[0])
	if !strings.Contains(text, "content block(s) of this answer were dropped") {
		t.Fatalf("this answer was expected to lose blocks; it did not: %.200q", text)
	}
	path := "/tmp/tool_outputs/" + useID + ".txt"
	if got := h.prov.sb.files[path]; got != want.String() {
		t.Errorf("spill file holds %d bytes, want the %d the answer carried", len(got), want.Len())
	}
	if got := lastBlockText(t, results[0]); got != "[the full text of this answer was written to "+path+"]" {
		t.Errorf("last block = %q, want the spill notice naming %s", got, path)
	}
}

// A block whose content this platform describes rather than carries — a resource
// link, an audio clip — is the answer as far as the model is concerned: it reads
// the sentence and nothing else. So an answer of nothing but links spills its
// sentences, because losing them is losing the answer, and it is measured by the
// blocks the budget dropped rather than by a text length it has none of.
func TestMCPLinkOnlyAnswerSpillsTheLinksTheBudgetDropped(t *testing.T) {
	const n = 4_000
	blocks := make([]mcptest.Block, n)
	for i := range blocks {
		blocks[i] = mcptest.Block{
			Type: "resource_link", URI: fmt.Sprintf("file:///doc-%04d.md", i), MIMEType: "text/markdown",
		}
	}
	url := mcptest.Server(t, mcptest.Tool{Name: "list", Blocks: blocks})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "list", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if text := blockText(t, results[0]); !strings.Contains(text, "content block(s) of this answer were dropped") {
		t.Fatalf("this answer was expected to lose blocks; it did not: %.200q", text)
	}
	got := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]
	for _, i := range []int{0, n - 1} {
		want := fmt.Sprintf("The tool returned a link to a resource: file:///doc-%04d.md (text/markdown)", i)
		if !strings.Contains(got, want) {
			t.Errorf("spill file (%d bytes) does not hold link %d: %q", len(got), i, want)
		}
	}
}

// An answer with no text at all spills nothing. Its bytes are an image the
// budget dropped whole, and a file holding one sentence saying so would be a
// pointer to content that is not in it — worse than the truncation, which at
// least does not promise.
func TestMCPImageOnlyAnswerSpillsNothing(t *testing.T) {
	huge := strings.Repeat("\x01", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "shot", Blocks: []mcptest.Block{
		{Type: "image", MIMEType: "image/png", Data: []byte(huge)},
	}})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "shot", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got, ok := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]; ok {
		t.Errorf("spill file holds %d bytes, want no file — the answer had no text", len(got))
	}
	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if text := blockText(t, results[0]); strings.Contains(text, "/tmp/tool_outputs/") {
		t.Errorf("the model was pointed at a file that was never written: %.200q", text)
	}
}

// Two oversized answers in one pass share one sandbox lookup: the answer is the
// same for every call of the pass, and a container listing per call would charge
// a turn of large MCP calls once each for it.
func TestMCPSecondSpillOfAPassReusesTheSandbox(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	first := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	second := h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.ownedLookups(); n != 1 {
		t.Errorf("sandbox lookups = %d, want 1 for the whole pass", n)
	}
	if n := h.prov.provisions; n != 1 {
		t.Errorf("sandbox adoptions = %d, want 1 for the whole pass", n)
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
	h.hasSandbox()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET kind = 'self_hosted', config = '{"type":"self_hosted"}'::jsonb WHERE id = $1`,
		h.envID.String()); err != nil {
		t.Fatal(err)
	}
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.ownedLookups(); n != 0 {
		t.Errorf("sandbox lookups = %d, want 0 — this session has no platform sandbox", n)
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
// truncates exactly as it would have, the model is not pointed at a file that is
// not there, and the rest of the pass does not pay a round trip each to learn
// what the first write established.
func TestMCPSpillFailureFallsBackToPlainTruncation(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.hasSandbox()
	h.prov.sb.writeErr = errors.New("read-only sandbox")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.sb.writes; n != 1 {
		t.Errorf("sandbox writes = %d, want 1 — a failed write is not retried per call", n)
	}
	results := h.mcpResults(t)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both calls answered", results)
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

// A sandbox the endpoint claims to hold and then cannot hand over is the same
// bargain a failed write makes, one step earlier — and it is not tried again for
// the rest of the pass.
func TestMCPSpillWhenTheSandboxCannotBeAdoptedFallsBack(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.hasSandbox()
	h.prov.provisionErr = errors.New("no capacity")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.provisions; n != 1 {
		t.Errorf("sandbox adoptions = %d, want 1 — a failed one is not retried per call", n)
	}
	results := h.mcpResults(t)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both calls answered", results)
	}
	text := blockText(t, results[0])
	if strings.Contains(text, "/tmp/tool_outputs/"+useID) {
		t.Errorf("the model was pointed at a file that was never written: %.200q", text)
	}
	if !strings.Contains(text, "[output truncated]") {
		t.Errorf("answer does not say it was truncated: %.200q", text)
	}
}
