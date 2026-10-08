// Package convert translates Anthropic Messages to OpenAI Chat Completions
// (docs/plan/59_model-gateway.md, "Two request paths"). Two callers share it:
// the model gateway's conversion path, which serves a Messages request from
// a Chat Completions upstream and answers in Messages, and the brain's
// provider/openai, which sends its Anthropic-native turns the same way and
// reads the answer into its own chunks. Messages is the pivot: a request is
// converted to Chat Completions, an answer back to Messages, and nothing
// converts where the protocols match. Every field is read by its exact key,
// as the gateway reads each field it acts on; a struct would take "Type"
// for "type". The typed references are anthropic-sdk-go and openai-go at
// the go.mod pins (docs/REFERENCE_PROJECTS.md).
//
// What has no counterpart is dropped where its absence changes nothing the
// caller can rely on, and refused otherwise; never silently lost:
//   - a signed thinking block and a redacted_thinking block are another
//     protocol's, and dropped; an unsigned thinking block is reasoning a Chat
//     Completions upstream produced (Answer), and goes back to it as the
//     assistant message's reasoning_content, which DeepSeek requires in a
//     tool loop — every Anthropic upstream signs its blocks;
//   - a tool_result's is_error is dropped, as a tool message has no such
//     field; its content, which says what failed, goes on;
//   - an image in a user turn becomes an image_url part, a base64 source as
//     a data URL; anywhere else, and every other block — a document, a
//     search_result outside a tool_result, a server tool's — is refused.
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// Message is one Anthropic turn: its role, and its content, a string or an
// array of blocks.
type Message struct {
	Role    string
	Content json.RawMessage
}

// ChatMessage is one Chat Completions message. Content is a string, or for
// a user turn holding an image an array of parts, and absent from an
// assistant message that only calls tools.
type ChatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
}

// ToolCall is an assistant message's call of a function tool.
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function is a tool call's function: its name, and its arguments as a JSON
// string, which is how Chat Completions carries them.
type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Messages converts a system prompt and a conversation. A user turn's
// tool_result blocks become one tool message each, ahead of the turn's other
// content, which they answer the previous assistant turn before; an
// assistant turn's tool_use blocks become its tool_calls. Text blocks join
// with nothing between them, as a turn's blocks read in sequence.
func Messages(system string, msgs []Message) ([]ChatMessage, error) {
	var out []ChatMessage
	if system != "" {
		out = append(out, ChatMessage{Role: "system", Content: encode(system)})
	}
	for i, m := range msgs {
		converted, err := message(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].%w", i, err)
		}
		out = append(out, converted...)
	}
	return out, nil
}

