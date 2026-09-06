package transcript

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The promoted grader renderer — Render, ContentText, FlattenBlocks — keeps
// its own proof where it has always been, in internal/brain's grader tests:
// the promotion changed no byte of it, and a second copy of those tests here
// would only be a second thing to keep in step. What this file pins is the
// dream renderer's markdown layout, which is new and which nothing else
// describes.

// update rewrites the fixtures after a deliberate layout change:
// `go test ./internal/transcript/ -update`, then read the diff.
var update = flag.Bool("update", false, "rewrite the golden files")

// checkGolden compares a rendered transcript against its fixture. The runner
// never parses this text — the model reads it — so what a golden file buys is
// review: a layout change shows up as a diff a human reads, not as a silently
// different prompt.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// The role labels and the drop list: three roles, a tool call and results of
// three kinds, thinking blocks and images handled per §3.2, and every
// session.*, span.*, user.interrupt and confirmation event gone.
func TestRenderDreamLayoutGolden(t *testing.T) {
	evs := []domain.Event{
		mustEvent(t, 1, domain.EventSessionStatusRunning, map[string]any{}),
		mustEvent(t, 2, domain.EventUserMessage, content("the lease test flakes in CI; find out why")),
		mustEvent(t, 3, domain.EventAgentThinking, content("where would a timing budget be set")),
		mustEvent(t, 4, domain.EventAgentMessage, map[string]any{"content": []map[string]any{
			{"type": "thinking", "thinking": "the TTL is probably the budget"},
			{"type": "text", "text": "Reading the test first."},
		}}),
		mustEvent(t, 5, domain.EventAgentToolUse, map[string]any{
			"name": "bash", "input": map[string]any{"command": "go test -run TestLease ./internal/queue/"}}),
		mustEvent(t, 6, domain.EventAgentToolResult,
			content("--- FAIL: TestLease (0.31s)\n    lease_test.go:44: renewal missed")),
		mustEvent(t, 7, domain.EventSpanOutcomeEvalEnd, map[string]any{"result": "needs_revision"}),
		mustEvent(t, 8, domain.EventUserInterrupt, map[string]any{}),
		mustEvent(t, 9, domain.EventUserToolConfirm, map[string]any{"decision": "allow"}),
		mustEvent(t, 10, domain.EventSystemMessage, content("the sandbox was restarted")),
		mustEvent(t, 11, domain.EventAgentMCPToolUse, map[string]any{
			"name": "search_docs", "input": map[string]any{"query": "lease renewal"}}),
		mustEvent(t, 12, domain.EventAgentMCPToolResult, map[string]any{"content": []map[string]any{
			{"type": "search_result", "title": "Lease renewal", "source": "docs://queue/lease",
				"content": []map[string]any{{"type": "text", "text": "a keeper renews at half the TTL"}}},
		}}),
		mustEvent(t, 13, domain.EventUserCustomToolRes, map[string]any{"content": []map[string]any{
			{"type": "image"}, {"type": "text", "text": "the CI timing chart"},
		}}),
		mustEvent(t, 14, domain.EventSessionStatusIdle, map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}),
		mustEvent(t, 15, domain.EventAgentMessage, content("The TTL budget is too tight for CI.")),
	}
	d, err := RenderDream(t.Context(), &fakeLog{evs: evs}, "sesn_layout")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	checkGolden(t, "layout.md", d.Text)
	if d.Turns != 1 || d.ElidedBytes != 0 {
		t.Errorf("Turns = %d, ElidedBytes = %d", d.Turns, d.ElidedBytes)
	}
	if d.FirstUser != "the lease test flakes in CI; find out why" {
		t.Errorf("FirstUser = %q", d.FirstUser)
	}
}

// A delegated session in the session view: the coordinator's send is dropped
// and the child's report — the only carrier of its result in this scope —
// survives with the sender named (§3.2).
func TestRenderDreamDelegatedGolden(t *testing.T) {
	evs := []domain.Event{
		mustEvent(t, 1, domain.EventUserMessage, content("research the flake and report back")),
		mustEvent(t, 2, domain.EventAgentToolUse, map[string]any{
			"name": "spawn_agent", "input": map[string]any{"agent_name": "researcher", "prompt": "investigate the lease test"}}),
		mustEvent(t, 3, domain.EventAgentThreadMessageSent, map[string]any{
			"content":              []map[string]any{{"type": "text", "text": "investigate the lease test"}},
			"to_session_thread_id": "sthr_child", "to_agent_name": "researcher"}),
		mustEvent(t, 4, domain.EventAgentThreadMessageReceived, map[string]any{
			"content":                []map[string]any{{"type": "text", "text": "the renewal budget is ~300ms; CI misses it under load"}},
			"from_session_thread_id": "sthr_child", "from_agent_name": "researcher"}),
		mustEvent(t, 5, domain.EventAgentMessage, content("Agreed — raising the budget.")),
	}
	d, err := RenderDream(t.Context(), &fakeLog{evs: evs}, "sesn_deleg")
	if err != nil {
		t.Fatalf("RenderDream: %v", err)
	}
	checkGolden(t, "delegated.md", d.Text)
}
