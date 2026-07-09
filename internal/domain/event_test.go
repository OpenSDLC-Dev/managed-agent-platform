package domain

import "testing"

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
