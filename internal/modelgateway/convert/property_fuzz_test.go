package convert

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func FuzzFunctionName(f *testing.F) {
	for _, seed := range []string{
		"",
		"bash",
		"mcp__server__tool",
		"0_starts_with_digit",
		"-starts-with-dash",
		"a_very_long_tool_name_that_exceeds_sixty_four_bytes_by_a_wide_margin_0123456789",
		"mcp__unicode_服务器__非常长的工具名称用于测试UTF8截断边界行为_0123456789_abcdef",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		fn := FunctionName(name)
		if len(fn) > maxFunctionName {
			t.Fatalf("FunctionName(%q) length = %d > %d (%q)", name, len(fn), maxFunctionName, fn)
		}
		if utf8.ValidString(name) && !utf8.ValidString(fn) {
			t.Fatalf("FunctionName(%q) broke UTF-8: %x", name, fn)
		}
		if len(name) <= maxFunctionName && fn != name {
			t.Fatalf("short name %q changed to %q", name, fn)
		}
		if again := FunctionName(name); again != fn {
			t.Fatalf("FunctionName(%q) non-deterministic: %q vs %q", name, fn, again)
		}
		// Single-tool round-trip through NamesOf must recover the original name.
		if utf8.ValidString(name) {
			toolJSON, err := json.Marshal(map[string]any{"name": name})
			if err != nil {
				t.Fatalf("marshal tool: %v", err)
			}
			names := NamesOf([]json.RawMessage{toolJSON}, nil)
			if got := names.Tool(fn); got != name {
				t.Fatalf("NamesOf round-trip for %q via %q = %q", name, fn, got)
			}
		}
	})
}

func FuzzGeminiName(f *testing.F) {
	for _, seed := range []string{
		"",
		"bash",
		"123tool",
		"-dash",
		"_under",
		"mcp__server__" + string(make([]byte, 140)),
		"9_unicode_工具_" + "abcdefghijklmnopqrstuvwxyz0123456789_abcdefghijklmnopqrstuvwxyz0123456789_abcdefghijklmnopqrstuvwxyz0123456789",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		fn := GeminiName(name)
		if len(fn) > maxGeminiName {
			t.Fatalf("GeminiName(%q) length = %d > %d (%q)", name, len(fn), maxGeminiName, fn)
		}
		if utf8.ValidString(name) && !utf8.ValidString(fn) {
			t.Fatalf("GeminiName(%q) broke UTF-8: %x", name, fn)
		}
		if name != "" {
			c := fn[0]
			if c != '_' && !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
				t.Fatalf("GeminiName(%q) = %q starts with invalid byte %q", name, fn, c)
			}
		}
		if again := GeminiName(name); again != fn {
			t.Fatalf("GeminiName(%q) non-deterministic: %q vs %q", name, fn, again)
		}
		if utf8.ValidString(name) {
			toolJSON, err := json.Marshal(map[string]any{"name": name})
			if err != nil {
				t.Fatalf("marshal tool: %v", err)
			}
			names := GeminiNamesOf([]json.RawMessage{toolJSON}, nil)
			if got := names.Tool(fn); got != name {
				t.Fatalf("GeminiNamesOf round-trip for %q via %q = %q", name, fn, got)
			}
		}
	})
}

func FuzzConvertAnswersNoPanic(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","reasoning_content":"step 1","content":"hi"},"finish_reason":"stop"}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"thought":true,"text":"think"},{"text":"hi"}]},"finishReason":"STOP"}]}`,
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`,
		`{"choices":[{"delta":{"reasoning_content":"step","content":"a"}}]}`,
		`not json`,
	} {
		f.Add([]byte(seed))
	}
	sig := func() string { return "fuzz-sig" }
	gSign := func(value string) string { return "fuzz-gsig:" + value }
	f.Fuzz(func(t *testing.T, raw []byte) {
		if out, err := Answer(raw, "alias", "msg_1", nil, sig, nil); err == nil && !json.Valid(out) {
			t.Fatalf("Answer produced invalid JSON: %q", out)
		}
		if out, err := GeminiAnswer(raw, "alias", "msg_1", nil, gSign, nil); err == nil && !json.Valid(out) {
			t.Fatalf("GeminiAnswer produced invalid JSON: %q", out)
		}
		meta := ResponseMeta{ID: "resp_1", Model: "alias", CreatedAt: 1700000000}
		if out, err := ResponsesAnswer(raw, meta); err == nil && !json.Valid(out) {
			t.Fatalf("ResponsesAnswer produced invalid JSON: %q", out)
		}
		s := NewStream("alias", "msg_1", sig, nil)
		_, _ = s.Chunk(raw)
		_ = s.End()
		gs := NewGeminiStream("alias", "msg_1", gSign, nil)
		_, _ = gs.Chunk(raw)
		_ = gs.End()
	})
}
