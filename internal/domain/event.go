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

	// The agent-to-agent message pair (plan 35 decision 6). _sent is the
	// observability event on the sender's own stream and _received the delivery
	// written to the target thread's input stream, so one message is two rows,
	// one per thread. Each names its peer thread in a field of its own —
	// to_session_thread_id and from_session_thread_id — rather than in the
	// envelope's session_thread_id, which neither carries; the agent name beside
	// it is null when the peer is the primary agent.
	EventAgentThreadMessageSent     EventType = "agent.thread_message_sent"
	EventAgentThreadMessageReceived EventType = "agent.thread_message_received"
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

// Session-thread lifecycle events (plan 35). The four status events mirror
// session.status_* per thread — emitted on the thread's own stream and, for a
// child, cross-posted to the primary's; the primary thread's accompany every
// session.status_running/_idle/_rescheduled on every session (decision 12),
// thread event first — never _terminated, which the primary does not emit (a
// child terminates; the primary ends with its session). Each carries
// {session_thread_id, agent_name}; _idle adds stop_reason.
const (
	EventSessionThreadCreated           EventType = "session.thread_created"
	EventSessionThreadStatusRunning     EventType = "session.thread_status_running"
	EventSessionThreadStatusIdle        EventType = "session.thread_status_idle"
	EventSessionThreadStatusRescheduled EventType = "session.thread_status_rescheduled"
	EventSessionThreadStatusTerminated  EventType = "session.thread_status_terminated"
)

// Span (observability) events. These are emitted from the same instrumentation
// point as the OTel spans so the two never drift.
const (
	EventSpanModelRequestStart EventType = "span.model_request_start"
	EventSpanModelRequestEnd   EventType = "span.model_request_end" // carries model_usage

	// The outcome-evaluation cycle trio (plan 21). _end carries the verdict
	// and the grader call's usage; _ongoing is the liveness heartbeat.
	EventSpanOutcomeEvalStart   EventType = "span.outcome_evaluation_start"
	EventSpanOutcomeEvalOngoing EventType = "span.outcome_evaluation_ongoing"
	EventSpanOutcomeEvalEnd     EventType = "span.outcome_evaluation_end"
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

// StartsNewWork reports whether this event is somebody outside the session
// asking it to do something new — the narrower sibling of Inbound, and the
// reset for the session delegation bound (#447).
//
// Inbound cannot serve: it is true for user.tool_result, and on a self_hosted
// environment every single tool call comes back as a client POST, so a bound
// reset by Inbound would be zeroed continuously and bound nothing. The split is
// demand versus answer. A message, an outcome and an operator's system message
// are demands. A tool result answers a call the session itself made, so it
// carries the session's own work forward rather than starting new work.
//
// user.tool_confirmation is a demand for a reason beyond "a human is present":
// it is absent from the brain's pendingInputTypes, so without it a human who
// has just approved a gated call would see the very next turn refused.
//
// user.interrupt is excluded deliberately: a stop is not a demand, and counting
// it would let an operator's own stop action hand the session a fresh budget.
func (t EventType) StartsNewWork() bool {
	switch t {
	case EventUserMessage, EventUserDefineOutcome, EventUserToolConfirm, EventSystemMessage:
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
	ThreadID    ID         // the emitting child thread; zero on the primary thread's rows
	Seq         int64      // monotonic per session; (SessionID, Seq) is unique
	Type        EventType  //
	Body        []byte     // type-specific JSON (flattened at the wire boundary)
	ProcessedAt *time.Time // nil = queued, awaiting in-order processing
	CreatedAt   time.Time  //
}

// StopReasonType enumerates why a session went idle.
type StopReasonType string

const (
	StopRequiresAction   StopReasonType = "requires_action"
	StopEndTurn          StopReasonType = "end_turn"
	StopRetriesExhausted StopReasonType = "retries_exhausted"
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

// SearchResultBlock is one web_search hit as an agent.tool_result carries it —
// the SDK's BetaManagedAgentsSearchResultBlock, field for field: the citation
// settings, the hit's text content, the source URL, the page title, and type
// "search_result". Every field is required on the wire, so none is omitempty.
type SearchResultBlock struct {
	Type      string                `json:"type"` // always "search_result"
	Citations SearchResultCitations `json:"citations"`
	Content   []ContentBlock        `json:"content"`
	Source    string                `json:"source"`
	Title     string                `json:"title"`
}

// SearchResultCitations mirrors BetaManagedAgentsSearchResultCitations.
type SearchResultCitations struct {
	Enabled bool `json:"enabled"`
}

// ModelUsage is the token accounting attached to a span.model_request_end
// event (wire field model_usage). All counters are always present on the
// wire; speed is nullable ("standard" | "fast").
type ModelUsage struct {
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	Speed                    *string `json:"speed"`
}
