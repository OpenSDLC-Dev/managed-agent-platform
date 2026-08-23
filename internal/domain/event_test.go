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

// The table is exhaustive over the inbound set on purpose: every exclusion
// below is load-bearing rather than a default, and the way this rots is
// somebody "fixing" one of the three false arms to match Inbound().
func TestEventStartsNewWork(t *testing.T) {
	cases := []struct {
		et   EventType
		want bool
		why  string
	}{
		{EventUserMessage, true, "the plain case: a client asked for something"},
		{EventUserDefineOutcome, true, "the agent begins work on it immediately"},
		{EventSystemMessage, true, "an operator reaching the session is a demand too"},
		{EventUserToolConfirm, true, "a human just approved a gated call, and it is not " +
			"in pendingInputTypes — without this the very next turn is refused"},
		{EventUserToolResult, false, "on a self_hosted environment EVERY tool call comes " +
			"back as a client POST, so resetting here would zero the counter continuously"},
		{EventUserCustomToolRes, false, "a custom tool's answer is the same case"},
		{EventUserInterrupt, false, "a stop is not a demand; treating it as one would let " +
			"an operator's own stop hand the session a fresh budget"},
	}
	seen := map[EventType]bool{}
	for _, c := range cases {
		seen[c.et] = true
		if got := c.et.StartsNewWork(); got != c.want {
			t.Errorf("%q.StartsNewWork() = %v, want %v — %s", c.et, got, c.want, c.why)
		}
	}
	// Every inbound type must be classified here, so a new one cannot join the
	// taxonomy without someone deciding which side of the bound it falls on.
	for _, et := range []EventType{
		EventUserMessage, EventUserInterrupt, EventUserToolConfirm,
		EventUserCustomToolRes, EventUserToolResult, EventUserDefineOutcome,
		EventSystemMessage,
	} {
		if !seen[et] {
			t.Errorf("inbound type %q is unclassified by this table", et)
		}
	}
	// Nothing the platform produces can reset a bound on the platform's own
	// autonomy — that is the whole point of the bound.
	for _, et := range []EventType{
		EventAgentMessage, EventAgentToolUse, EventAgentToolResult,
		EventSessionStatusIdle, EventSessionError,
	} {
		if et.StartsNewWork() {
			t.Errorf("%q is platform-produced and must not start new work", et)
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
