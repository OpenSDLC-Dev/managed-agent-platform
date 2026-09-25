package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
)

// A user turn answers the assistant turn before it in that turn's tool_use
// order, whatever order the log holds the results in. The log keeps the order
// the results were processed in: a denial that resumes the primary beside a
// later call's result writes its own result behind the resume's running
// pair, after that later result (#793; 2026-09-19-custom-order-followup
// setup.json, ask-first-deny.final-audit.events idx 15 to 18 lists the same
// order). A backend that pairs results with calls by position — the tool
// messages an OpenAI-compatible endpoint reads — must still see them in call
// order, so both providers' requests carry the results as the calls ran.
func TestBuildRequestAnswersToolUsesInCallOrder(t *testing.T) {
	first := ev(2, domain.EventAgentToolUse, `{"name":"bash","input":{},"evaluated_permission":"ask"}`)
	second := ev(3, domain.EventAgentCustomToolUse, `{"name":"lookup","input":{}}`)
	history := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"go"}`),
		first, second,
		ev(4, domain.EventUserToolConfirm, `{"tool_use_id":"`+first.ID.String()+`","result":"deny"}`),
		ev(5, domain.EventUserCustomToolRes, `{"custom_tool_use_id":"`+second.ID.String()+`","content":[{"type":"text","text":"found"}]}`),
		ev(6, domain.EventSessionStatusRunning, `{}`),
		ev(7, domain.EventSessionThreadStatusRunning, `{}`),
		ev(8, domain.EventAgentToolResult, `{"tool_use_id":"`+first.ID.String()+`","content":[{"type":"text","text":"denied"}],"is_error":true}`),
	}
	req, _, err := buildRequest("", nil, history, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want the message, the calls and their answers", len(req.Messages))
	}
	var answers []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(req.Messages[2].Content, &answers); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range answers {
		ids = append(ids, a.ToolUseID)
	}
	want := []string{first.ID.String(), second.ID.String()}
	if !slices.Equal(ids, want) {
		t.Fatalf("results answer %v, want the calls' order %v", ids, want)
	}

	for _, tc := range []struct {
		protocol string
		factory  provider.Factory
		// ids reads the answered calls off the wire body, in order.
		ids func(body map[string]any) []string
	}{
		{"anthropic", anthropic.New, func(body map[string]any) []string {
			msgs, _ := body["messages"].([]any)
			last, _ := msgs[len(msgs)-1].(map[string]any)
			blocks, _ := last["content"].([]any)
			var out []string
			for _, b := range blocks {
				out = append(out, b.(map[string]any)["tool_use_id"].(string))
			}
			return out
		}},
		{"openai", openai.New, func(body map[string]any) []string {
			msgs, _ := body["messages"].([]any)
			var out []string
			for _, m := range msgs {
				if msg := m.(map[string]any); msg["role"] == "tool" {
					out = append(out, msg["tool_call_id"].(string))
				}
			}
			return out
		}},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			bodies := make(chan map[string]any, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("request body: %v", err)
				}
				bodies <- body
				// A refusal the SDKs do not retry: the request is all this reads.
				http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stop"}}`, http.StatusBadRequest)
			}))
			defer srv.Close()
			p, err := tc.factory(provider.Config{Protocol: tc.protocol, Model: "m", BaseURL: srv.URL, APIKey: "k"})
			if err != nil {
				t.Fatal(err)
			}
			if stream, err := p.Generate(context.Background(), req); err == nil {
				for stream.Next() {
				}
				_ = stream.Close()
			}
			select {
			case body := <-bodies:
				if got := tc.ids(body); !slices.Equal(got, want) {
					t.Errorf("wire answers %v, want the calls' order %v", got, want)
				}
			default:
				t.Fatal("no request reached the endpoint")
			}
		})
	}
}
