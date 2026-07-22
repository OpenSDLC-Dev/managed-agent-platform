package anthropic_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/providertest"
)

// TestSharedContract runs the cross-provider contract suite against the
// Anthropic adapter. The wire-specific behavior — request shape, credential
// redaction, malformed-stream handling, the usage edge cases — stays in
// anthropic_test.go; this pins only the protocol-agnostic invariants the suite
// also holds the OpenAI adapter to.
func TestSharedContract(t *testing.T) {
	providertest.Run(t, providertest.Backend{
		Turn: func(t *testing.T, s providertest.Script) provider.Provider {
			return start(t, &fakeServer{sse: renderAnthropicTurn(s)})
		},
		Hang: startHangingAnthropic,
	})
}

// renderAnthropicTurn scripts a Script as Anthropic Messages SSE data payloads
// (the fakeServer names each event from the payload's type field). The whole
// usage reading rides message_start; message_delta carries only the stop
// reason, mirroring the official API's shape.
func renderAnthropicTurn(s providertest.Script) []string {
	ms := `{"type":"message_start","message":{"id":"msg_ct","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null`
	if s.Usage != nil {
		ms += fmt.Sprintf(`,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}`,
			s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.CacheCreationInputTokens, s.Usage.CacheReadInputTokens)
	}
	ms += `}}`
	frames := []string{ms}

	stop := "end_turn"
	if s.Text != "" {
		frames = append(frames, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, part := range splitHalf(s.Text) {
			frames = append(frames, fmt.Sprintf(
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonString(part)))
		}
		frames = append(frames, `{"type":"content_block_stop","index":0}`)
	}
	if s.Tool != nil {
		stop = "tool_use"
		frames = append(frames, fmt.Sprintf(
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
			jsonString(s.Tool.ID), jsonString(s.Tool.Name)))
		// A non-empty input streams as >=2 input_json_delta frames so
		// accumulation is exercised; an empty object streams as none — the
		// adapter defaults an unstreamed input to {}, as the real protocol does.
		for _, frag := range splitToolInput(s.Tool.Input) {
			frames = append(frames, fmt.Sprintf(
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%s}}`, jsonString(frag)))
		}
		frames = append(frames, `{"type":"content_block_stop","index":1}`)
	}
	frames = append(frames, fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null}}`, jsonString(stop)))
	frames = append(frames, `{"type":"message_stop"}`)
	return frames
}

// startHangingAnthropic stands up an upstream that streams one text delta and
// then blocks, so the shared suite can cancel the request context mid-stream.
func startHangingAnthropic(t *testing.T) provider.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, data := range []string{
			`{"type":"message_start","message":{"id":"msg_hang","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		} {
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(data), &m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, data)
		}
		fl.Flush()
		// Block until the client disconnects (its context cancel) or a safety
		// timeout, so the turn never completes on its own.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	p, err := anthropic.New(provider.Config{Protocol: "anthropic", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey})
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
