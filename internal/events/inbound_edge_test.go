package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

func norm(t *testing.T, envKind string, evs ...string) ([]events.NewEvent, error) {
	t.Helper()
	raws := make([]json.RawMessage, len(evs))
	for i, e := range evs {
		raws[i] = json.RawMessage(e)
	}
	return events.NormalizeInbound(envKind, raws)
}

// The wire-shape edge cases: NormalizeInbound is pure, so every validation
// branch is exercised directly.
func TestNormalizeInboundRejections(t *testing.T) {
	cases := []struct {
		name string
		ev   string
		frag string
	}{
		{"not an object", `"hi"`, "must be a JSON object"},
		{"null event", `null`, "must be a JSON object"},
		{"type not string", `{"type":42}`, "type must be a string"},
		{"interrupt unknown key", `{"type":"user.interrupt","force":true}`, "unknown field"},
		{"deny_message not string", `{"type":"user.tool_confirmation","result":"deny","tool_use_id":"x","deny_message":7}`, "deny_message must be a string"},
		{"tool_use_id not string", `{"type":"user.tool_confirmation","result":"allow","tool_use_id":[1]}`, "must be a string"},
		{"tool_use_id empty", `{"type":"user.tool_confirmation","result":"allow","tool_use_id":""}`, "must not be empty"},
		{"result null", `{"type":"user.tool_confirmation","result":null,"tool_use_id":"x"}`, "result is required"},
		{"content blocks not objects", `{"type":"user.message","content":[42]}`, "must be a JSON object"},
		{"block missing type", `{"type":"user.message","content":[{"text":"x"}]}`, "type is required"},
		{"text unknown key", `{"type":"user.message","content":[{"type":"text","text":"x","weight":1}]}`, "unknown field"},
		{"text not string", `{"type":"user.message","content":[{"type":"text","text":9}]}`, "text"},
		{"image unknown key", `{"type":"user.message","content":[{"type":"image","source":{"type":"url","url":"https://x"},"alt":"y"}]}`, "unknown field"},
		{"source not object", `{"type":"user.message","content":[{"type":"image","source":"x"}]}`, "must be a JSON object"},
		{"source null", `{"type":"user.message","content":[{"type":"image","source":null}]}`, "source is required"},
		{"source missing type", `{"type":"user.message","content":[{"type":"image","source":{"url":"https://x"}}]}`, "type is required"},
		{"base64 unknown key", `{"type":"user.message","content":[{"type":"image","source":{"type":"base64","data":"aGk=","media_type":"image/png","size":3}}]}`, "unknown field"},
		{"base64 missing media_type", `{"type":"user.message","content":[{"type":"image","source":{"type":"base64","data":"aGk="}}]}`, "media_type is required"},
		{"url missing url", `{"type":"user.message","content":[{"type":"document","source":{"type":"url"}}]}`, "url is required"},
		{"url unknown key", `{"type":"user.message","content":[{"type":"document","source":{"type":"url","url":"https://x","ttl":1}}]}`, "unknown field"},
		{"file missing file_id", `{"type":"user.message","content":[{"type":"document","source":{"type":"file"}}]}`, "file_id is required"},
		{"file unknown key", `{"type":"user.message","content":[{"type":"document","source":{"type":"file","file_id":"file_1","name":"x"}}]}`, "unknown field"},
		{"document context not string", `{"type":"user.message","content":[{"type":"document","source":{"type":"url","url":"https://x"},"context":5}]}`, "context must be a string"},
		{"document unknown key", `{"type":"user.message","content":[{"type":"document","source":{"type":"url","url":"https://x"},"pages":2}]}`, "unknown field"},
		{"image with text source", `{"type":"user.message","content":[{"type":"image","source":{"type":"text","data":"x","media_type":"text/plain"}}]}`, "not allowed here"},
		{"search_result missing title", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","content":[{"type":"text","text":"b"}]}]}`, "title is required"},
		{"search_result source not string", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":{"type":"url","url":"https://x"},"title":"t","content":[]}]}`, "source must be a string"},
		{"search_result missing content", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t"}]}`, "requires content"},
		{"search_result nested image", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","content":[{"type":"image","source":{"type":"url","url":"https://x"}}]}]}`, "not allowed here"},
		{"citations not object", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":true,"content":[{"type":"text","text":"b"}]}]}`, "must be a JSON object"},
		{"citations unknown key", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enable":true},"content":[{"type":"text","text":"b"}]}]}`, "unknown field"},
		{"system content null", `{"type":"system.message","content":null}`, "content is required"},
		{"system unknown key", `{"type":"system.message","content":[{"type":"text","text":"x"}],"role":"system"}`, "unknown field"},
		{"custom result unknown key", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","exit_code":0}`, "unknown field"},
	}
	for _, tc := range cases {
		var err error
		if strings.Contains(tc.ev, "system.message") && !strings.Contains(tc.name, "alone") {
			_, err = norm(t, "cloud", `{"type":"user.message","content":[{"type":"text","text":"x"}]}`, tc.ev)
		} else {
			_, err = norm(t, "cloud", tc.ev)
		}
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.frag) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.frag)
		}
	}
}

func TestNormalizeInboundNullHandling(t *testing.T) {
	// Explicit nulls behave like absence for every nullable field.
	evs, err := norm(t, "self_hosted",
		`{"type":"user.interrupt","session_thread_id":null}`,
		`{"type":"user.tool_confirmation","result":"allow","tool_use_id":"tu","deny_message":null,"session_thread_id":null}`,
		`{"type":"user.tool_result","tool_use_id":"tu","content":null,"is_error":null,"session_thread_id":null}`,
	)
	if err != nil {
		t.Fatalf("explicit nulls rejected: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(evs[2].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"content", "is_error", "session_thread_id"} {
		if string(payload[key]) != "null" {
			t.Errorf("normalized %s = %s, want explicit null", key, payload[key])
		}
	}

	// Confirmation with deny and no message: deny_message normalizes to null.
	if err := json.Unmarshal(evs[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["deny_message"]) != "null" {
		t.Errorf("deny_message = %s, want null", payload["deny_message"])
	}
}

func TestNormalizeInboundDocumentTextSource(t *testing.T) {
	// Documents (unlike images) accept the plain-text source kind.
	_, err := norm(t, "cloud",
		`{"type":"user.message","content":[{"type":"document","source":{"type":"text","data":"notes","media_type":"text/plain"},"context":null,"title":"t"}]}`)
	if err != nil {
		t.Errorf("document text source rejected: %v", err)
	}
}
