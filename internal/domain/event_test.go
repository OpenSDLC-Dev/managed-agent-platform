package domain

import (
	"encoding/json"
	"testing"
)

func TestEventTypeDomain(t *testing.T) {
	cases := map[EventType]string{
		EventUserMessage:           "user",
		EventSystemMessage:         "system",
		EventAgentToolUse:          "agent",
		EventSessionStatusIdle:     "session",
		EventSpanModelRequestStart: "span",
		EventStart:                 "event_start", // no dot → whole string
	}
	for et, want := range cases {
		if got := et.Domain(); got != want {
			t.Errorf("%q.Domain() = %q, want %q", et, got, want)
		}
	}
}

func TestEventInbound(t *testing.T) {
	inbound := []EventType{
		EventUserMessage, EventUserInterrupt, EventUserToolConfirm,
		EventUserCustomToolRes, EventUserToolResult, EventUserDefineOutcome,
		EventSystemMessage,
	}
	for _, et := range inbound {
		if !et.Inbound() {
			t.Errorf("%q should be inbound", et)
		}
	}
	outbound := []EventType{
		EventAgentMessage, EventAgentToolUse, EventAgentToolResult,
		EventSessionStatusRunning, EventSessionStatusIdle, EventSessionError,
		EventSpanModelRequestStart, EventSpanModelRequestEnd,
	}
	for _, et := range outbound {
		if et.Inbound() {
			t.Errorf("%q should not be inbound", et)
		}
	}
}

func TestEventPersisted(t *testing.T) {
	if EventStart.Persisted() || EventDelta.Persisted() {
		t.Errorf("preview frames must not be persisted")
	}
	if !EventAgentMessage.Persisted() || !EventSessionStatusIdle.Persisted() {
		t.Errorf("real events must be persisted")
	}
}

func TestModelUsageJSON(t *testing.T) {
	// Wire shape: all four counters always present, speed present-but-null
	// when unset (nullable, never omitted).
	b, err := json.Marshal(ModelUsage{InputTokens: 7, OutputTokens: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":7,"output_tokens":3,"speed":null}`
	if string(b) != want {
		t.Errorf("ModelUsage JSON = %s, want %s", b, want)
	}
}

func TestStopReasonJSON(t *testing.T) {
	// requires_action carries event_ids; the other variants omit it.
	b, _ := json.Marshal(StopReason{Type: StopRequiresAction, EventIDs: []ID{"sevt_1"}})
	if string(b) != `{"type":"requires_action","event_ids":["sevt_1"]}` {
		t.Errorf("requires_action JSON = %s", b)
	}
	for _, sr := range []StopReasonType{StopEndTurn, StopRetriesExhausted} {
		b, _ := json.Marshal(StopReason{Type: sr})
		if string(b) != `{"type":"`+string(sr)+`"}` {
			t.Errorf("%s JSON = %s", sr, b)
		}
	}
}
