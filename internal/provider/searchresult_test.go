package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// SearchResultText is the rendering both provider/openai (which flattens every
// tool_result, since Chat Completions tool messages are text-only) and
// provider/anthropic's opt-in flatten_search_results route flag (#565) share,
// so the two adapters render a search_result block identically.
func TestSearchResultText(t *testing.T) {
	got, err := provider.SearchResultText("Go docs", json.RawMessage(`"https://go.dev/doc/"`),
		json.RawMessage(`[{"type":"text","text":"How to write Go."}]`))
	if err != nil {
		t.Fatalf("SearchResultText: %v", err)
	}
	if want := "Go docs (https://go.dev/doc/)\nHow to write Go.\n"; got != want {
		t.Errorf("SearchResultText = %q, want %q", got, want)
	}
}

func TestSearchResultTextEmptyContent(t *testing.T) {
	got, err := provider.SearchResultText("t", json.RawMessage(`"https://a.example/"`), nil)
	if err != nil {
		t.Fatalf("SearchResultText: %v", err)
	}
	if want := "t (https://a.example/)\n"; got != want {
		t.Errorf("SearchResultText = %q, want %q", got, want)
	}
}

// The wire union says search_result content is text blocks only, so anything
// else is an upstream bug and fails loudly rather than vanishing silently.
func TestSearchResultTextRejectsNonTextInner(t *testing.T) {
	_, err := provider.SearchResultText("t", json.RawMessage(`"https://a.example/"`),
		json.RawMessage(`[{"type":"image","source":{}}]`))
	if err == nil {
		t.Fatal("a non-text block inside search_result content should fail loudly")
	}
	if !strings.Contains(err.Error(), `unsupported block "image" inside search_result`) {
		t.Errorf("error = %v, want the inner type guard's message", err)
	}
}

func TestSearchResultTextBadSource(t *testing.T) {
	_, err := provider.SearchResultText("t", json.RawMessage(`{"not":"a string"}`), nil)
	if err == nil {
		t.Fatal("a non-string source should fail loudly")
	}
}
