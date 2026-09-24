package events_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// norm normalizes under a worker's credential, the one no event type refuses,
// so a case isolates the rule it is about.
func norm(t *testing.T, envKind string, evs ...string) ([]events.NewEvent, error) {
	t.Helper()
	return normAs(t, envKind, events.EnvironmentCredential, evs...)
}

func normAs(t *testing.T, envKind string, cred events.Credential, evs ...string) ([]events.NewEvent, error) {
	t.Helper()
	raws := make([]json.RawMessage, len(evs))
	for i, e := range evs {
		raws[i] = json.RawMessage(e)
	}
	return events.NormalizeInbound(envKind, cred, raws)
}

// TestNormalizeInboundToolResultCredential: a user.tool_result is a worker's
// to post (#662). Under any other credential it is refused before anything
// else about it is read — its environment kind and its own fields included —
// with the sentinel the API answers 403, carrying the event's index. Only an
// environment credential reaches the environment-kind rule behind it.
func TestNormalizeInboundToolResultCredential(t *testing.T) {
	const msg = `{"type":"user.message","content":[{"type":"text","text":"x"}]}`
	const result = `{"type":"user.tool_result","tool_use_id":"tu"}`
	for _, kind := range []string{"self_hosted", "cloud"} {
		_, err := normAs(t, kind, events.ManagementCredential, msg, result)
		if !errors.Is(err, events.ErrEnvironmentCredentialRequired) || !strings.HasPrefix(err.Error(), "events[1]: ") {
			t.Errorf("%s, management: err = %v, want events[1] and ErrEnvironmentCredentialRequired", kind, err)
		}
	}
	// Refused before its fields are read: a malformed result is the credential's refusal too.
	if _, err := normAs(t, "self_hosted", events.ManagementCredential, `{"type":"user.tool_result","bogus":1}`); !errors.Is(err, events.ErrEnvironmentCredentialRequired) {
		t.Errorf("malformed result, management: err = %v, want ErrEnvironmentCredentialRequired", err)
	}
	// The zero value is not a worker's.
	if _, err := normAs(t, "self_hosted", events.Credential(0), result); !errors.Is(err, events.ErrEnvironmentCredentialRequired) {
		t.Errorf("zero credential: err = %v, want ErrEnvironmentCredentialRequired", err)
	}
	// Every other type is the credential's to send.
	if _, err := normAs(t, "cloud", events.ManagementCredential, msg,
		`{"type":"user.custom_tool_result","custom_tool_use_id":"c"}`,
		`{"type":"user.tool_confirmation","result":"allow","tool_use_id":"tu"}`,
		`{"type":"user.interrupt"}`); err != nil {
		t.Errorf("management batch without a tool_result: %v", err)
	}
	_, err := normAs(t, "cloud", events.EnvironmentCredential, result)
	if err == nil || errors.Is(err, events.ErrEnvironmentCredentialRequired) || !strings.Contains(err.Error(), "only valid on self_hosted") {
		t.Errorf("cloud, environment credential: err = %v, want the environment-kind refusal", err)
	}
	if _, err := normAs(t, "self_hosted", events.EnvironmentCredential, result); err != nil {
		t.Errorf("self_hosted, environment credential: %v", err)
	}
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
		{"search_result missing content", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enabled":true}}]}`, "requires content"},
		{"search_result nested image", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enabled":true},"content":[{"type":"image","source":{"type":"url","url":"https://x"}}]}]}`, "not allowed here"},
		{"search_result missing citations", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","content":[{"type":"text","text":"b"}]}]}`, "citations is required"},
		{"citations missing enabled", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{},"content":[{"type":"text","text":"b"}]}]}`, "boolean enabled"},
		{"citations enabled not bool", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enabled":"yes"},"content":[{"type":"text","text":"b"}]}]}`, "boolean enabled"},
		{"citations not object", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":true,"content":[{"type":"text","text":"b"}]}]}`, "must be a JSON object"},
		{"citations unknown key", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enable":true},"content":[{"type":"text","text":"b"}]}]}`, "unknown field"},
		{"text null", `{"type":"user.message","content":[{"type":"text","text":null}]}`, "text"},
		// An empty text block is refused wherever a text block may appear: the
		// reference API rejects it in a tool result (its runner substitutes
		// "(no output)" since anthropic-sdk-go v1.63.1 —
		// betasessiontoolrunner.go toToolResultContent), and every carrier here
		// is replayed to a Messages endpoint that rejects it too (INFERRED,
		// docs/DIVERGENCES.md). user.tool_result shares the same block
		// validator (self_hosted-only, so it cannot sit in this cloud table).
		{"text empty in custom tool result", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"text","text":""}]}`, "must not be empty"},
		{"text empty in message", `{"type":"user.message","content":[{"type":"text","text":""}]}`, "must not be empty"},
		{"string content empty in message", `{"type":"user.message","content":""}`, "must not be empty"},
		{"text empty in system message", `{"type":"system.message","content":[{"type":"text","text":""}]}`, "must not be empty"},
		{"text empty in search_result", `{"type":"user.custom_tool_result","custom_tool_use_id":"x","content":[{"type":"search_result","source":"https://x","title":"t","citations":{"enabled":true},"content":[{"type":"text","text":""}]}]}`, "must not be empty"},
		{"NUL escape in text", `{"type":"user.message","content":[{"type":"text","text":"a\u0000b"}]}`, "U+0000"},
		{"NUL escape in deny_message", `{"type":"user.tool_confirmation","result":"deny","tool_use_id":"x","deny_message":"no\u0000pe"}`, "U+0000"},
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

// The agent-to-agent message pair is platform-emitted like every other agent.*
// event (plan 35 decision 6), so a client posting one is told that rather than
// "unknown event type" — the difference between a wrong endpoint and a typo.
func TestNormalizeInboundRefusesThreadMessages(t *testing.T) {
	for _, typ := range []string{"agent.thread_message_sent", "agent.thread_message_received"} {
		_, err := norm(t, "self_hosted", `{"type":"`+typ+`"}`)
		if err == nil || !strings.Contains(err.Error(), "emitted by the platform") {
			t.Errorf("%s: err = %v, want it refused as platform-emitted", typ, err)
		}
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
