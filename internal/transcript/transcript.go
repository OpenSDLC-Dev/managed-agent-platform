// Package transcript renders a session's event log as text a model reads. Two
// renderers live here over the same log: Render, the outcome grader's
// role-labelled plain text (plan 21 slice 3), and RenderDream, the streamed,
// redacted, capped markdown a dream's stages read (plan 41 §3.2). It sits
// outside internal/brain because both the brain and the controlplane's dream
// runner import it, and it depends on internal/domain and internal/events
// alone — nothing here may reach for a provider, a store or an HTTP layer.
package transcript

import (
	"encoding/json"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

const (
	// GraderTranscriptBudget caps the rendered transcript handed to the
	// grader; the head is kept and a truncation note marks the cut (ours).
	GraderTranscriptBudget = 200_000
	// GraderItemBudget caps any single transcript item (a tool result can be
	// 100 KiB on its own; the grader needs the shape, not every byte).
	GraderItemBudget = 4_000
)

// Render renders the session's conversation-bearing events as the
// role-labeled plain text the grader reads (shape ours, INFERRED). Long items
// truncate at GraderItemBudget; the whole transcript at GraderTranscriptBudget.
//
// The agent-to-agent message pair (plan 35) is deliberately not rendered. The
// grader reads the whole session rather than one thread, so a child's report is
// already here as that child's own submit_result call, and rendering the
// coordinator's copy of it would put the same text in twice. RenderDream keeps
// the received half, because it reads the session view alone (§3.2).
func Render(history []domain.Event) string {
	var sb strings.Builder
	add := func(role, text string) {
		if sb.Len() >= GraderTranscriptBudget {
			return
		}
		if len(text) > GraderItemBudget {
			text = text[:GraderItemBudget] + "\n[truncated]"
		}
		sb.WriteString("## " + role + "\n" + text + "\n\n")
	}
	for _, ev := range history {
		switch ev.Type {
		case domain.EventUserMessage:
			add("user", ContentText(ev.Body))
		case domain.EventSystemMessage:
			add("system", ContentText(ev.Body))
		case domain.EventAgentMessage:
			add("agent", ContentText(ev.Body))
		case domain.EventAgentToolUse, domain.EventAgentMCPToolUse, domain.EventAgentCustomToolUse:
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(ev.Body, &p) == nil {
				add("agent tool call", p.Name+" "+string(p.Input))
			}
		case domain.EventUserToolResult, domain.EventUserCustomToolRes,
			domain.EventAgentToolResult, domain.EventAgentMCPToolResult:
			add("tool result", ContentText(ev.Body))
		}
	}
	out := sb.String()
	if len(out) > GraderTranscriptBudget {
		out = out[:GraderTranscriptBudget] + "\n[transcript truncated]"
	}
	return out
}

// ContentText flattens an event body's content — a string or a block array —
// into plain text for the transcript.
func ContentText(body []byte) string {
	var p struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &p) != nil || len(p.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Content, &s) == nil {
		return s
	}
	if text, ok := FlattenBlocks(p.Content); ok {
		return text
	}
	return string(p.Content)
}

// FlattenBlocks renders a content-block array as plain text. Most blocks
// carry their text at the top level; a search_result block carries its
// evidence as title + source + nested text blocks, so those flatten too —
// web_search answers would otherwise vanish from the transcript.
func FlattenBlocks(raw json.RawMessage) (string, bool) {
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Title   string          `json:"title"`
		Source  string          `json:"source"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	add := func(text string) {
		if text == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(text)
	}
	for _, b := range blocks {
		if b.Type == "search_result" {
			var parts []string
			if b.Title != "" {
				parts = append(parts, b.Title)
			}
			if b.Source != "" {
				parts = append(parts, b.Source)
			}
			if nested, ok := FlattenBlocks(b.Content); ok && nested != "" {
				parts = append(parts, nested)
			}
			add(strings.Join(parts, "\n"))
			continue
		}
		add(b.Text)
	}
	return sb.String(), true
}
