package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// Plan 65 slice 2's brain rows: a session that mounts a repository holds its
// first turn until the executor has scanned the checkout for skills, then
// announces them in the system prompt from the first request.

const (
	repoSkillsResources = `[{"id":"sesrsc_a","type":"github_repository",` +
		`"url":"https://github.com/example-org/widget","mount_path":"/workspace/widget","checkout":null,` +
		`"created_at":"2026-08-07T00:00:00Z","updated_at":"2026-08-07T00:00:00Z"}]`

	repoSkillsRecord = `[{"name":"alpha","description":"Alpha marker.",` +
		`"location":"/workspace/widget/.claude/skills/alpha/SKILL.md"},` +
		`{"name":"nodesc","description":"","location":"/workspace/widget/.claude/skills/nodesc/SKILL.md"}]`
)

// repoSkillsAgent is a resolved agent snapshot whose toolset is the given
// agent_toolset entry.
func repoSkillsAgent(toolset string) string {
	return `{"type":"agent","id":"agent_fixture","version":1,"name":"fixture",` +
		`"model":{"id":"fixture-model"},"system":"base prompt","description":"",` +
		`"tools":[` + toolset + `],"mcp_servers":[],"skills":[],"multiagent":null}`
}

// eventCount is the number of events the session's log holds.
func (h *harness) eventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE session_id = $1`, h.sessionID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedRepoSkillsSession(t *testing.T, h *harness, agent string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = $1, resolved_agent = $2 WHERE id = $3`,
		repoSkillsResources, agent, h.sessionID.String()); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
}

// TestRepoSkillsHoldTheFirstTurn: the first turn of a cloud session mounting a
// repository is held, not sent, and hands the session to the executor. Once
// the scan is recorded and the executor wakes the turn, the first request the
// model sees already lists the repository's skills.
func TestRepoSkillsHoldTheFirstTurn(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	seedRepoSkillsSession(t, h, repoSkillsAgent(`{"type":"agent_toolset_20260401"}`))
	ctx := context.Background()

	h.wake(t, "hi")
	before := h.eventCount(t)
	h.runOnce(t)
	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("the model was called %d times before the scan", n)
	}
	// Held above the span start: the turn never began, so nothing was written,
	// no span.model_request_start among it.
	if n := h.eventCount(t); n != before {
		t.Errorf("the held turn appended %d events, want none", n-before)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec items = %d, want 1: the scan is the executor's", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model turns = %d, want 0 while the turn is held", n)
	}

	// The executor's run: the record lands and the turn is woken.
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET repo_skills = $1 WHERE id = $2`,
		repoSkillsRecord, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.queue.Enqueue(ctx, h.pool, h.envID, h.sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(h.provider.calls))
	}
	sys := h.provider.calls[0].System
	want := "<available_skills source=\"repo\">\n" +
		"<skill>\n  <name>alpha</name>\n  <description>Alpha marker.</description>\n" +
		"  <location>/workspace/widget/.claude/skills/alpha/SKILL.md</location>\n</skill>\n" +
		"<skill>\n  <name>nodesc</name>\n  <description></description>\n" +
		"  <location>/workspace/widget/.claude/skills/nodesc/SKILL.md</location>\n</skill>\n" +
		"</available_skills>"
	if !strings.Contains(sys, want) {
		t.Errorf("system prompt lacks the repository skills:\n%s", sys)
	}
	if strings.Contains(sys, "Skills are available under") {
		t.Errorf("with no attached skill the root sentence is not rendered:\n%s", sys)
	}
}

// TestRepoSkillsHeldOnce: a turn woken with no scan recorded, as an executor
// that predates the scan wakes it, is not held again: it goes ahead without
// the list, and the list joins the prompt once a run records it.
func TestRepoSkillsHeldOnce(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{
		{textChunk(0, "ok"), done("end_turn", 1)},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	seedRepoSkillsSession(t, h, repoSkillsAgent(`{"type":"agent_toolset_20260401"}`))
	ctx := context.Background()

	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) != 0 {
		t.Fatal("the first turn was not held")
	}
	// An old executor's run: no record, but the turn is woken.
	if _, err := h.queue.Enqueue(ctx, h.pool, h.envID, h.sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want 1: a session is held once", len(h.provider.calls))
	}
	if sys := h.provider.calls[0].System; strings.Contains(sys, "available_skills") {
		t.Errorf("a list was rendered with no scan recorded:\n%s", sys)
	}

	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET repo_skills = $1 WHERE id = $2`,
		repoSkillsRecord, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "again")
	h.runOnce(t)
	if len(h.provider.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(h.provider.calls))
	}
	if sys := h.provider.calls[1].System; !strings.Contains(sys, `<available_skills source="repo">`) {
		t.Errorf("the recorded list did not join the prompt:\n%s", sys)
	}
}

// TestRepoSkillsNeedRead: an agent whose read tool is off is told of no
// repository skill, as the reference announces none then, so its turn is not
// held for a scan that has not happened, nor told of one that has.
func TestRepoSkillsNeedRead(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{
		{textChunk(0, "ok"), done("end_turn", 1)},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	seedRepoSkillsSession(t, h, repoSkillsAgent(
		`{"type":"agent_toolset_20260401","configs":[{"name":"read","enabled":false}]}`))

	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want 1: a turn without read is not held", len(h.provider.calls))
	}

	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET repo_skills = $1 WHERE id = $2`,
		repoSkillsRecord, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "again")
	h.runOnce(t)
	if len(h.provider.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(h.provider.calls))
	}
	if sys := h.provider.calls[1].System; strings.Contains(sys, "available_skills") {
		t.Errorf("an agent without read was told of repository skills:\n%s", sys)
	}
}

// TestRepoSkillsNotOnSelfHosted: nothing clones on self_hosted, so a session
// stored there with a repository before create refused one (#322) is not
// held for a scan that would never come.
func TestRepoSkillsNotOnSelfHosted(t *testing.T) {
	h := newHarnessEnv(t, "self_hosted", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	seedRepoSkillsSession(t, h, repoSkillsAgent(`{"type":"agent_toolset_20260401"}`))

	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want 1: a self-hosted turn is not held", len(h.provider.calls))
	}
}

// TestRepoSkillsNeedARepository: a cloud session that mounts no repository is
// not held for a scan, whatever its agent's tools.
func TestRepoSkillsNeedARepository(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET resolved_agent = $1 WHERE id = $2`,
		repoSkillsAgent(`{"type":"agent_toolset_20260401"}`), h.sessionID.String()); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want 1: a session without a repository is not held", len(h.provider.calls))
	}
}
