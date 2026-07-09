package domain

import (
	"strings"
	"time"
)

// EventType is a "{domain}.{action}" event name, wire-compatible with Anthropic
// Managed Agents. The full taxonomy is authoritative here; the event log stores
// these verbatim and the SSE stream replays them.
type EventType string

// Inbound events — the client/harness sends these into a session.
const (
	EventUserMessage       EventType = "user.message"
	EventUserInterrupt     EventType = "user.interrupt"
	EventUserToolConfirm   EventType = "user.tool_confirmation"
	EventUserCustomToolRes EventType = "user.custom_tool_result"
	EventUserToolResult    EventType = "user.tool_result" // self_hosted: worker returns agent_toolset results
	EventUserDefineOutcome EventType = "user.define_outcome"
	EventSystemMessage     EventType = "system.message"
)

// Outbound agent events — produced by the brain during a turn.
const (
	EventAgentMessage       EventType = "agent.message"
	EventAgentThinking      EventType = "agent.thinking"
	EventAgentToolUse       EventType = "agent.tool_use"
	EventAgentToolResult    EventType = "agent.tool_result"
	EventAgentMCPToolUse    EventType = "agent.mcp_tool_use"
	EventAgentMCPToolResult EventType = "agent.mcp_tool_result"
	EventAgentCustomToolUse EventType = "agent.custom_tool_use"
)

// Session lifecycle events.
const (
	EventSessionStatusRunning     EventType = "session.status_running"
	EventSessionStatusIdle        EventType = "session.status_idle" // carries stop_reason
	EventSessionStatusRescheduled EventType = "session.status_rescheduled"
	EventSessionStatusTerminated  EventType = "session.status_terminated"
	EventSessionError             EventType = "session.error"
	EventSessionUpdated           EventType = "session.updated"
	EventSessionDeleted           EventType = "session.deleted"
)

// Span (observability) events. These are emitted from the same instrumentation
// point as the OTel spans so the two never drift.
const (
	EventSpanModelRequestStart EventType = "span.model_request_start"
	EventSpanModelRequestEnd   EventType = "span.model_request_end" // carries model_usage
)

// Stream-only preview frames. These are NOT persisted and never carry their own
// id/processed_at — their only identity is the previewed event's id.
const (
	EventStart EventType = "event_start"
	EventDelta EventType = "event_delta"
)

// Domain returns the part before the ".", e.g. "agent" for "agent.tool_use".
func (t EventType) Domain() string {
	if i := strings.IndexByte(string(t), '.'); i >= 0 {
		return string(t)[:i]
	}
	return string(t)
}

// Inbound reports whether this event type is sent into the session by a client
// or worker (user.* / system.*) as opposed to produced by the platform.
func (t EventType) Inbound() bool {
	switch t.Domain() {
	case "user", "system":
		return true
	default:
		return false
	}
}

// Persisted reports whether the event is durably stored in the log. The
// stream-only preview frames (event_start/event_delta) are not.
func (t EventType) Persisted() bool {
	return t != EventStart && t != EventDelta
}

// Event is the stored envelope. Type-specific fields live in Body and are
// flattened onto the wire object (alongside id/type/processed_at) at the API
// boundary — the persisted JSON is a flat object keyed by "type".
type Event struct {
	ID          ID         // sevt_…
	SessionID   ID         // sesn_…
	ThreadID    ID         // optional (multi-agent), zero if none
	Seq         int64      // monotonic per session; (SessionID, Seq) is unique
	Type        EventType  //
	Body        []byte     // type-specific JSON (flattened at the wire boundary)
	ProcessedAt *time.Time // nil = queued, awaiting in-order processing
	CreatedAt   time.Time  //
}

// StopReasonType enumerates why a session went idle.
type StopReasonType string

const (
	StopRequiresAction StopReasonType = "requires_action"
	StopEndTurn        StopReasonType = "end_turn"
)

// StopReason accompanies a session.status_idle event. EventIDs lists the
// blocking tool_use/custom_tool_use events when Type is requires_action.
type StopReason struct {
	Type     StopReasonType `json:"type"`
	EventIDs []ID           `json:"event_ids,omitempty"`
}

// ContentBlock is a single block of message content. v1 handles text; other
// block types (image, etc.) are added as the toolset grows.
type ContentBlock struct {
	Type string `json:"type"` // "text", …
	Text string `json:"text,omitempty"`
}
