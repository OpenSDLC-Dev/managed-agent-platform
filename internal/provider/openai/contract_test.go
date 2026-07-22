package openai_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/providertest"
)

// TestSharedContract runs the cross-provider contract suite against the OpenAI
// adapter. The lossy-conversion assertions and the finish_reason -> stop_reason
// mapping table stay in openai_test.go / openai_lossy_test.go; this pins only
// the protocol-agnostic invariants shared with the Anthropic adapter.
func TestSharedContract(t *testing.T) {
	providertest.Run(t, providertest.Backend{
		Turn: func(t *testing.T, s providertest.Script) provider.Provider {
			return start(t, &fakeServer{sse: renderOpenAITurn(s)})
		},
		Hang: startHangingOpenAI,
	})
}

// renderOpenAITurn scripts a Script as OpenAI Chat Completions streaming delta
// payloads (the fakeServer wraps each as one `data:` frame and appends the
// trailing `[DONE]`). A tool turn ends with finish_reason tool_calls — its
// natural completion — proving stop_reason tool_use is derived from the tool
// call itself, not copied from finish_reason.
func renderOpenAITurn(s providertest.Script) []string {
	var frames []string
	if s.Text != "" {
		for _, part := range splitHalf(s.Text) {
			frames = append(frames, fmt.Sprintf(
				`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, jsonString(part)))
		}
	}
	finish := "stop"
	if s.Tool != nil {
		finish = "tool_calls"
		frames = append(frames, fmt.Sprintf(
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]},"finish_reason":null}]}`,
			jsonString(s.Tool.ID), jsonString(s.Tool.Name)))
		// A non-empty input streams as >=2 argument fragments so accumulation
		// is exercised; an empty object streams as none (the adapter defaults an
		// unstreamed input to {}).
		for _, frag := range splitToolInput(s.Tool.Input) {
			frames = append(frames, fmt.Sprintf(
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`, jsonString(frag)))
		}
	}
	frames = append(frames, fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%s}]}`, jsonString(finish)))
	if s.Usage != nil {
		frames = append(frames, fmt.Sprintf(
			`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.InputTokens+s.Usage.OutputTokens))
	}
	return frames
}

// startHangingOpenAI stands up an upstream that streams one content delta and
// then blocks, so the shared suite can cancel the request context mid-stream.
func startHangingOpenAI(t *testing.T) provider.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		// Block until the client disconnects (its context cancel) or a safety
		// timeout, so the turn never completes on its own.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	p, err := openai.New(provider.Config{Protocol: "openai", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// jsonString renders v as a JSON string literal, escaping as needed.
func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// splitHalf splits s into two fragments so text streams as more than one delta.
func splitHalf(s string) []string {
	if len(s) < 2 {
		return []string{s}
	}
	mid := len(s) / 2
	return []string{s[:mid], s[mid:]}
}

// splitToolInput splits a tool-input JSON object into >=2 streaming fragments.
// An empty object yields none (the adapter defaults an unstreamed input to {}).
func splitToolInput(input string) []string {
	if input == "" || input == "{}" {
		return nil
	}
	mid := len(input) / 2
	return []string{input[:mid], input[mid:]}
}
