package brain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// TestNestedSkillsAnnouncedAfterTheirRead is plan 65 decision 5's brain half:
// what a read found below a repository's root is told to the thread that read
// it, in the user turn that carries the read's result and after it, rendered
// from Postgres on every request; a child thread's read is not told to the
// primary.
func TestNestedSkillsAnnouncedAfterTheirRead(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{
		{textChunk(0, "ok"), done("end_turn", 1)},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	ctx := context.Background()

	uses, err := h.log.Append(ctx, h.sessionID, []events.NewEvent{{Type: domain.EventAgentToolUse,
		Payload: json.RawMessage(`{"name":"read","input":{"file_path":"/workspace/widget/x/probe.txt"}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	read := domain.NewID(domain.PrefixEvent)
	child := domain.NewID(domain.PrefixEvent)
	if _, err := h.log.Append(ctx, h.sessionID, []events.NewEvent{
		{ID: read, Type: domain.EventAgentToolResult,
			Payload: json.RawMessage(`{"tool_use_id":"` + uses[0].ID.String() + `","content":[{"type":"text","text":"probe"}]}`)},
		{ID: child, Type: domain.EventAgentToolResult, ThreadID: "sthr_child",
			Payload: json.RawMessage(`{"tool_use_id":"sevt_childuse","content":[{"type":"text","text":"elsewhere"}]}`)},
	}); err != nil {
		t.Fatal(err)
	}
	for id, list := range map[domain.ID]string{
		read:  `[{"name":"in&ner","description":"Inner & marker.","location":"/workspace/widget/x/.claude/skills/in&ner/SKILL.md"}]`,
		child: `[{"name":"ghost","description":"","location":"/workspace/widget/y/.claude/skills/ghost/SKILL.md"}]`,
	} {
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO session_nested_skills (result_event_id, session_id, skills) VALUES ($1, $2, $3)`,
			id.String(), h.sessionID.String(), list); err != nil {
			t.Fatal(err)
		}
	}

	want := "Reading that file found skills in the directories above it. " +
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
		req := h.provider.calls[turn]
		var found bool
		for _, m := range req.Messages {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for i, b := range blocks {
				if b["type"] != "tool_result" || b["tool_use_id"] != uses[0].ID.String() {
					continue
				}
				found = true
				if m.Role != "user" || i+1 >= len(blocks) || blocks[len(blocks)-2]["type"] != "text" ||
					blocks[len(blocks)-2]["text"] != want {
					t.Errorf("request %d: the read's turn = %s\nwant its result, then the nested skills, then the message", turn, m.Content)
				}
			}
			if strings.Contains(string(m.Content), "ghost") {
				t.Errorf("request %d: the primary was told of a child thread's nested skill: %s", turn, m.Content)
			}
		}
		if !found {
			t.Fatalf("request %d carries no result for the read", turn)
		}
	}
}