// message converts one turn into the messages it becomes.
func message(m Message) ([]ChatMessage, error) {
	raw := bytes.TrimSpace(m.Content)
	if len(raw) == 0 {
		return nil, fmt.Errorf("content: empty")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("content: %w", err)
		}
		return []ChatMessage{{Role: m.Role, Content: encode(s)}}, nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	var (
		results   []ChatMessage
		parts     []part
		reasoning []string
		calls     []ToolCall
		images    bool
	)
	for j, b := range blocks {
		typ, _ := text(b, "type")
		switch typ {
		case "text":
			t, ok := text(b, "text")
			if !ok {
				return nil, fmt.Errorf("content[%d].text: must be a string", j)
			}
			parts = append(parts, part{Type: "text", Text: t})
		case "image":
			if m.Role != "user" {
				return nil, fmt.Errorf("content[%d]: an image in a %s turn has no Chat Completions counterpart", j, m.Role)
			}
			url, err := imageURL(b["source"])
			if err != nil {
				return nil, fmt.Errorf("content[%d].source: %w", j, err)
			}
			parts = append(parts, part{Type: "image_url", ImageURL: &imageRef{URL: url}})
			images = true
		case "tool_use":
			id, _ := text(b, "id")
			name, _ := text(b, "name")
			args, err := arguments(b["input"])
			if err != nil {
				return nil, fmt.Errorf("content[%d].input: %w", j, err)
			}
			calls = append(calls, ToolCall{ID: id, Type: "function", Function: Function{Name: name, Arguments: args}})
		case "tool_result":
			id, _ := text(b, "tool_use_id")
			content, err := resultText(b["content"])
			if err != nil {
				return nil, fmt.Errorf("content[%d].content: %w", j, err)
			}
			results = append(results, ChatMessage{Role: "tool", ToolCallID: id, Content: encode(content)})
		case "thinking":
			if sig, _ := text(b, "signature"); sig == "" {
				t, _ := text(b, "thinking")
				reasoning = append(reasoning, t)
			}
		case "redacted_thinking":
		default:
			return nil, fmt.Errorf("content[%d]: a %q block has no Chat Completions counterpart", j, typ)
		}
	}
	out := results
	if len(parts) == 0 && len(calls) == 0 {
		return out, nil
	}
	msg := ChatMessage{Role: m.Role, ToolCalls: calls, ReasoningContent: strings.Join(reasoning, "")}
	switch {
	case images:
		msg.Content = encode(parts)
	case len(parts) > 0:
		texts := make([]string, len(parts))
		for k, p := range parts {
			texts[k] = p.Text
		}
		msg.Content = encode(strings.Join(texts, ""))
	}
	return append(out, msg), nil
}

// part is one part of a user message's content.
type part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageRef `json:"image_url,omitempty"`
}

type imageRef struct {
	URL string `json:"url"`
}

// imageURL is an image block's source as Chat Completions takes it: a URL,
// or base64 data as a data URL.
func imageURL(raw json.RawMessage) (string, error) {
	var src map[string]json.RawMessage
	if json.Unmarshal(raw, &src) != nil {
		return "", fmt.Errorf("must be an object")
	}
	switch typ, _ := text(src, "type"); typ {
	case "base64":
		media, _ := text(src, "media_type")
		data, ok := text(src, "data")
		if media == "" || !ok {
			return "", fmt.Errorf("a base64 source needs media_type and data")
		}
		return "data:" + media + ";base64," + data, nil
	case "url":
		url, ok := text(src, "url")
		if !ok || url == "" {
			return "", fmt.Errorf("a url source needs url")
		}
		return url, nil
	default:
		return "", fmt.Errorf("a %q source has no Chat Completions counterpart", typ)
	}
}

// arguments is a tool_use block's input as a tool call carries it: compact
// JSON, and an object for an input left out.
func arguments(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// resultText flattens a tool_result's content, a string or an array of
// blocks, into the text a tool message carries. A search_result renders
// through provider.SearchResultText, which provider/anthropic's
// flatten_search_results and the gateway's profiles share; any other block,
// an image among them, has no place in a tool message and is refused.
func resultText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var out []string
	for j, b := range blocks {
		switch typ, _ := text(b, "type"); typ {
		case "text":
			t, ok := text(b, "text")
			if !ok {
				return "", fmt.Errorf("[%d].text: must be a string", j)
			}
			out = append(out, t)
		case "search_result":
			title, _ := text(b, "title")
			flat, err := provider.SearchResultText(title, b["source"], b["content"])
			if err != nil {
				return "", err
			}
			// The flat form is line-shaped; joined bare onto a preceding text
			// block it would glue onto that block's last line.
			if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
				out = append(out, "\n")
			}
			out = append(out, flat)
		default:
			return "", fmt.Errorf("unsupported tool_result content block %q (OpenAI tool messages are text-only)", typ)
		}
	}
	return strings.Join(out, ""), nil
}

// text is obj[key] when it is a JSON string; null and anything else is
// absent.
func text(obj map[string]json.RawMessage, key string) (string, bool) {
	var s string
	v := obj[key]
	if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// encode is v as JSON, with no HTML escapes, as every wire-bound encode in
// the gateway is.
func encode(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
