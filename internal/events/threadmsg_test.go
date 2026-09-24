package events_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// The peer-name rule the reference was recorded keeping (2026-09-02, all eight
// agent.thread_message_* events of two coordinator sessions; #675): a received
// row always names its sender, the coordinator included, and a sent row names
// its target only when the target is a child — a child's message to its
// coordinator carries no to_agent_name key at all, not a null. The omission is
// keyed on the target being the primary thread, not on a name being absent, so
// a named coordinator peer is omitted all the same.
func TestThreadMessageNamesTheSenderAndOnlyAChildTarget(t *testing.T) {
	const sid = domain.ID("sesn_abc")
	coordinator := events.ThreadPeer{AgentName: "planner"}
	child := events.ThreadPeer{ThreadID: "sthr_child", AgentName: "researcher"}
	for _, tc := range []struct {
		name           string
		from, to       events.ThreadPeer
		sent, received map[string]any
	}{
		{
			"coordinator to child", coordinator, child,
			map[string]any{"to_session_thread_id": "sthr_child", "to_agent_name": "researcher"},
			map[string]any{"from_session_thread_id": "sthr_abc", "from_agent_name": "planner"},
		},
		{
			"child to coordinator", child, coordinator,
			map[string]any{"to_session_thread_id": "sthr_abc"},
			map[string]any{"from_session_thread_id": "sthr_child", "from_agent_name": "researcher"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent, received, err := events.ThreadMessage(sid, tc.from, tc.to, "hello")
			if err != nil {
				t.Fatalf("ThreadMessage: %v", err)
			}
			for _, half := range []struct {
				ev     events.NewEvent
				typ    domain.EventType
				thread domain.ID
				want   map[string]any
			}{
				{sent, domain.EventAgentThreadMessageSent, tc.from.ThreadID, tc.sent},
				{received, domain.EventAgentThreadMessageReceived, tc.to.ThreadID, tc.received},
			} {
				if half.ev.Type != half.typ || half.ev.ThreadID != half.thread {
					t.Errorf("event = %s on %q, want %s on %q", half.ev.Type, half.ev.ThreadID, half.typ, half.thread)
				}
				var got map[string]any
				if err := json.Unmarshal(half.ev.Payload, &got); err != nil {
					t.Fatalf("%s payload: %v", half.typ, err)
				}
				// The content is the same on both halves and not what this
				// test is about; everything else is compared key for key, so
				// a present null fails as surely as a wrong name.
				delete(got, "content")
				if !reflect.DeepEqual(got, half.want) {
					t.Errorf("%s payload = %v, want %v", half.typ, got, half.want)
				}
			}
		})
	}
}
