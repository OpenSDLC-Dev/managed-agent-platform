package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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

// hasSandbox makes this endpoint hold a running sandbox for the session — what
// an earlier tool_exec would have left behind, and the only state in which an
// MCP answer spills.
func (h *harness) hasSandbox() {
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	if h.prov.running == nil {
		h.prov.running = map[domain.ID]bool{}
	}
	h.prov.running[h.sid] = true
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
// ran a built-in tool. Attach is the seam that makes that structural rather than
// conditional: it cannot create one.
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
		t.Errorf("sandbox provisions = %d, want 0 — a spill never provisions", n)
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

// An answer that arrived whole does not go looking for a sandbox at all: the
// lookup is a round trip to the endpoint, and paying for one per answer would
// charge every MCP call for a file almost none of them need.
func TestMCPAnswerWithinTheBudgetLooksForNoSandbox(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "small"})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.attachCount(); n != 0 {
		t.Errorf("sandbox lookups = %d, want 0 — the answer arrived whole", n)
	}
	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0", n)
	}
}

// An answer that overruns the budget and still arrives whole spills nothing.
// capMCPBlocks exempts one already-capped block, so two blocks of two thirds the
// budget each are both delivered — and a spill triggered on the answer's size
// would write a file the model already has and tell it to go and read it, buying
// a wasted tool call and a second copy in its context.
func TestMCPOversizedAnswerThatArrivedWholeSpillsNothing(t *testing.T) {
	two := 2 * toolset.MaxOutputBytes / 3
	url := mcptest.Server(t, mcptest.Tool{Name: "report", Blocks: []mcptest.Block{
		{Type: "text", Text: strings.Repeat("a", two)},
		{Type: "text", Text: strings.Repeat("b", two)},
	}})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "report", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	text := blockText(t, results[0])
	if len(text) < 2*two {
		t.Fatalf("answer is %d bytes, want both blocks whole — the fixture no longer tests this", len(text))
	}
	if n := h.prov.attachCount(); n != 0 {
		t.Errorf("sandbox lookups = %d, want 0 — the answer lost nothing", n)
	}
	if _, ok := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]; ok {
		t.Errorf("a file was written for an answer the model already has whole")
	}
	if strings.Contains(text, "/tmp/tool_outputs/") {
		t.Errorf("the model was told to read a file it did not need: %.200q", text)
	}
}

