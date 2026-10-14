package brain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// seedNestedSkills mounts the repositories, with their root scans recorded
// so the first turn is not held, and plants two reads on the primary thread
// and one on a child thread, each answered by a result with the nested skills
// it found: one below a mounted repository, one below a repository the
// session no longer mounts, and the child's. The child's agent, like the
// session's, has the agent toolset. It answers the primary's first read's use
// and the child thread.
func seedNestedSkills(t *testing.T, h *harness, agent string) (domain.ID, domain.ID) {
	t.Helper()
	seedRepoSkillsSession(t, h, agent)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET repo_skills = '{}' WHERE id = $1`, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	child := pgtest.NewChildThreadWithAgent(t, h.pool, h.sessionID, `{"type":"agent","id":"agent_worker","version":1,"name":"worker",`+
		`"model":{"id":"fixture-model"},"system":"","description":"","tools":[{"type":"agent_toolset_20260401"}],"mcp_servers":[],"skills":[]}`)
	uses, err := h.log.Append(ctx, h.sessionID, []events.NewEvent{
		{Type: domain.EventAgentToolUse, Payload: json.RawMessage(`{"name":"read","input":{"file_path":"/workspace/widget/x/probe.txt"}}`)},
		{Type: domain.EventAgentToolUse, Payload: json.RawMessage(`{"name":"read","input":{"file_path":"/workspace/gone/x/probe.txt"}}`)},
		{Type: domain.EventAgentToolUse, ThreadID: child, CrossPosted: true,
			Payload: json.RawMessage(`{"name":"read","input":{"file_path":"/workspace/widget/y/probe.txt"}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := func(use domain.ID, thread domain.ID) events.NewEvent {
		return events.NewEvent{ID: domain.NewID(domain.PrefixEvent), Type: domain.EventAgentToolResult, ThreadID: thread, CrossPosted: thread != "",
			Payload: json.RawMessage(`{"tool_use_id":"` + use.String() + `","content":[{"type":"text","text":"probe"}]}`)}
	}
	results := []events.NewEvent{result(uses[0].ID, ""), result(uses[1].ID, ""), result(uses[2].ID, child)}
	if _, err := h.log.Append(ctx, h.sessionID, results); err != nil {
		t.Fatal(err)
	}
	for i, list := range []string{
		`[{"name":"in&ner","description":"Inner & marker.","location":"/workspace/widget/x/.claude/skills/in&ner/SKILL.md"}]`,
		`[{"name":"gonner","description":"","location":"/workspace/gone/x/.claude/skills/gonner/SKILL.md"}]`,
		`[{"name":"ghost","description":"","location":"/workspace/widget/y/.claude/skills/ghost/SKILL.md"}]`,
	} {
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO session_nested_skills (result_event_id, session_id, skills) VALUES ($1, $2, $3)`,
			results[i].ID.String(), h.sessionID.String(), list); err != nil {
			t.Fatal(err)
		}
	}
	return uses[0].ID, child
}

// TestNestedSkillsAnnouncedAfterTheirRead is plan 65 decision 5's brain half:
// what a read found below a repository's root is told to the thread that read
// it, in the user turn that carries the read's result and after it, rendered
// from Postgres on every request. A child thread's read is not told to the
// primary, and a skill of a repository the session no longer mounts is not
// told at all.
func TestNestedSkillsAnnouncedAfterTheirRead(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{
		{textChunk(0, "ok"), done("end_turn", 1)},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	use, _ := seedNestedSkills(t, h, repoSkillsAgent(`{"type":"agent_toolset_20260401"}`))

	want := "Skills were found in the directories above a file you read. " +
		"Before you act on a task one of them matches, read its SKILL.md.\n\n" +
		"<available_skills source=\"repo\">\n" +
		"<skill>\n  <name>in&amp;ner</name>\n  <description>Inner &amp; marker.</description>\n" +
		"  <location>/workspace/widget/x/.claude/skills/in&amp;ner/SKILL.md</location>\n</skill>\n" +
		"</available_skills>"
	for turn, text := range []string{"next", "again"} {
		h.wake(t, text)
		h.runOnce(t)
		if len(h.provider.calls) != turn+1 {
			t.Fatalf("model calls = %d, want %d", len(h.provider.calls), turn+1)
		}
		var found bool
		for _, m := range h.provider.calls[turn].Messages {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b["type"] == "tool_result" && b["tool_use_id"] == use.String() {
					found = true
					if m.Role != "user" || len(blocks) != 4 || blocks[2]["type"] != "text" || blocks[2]["text"] != want {
						t.Errorf("request %d: the read's turn = %s\nwant both results, then the nested skills, then the message", turn, m.Content)
					}
				}
			}
			for _, absent := range []string{"ghost", "gonner"} {
				if strings.Contains(string(m.Content), absent) {
					t.Errorf("request %d: the primary was told of %s: %s", turn, absent, m.Content)
				}
			}
		}
		if !found {
			t.Fatalf("request %d carries no result for the read", turn)
		}
	}
}

// TestNestedSkillsToldToTheThreadThatRead: a child thread's turn is told of
// what its own read found, after that read's result, and not of what the
// primary's reads found.
func TestNestedSkillsToldToTheThreadThatRead(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	_, child := seedNestedSkills(t, h, repoSkillsAgent(`{"type":"agent_toolset_20260401"}`))
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE session_threads SET status = 'running' WHERE id = $1`, child.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.Append(ctx, h.sessionID, []events.NewEvent{{Type: domain.EventUserMessage, ThreadID: child,
		Payload: json.RawMessage(`{"content":[{"type":"text","text":"go on"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.queue.EnqueueThread(ctx, h.pool, h.envID, h.sessionID, child, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	h.runOnce(t)
	if len(h.provider.calls) != 1 {
		t.Fatalf("model calls = %d, want the child's turn", len(h.provider.calls))
	}
	var told bool
	for _, m := range h.provider.calls[0].Messages {
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for i, b := range blocks {
			if b["type"] == "text" && strings.Contains(fmt.Sprint(b["text"]), "<name>ghost</name>") {
				told = i > 0 && blocks[i-1]["type"] == "tool_result"
			}
		}
		if strings.Contains(string(m.Content), "in&amp;ner") {
			t.Errorf("the child was told of the primary's nested skill: %s", m.Content)
		}
	}
	if !told {
		t.Errorf("the child's request does not tell it of its own read's skill after that read's result: %+v", h.provider.calls[0].Messages)
	}
}

// TestNestedSkillsTakeTheRepositoryListsGates: nested skills are told under
// the root list's conditions, so none to an agent whose read tool is off,
// as the reference announces none then, and none on a self-hosted session.
func TestNestedSkillsTakeTheRepositoryListsGates(t *testing.T) {
	for _, c := range []struct{ name, env, toolset string }{
		{"read off", "cloud", `{"type":"agent_toolset_20260401","configs":[{"name":"read","enabled":false}]}`},
		{"self-hosted", "self_hosted", `{"type":"agent_toolset_20260401"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessEnv(t, c.env, [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
			seedNestedSkills(t, h, repoSkillsAgent(c.toolset))
			h.wake(t, "next")
			h.runOnce(t)
			if len(h.provider.calls) != 1 {
				t.Fatalf("model calls = %d, want 1", len(h.provider.calls))
			}
			for _, m := range h.provider.calls[0].Messages {
				if strings.Contains(string(m.Content), "available_skills") {
					t.Errorf("nested skills told: %s", m.Content)
				}
			}
		})
	}
}
