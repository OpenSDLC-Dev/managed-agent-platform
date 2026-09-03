package anthropic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
)

// simpleTurnSSE is a minimal message_start/message_delta/message_stop stream,
// enough to complete a turn without exercising anything these tests are not
// about.
var simpleTurnSSE = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":3}}}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
	`{"type":"message_stop"}`,
}

// startWith is start (anthropic_test.go) with control over the config beyond
// the fixed one start() builds — these tests need FlattenSearchResults, which
// start()'s fixed Config never sets. base_url is always the fake server's own.
func startWith(t *testing.T, f *fakeServer, cfg provider.Config) provider.Provider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	p, err := anthropic.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// generateContent drives one turn carrying a single user message with the
// given raw content and returns the content the adapter actually put on the
// wire for that message, decoded generically (so a test can compare it
// structurally against what it sent, or against a hand-built expectation).
func generateContent(t *testing.T, flatten bool, content string) any {
	t.Helper()
	f := &fakeServer{sse: simpleTurnSSE}
	p := startWith(t, f, provider.Config{
		Protocol:             "anthropic",
		Model:                "upstream-model",
		APIKey:               testAPIKey,
		FlattenSearchResults: flatten,
	})
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(content)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	collect(t, stream)
	msgs, _ := f.gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	return msgs[0].(map[string]any)["content"]
}

// TestFlattenSearchResults covers the route opt-in Config.FlattenSearchResults
// (#565): off, it is a no-op (CLAUDE.md principle 4, near-zero-conversion);
// on, it rewrites search_result blocks inside tool_result content to text via
// the rendering shared with provider/openai (provider.SearchResultText),
// leaving everything else — sibling blocks, string-form content, a
// tool_result's own fields, a block it cannot safely render — untouched.
func TestFlattenSearchResults(t *testing.T) {
	cases := []struct {
		name    string
		flatten bool
		content string
		want    string // JSON; empty means "unchanged from content"
	}{
		{
			name:    "off leaves a tool_result's search_result blocks unchanged",
			flatten: false,
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"search_result","citations":{"enabled":false},"content":[{"type":"text","text":"How to write Go."}],"source":"https://go.dev/doc/","title":"Go docs"},` +
				`{"type":"text","text":"tail"}]}]`,
		},
		{
			name:    "on rewrites only search_result blocks, leaving sibling text/image and a second tool_result's string content untouched",
			flatten: true,
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"search_result","citations":{"enabled":false},"content":[{"type":"text","text":"How to write Go."}],"source":"https://go.dev/doc/","title":"Go docs"},` +
				`{"type":"text","text":"tail"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA="}}]},` +
				`{"type":"tool_result","tool_use_id":"toolu_2","content":"plain string content"}]`,
			want: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"text","text":"Go docs (https://go.dev/doc/)\nHow to write Go.\n"},` +
				`{"type":"text","text":"tail"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA="}}]},` +
				`{"type":"tool_result","tool_use_id":"toolu_2","content":"plain string content"}]`,
		},
		{
			// Two search_result blocks in one tool_result.content are BOTH
			// rewritten, proving the scan does not stop after the first
			// match; the tool_result's own is_error and cache_control
			// survive on the tool_result block exactly as sent (a
			// search_result block carries no cache_control on the wire —
			// see flattenSearchResultBlock — so there is none to check on
			// its replacement).
			name:    "on rewrites every search_result in one tool_result and keeps the tool_result's own fields",
			flatten: true,
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"cache_control":{"type":"ephemeral"},"content":[` +
				`{"type":"search_result","citations":{"enabled":false},"content":[{"type":"text","text":"first hit"}],"source":"https://a.example/","title":"A"},` +
				`{"type":"search_result","citations":{"enabled":true},"content":[{"type":"text","text":"second hit"}],"source":"https://b.example/","title":"B"}]}]`,
			want: `[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"cache_control":{"type":"ephemeral"},"content":[` +
				`{"type":"text","text":"A (https://a.example/)\nfirst hit\n"},` +
				`{"type":"text","text":"B (https://b.example/)\nsecond hit\n"}]}]`,
		},
		{
			name:    "on leaves content with no tool_result block unchanged",
			flatten: true,
			content: `[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls"}}]`,
		},
		{
			name:    "on leaves string-form content unchanged",
			flatten: true,
			content: `"hi"`,
		},
		{
			name:    "on leaves a tool_result without any search_result block unchanged",
			flatten: true,
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"plain"}]}]`,
		},
		{
			// A search_result SearchResultText cannot render — a non-string
			// source, a non-text inner block — is left exactly as received
			// rather than failing the whole request (validity is the
			// endpoint's judgment, not this adapter's); a well-formed
			// sibling in the same tool_result is still flattened.
			name:    "on leaves unrenderable search_result blocks untouched but still flattens a well-formed sibling",
			flatten: true,
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"search_result","title":"bad source","source":{"not":"a string"},"content":[]},` +
				`{"type":"search_result","title":"bad inner","source":"https://a.example/","content":[{"type":"image","source":{}}]},` +
				`{"type":"search_result","title":"Go docs","source":"https://go.dev/doc/","content":[{"type":"text","text":"How to write Go."}]}]}]`,
			want: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"search_result","title":"bad source","source":{"not":"a string"},"content":[]},` +
				`{"type":"search_result","title":"bad inner","source":"https://a.example/","content":[{"type":"image","source":{}}]},` +
				`{"type":"text","text":"Go docs (https://go.dev/doc/)\nHow to write Go.\n"}]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateContent(t, tc.flatten, tc.content)

			wantJSON := tc.want
			if wantJSON == "" {
				wantJSON = tc.content
			}
			var want any
			if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("content = %#v, want %#v", got, want)
			}
		})
	}
}