// The file holds the whole answer, not its first block: a server may split a
// long document across several, and a spill that kept only the first would hand
// the model a file that looks complete and is not. Order is the server's, since
// that is the order the answer would have read in — and each resource body is
// named by the URI its inline document block is titled with, so a model reading
// three files back can still tell which bytes came from which.
func TestMCPSpillHoldsEveryBlockInOrderAndNamesEachResource(t *testing.T) {
	// The leading block overruns on its own, so it arrives truncated and
	// everything behind it is dropped — an answer that lost all three of these,
	// which is what makes the file's contents the assertion.
	head := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	tail := strings.Repeat("b", toolset.MaxOutputBytes/2+2_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "report", Blocks: []mcptest.Block{
		{Type: "text", Text: head},
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
	want := head + "\nThe tool returned the resource file:///notes.txt (text/plain):\nthe middle\n" + tail
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

	if n := h.prov.attachCount(); n != 1 {
		t.Errorf("sandbox lookups = %d, want 1 for the whole pass — the handle is reusable", n)
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

	if n := h.prov.attachCount(); n != 0 {
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

// An endpoint that cannot be reached at all is not a new way for a call to fail
// either: the answer truncates, and nothing is provisioned on the strength of
// not knowing. It *is* asked again — the lookup is one cheap read, and a daemon
// that blinked between two calls of a pass should not cost the second one its
// file.
func TestMCPSpillWhenTheSandboxLookupFailsFallsBack(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: huge})
	h := mcpHarness(t)
	h.hasSandbox()
	h.prov.attachErr = errors.New("daemon unreachable")
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.appendMCPToolUse(t, "docs", "search", `{"n":2}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.prov.attachCount(); n != 2 {
		t.Errorf("sandbox lookups = %d, want one per call — a blink is not latched", n)
	}
	if n := h.prov.provisions; n != 0 {
		t.Errorf("sandbox provisions = %d, want 0 — a spill never provisions", n)
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

// A server cannot spend the whole answer budget on a label. A resource link
// carries no content — only an address and a media type — so the sentence
// describing it caps both where every other server-chosen label is capped, and a
// megabyte of either arrives truncated rather than as an exempt block that eats
// the budget and takes the rest of the answer with it. Nothing is over budget,
// so nothing spills: there was never an answer lost here to spill.
func TestMCPGiantLinkLabelsAreCutRatherThanEatingTheAnswer(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "list", Blocks: []mcptest.Block{
		{Type: "resource_link", URI: "file:///" + strings.Repeat("u", 2*toolset.MaxOutputBytes),
			MIMEType: "text/" + strings.Repeat("m", 2*toolset.MaxOutputBytes)},
		{Type: "text", Text: "the report the link was standing in front of"},
	}})
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
	text := blockText(t, results[0])
	if !strings.Contains(text, "the report the link was standing in front of") {
		t.Errorf("the report behind the link was lost: %.300q", text)
	}
	if strings.Contains(text, "content block(s) of this answer were dropped") {
		t.Errorf("a capped label still cost the answer its blocks: %.300q", text)
	}
	if len(text) > 6*maxResourceLabel {
		t.Errorf("answer is %d bytes, want both of the link's labels cut", len(text))
	}
	if _, ok := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]; ok {
		t.Errorf("a file was written for an answer that lost nothing")
	}
}

// An answer of nothing but NUL bytes is over the budget in what the server sent
// and empty in what a file can hold — jsonb cannot store a NUL, so the driver
// strips them. Writing the empty file anyway would put a notice promising the
// answer's text over a file holding none of it.
func TestMCPNULOnlyAnswerWritesNoEmptyFile(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{
		Name: "search", Result: strings.Repeat("\x00", toolset.MaxOutputBytes+5_000),
	})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got, ok := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]; ok {
		t.Errorf("spill file exists holding %d bytes, want none written", len(got))
	}
	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if text := blockText(t, results[0]); strings.Contains(text, "/tmp/tool_outputs/") {
		t.Errorf("the model was pointed at an empty file: %.200q", text)
	}
}

// A single resource whose body overruns the budget loses nothing to the
// dropped-block rule — it is the one block capMCPBlocks exempts — and everything
// to the cap on the way into its document source. So the loss has to be reported
// from where the capping happens; nothing downstream can see it.
func TestMCPTruncatedResourceBodySpills(t *testing.T) {
	body := strings.Repeat("r", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", URI: "file:///big.txt", MIMEType: "text/plain", Text: body},
	}})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if text := blockText(t, results[0]); strings.Contains(text, "content block(s) of this answer were dropped") {
		t.Fatalf("this answer was expected to be truncated rather than dropped: %.200q", text)
	}
	path := "/tmp/tool_outputs/" + useID + ".txt"
	want := "The tool returned the resource file:///big.txt (text/plain):\n" + body
	if got := h.prov.sb.files[path]; got != want {
		t.Errorf("spill file holds %d bytes, want the resource's whole %d-byte body", len(got), len(want))
	}
	if got := lastBlockText(t, results[0]); got != "[the full text of this answer was written to "+path+"]" {
		t.Errorf("last block = %q, want the spill notice naming %s", got, path)
	}
}

// MCP lets one embedded resource carry both a text body and a blob, and the
// inline rendering reads the blob: a document the model can open beats an
// extracted text it cannot check. The spill file has to read it the same way, or
// the file and the answer describe one block differently and a model reconciling
// them is told two things.
func TestMCPSpillDescribesAResourceTheWayTheAnswerDid(t *testing.T) {
	huge := strings.Repeat("a", toolset.MaxOutputBytes+5_000)
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "text", Text: huge},
		{Type: "resource", URI: "file:///both.bin", MIMEType: "application/octet-stream",
			Text: "an extraction of the bytes", Data: []byte("the bytes themselves")},
	}})
	h := mcpHarness(t)
	h.hasSandbox()
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.prov.sb.files["/tmp/tool_outputs/"+useID+".txt"]
	want := "The tool returned 20 bytes of application/octet-stream at file:///both.bin, " +
		"which is not in this file."
	if !strings.Contains(got, want) {
		t.Errorf("spill file describes the resource as %.200q, want it read as the answer read it: %q",
			got[len(got)-min(len(got), 200):], want)
	}
	if strings.Contains(got, "an extraction of the bytes") {
		t.Errorf("spill file holds the resource's text where the answer carried its bytes")
	}
}
