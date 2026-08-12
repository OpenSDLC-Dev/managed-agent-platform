package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// inDeny builds an un-appended user.tool_confirmation denying ref. An empty msg
// omits deny_message entirely, which is what a client sends when it refuses
// without giving a reason.
func inDeny(ref, msg string) events.NewEvent {
	if msg == "" {
		return inRaw(domain.EventUserToolConfirm,
			fmt.Sprintf(`{"result":"deny","tool_use_id":%q}`, ref))
	}
	return inRaw(domain.EventUserToolConfirm,
		fmt.Sprintf(`{"result":"deny","tool_use_id":%q,"deny_message":%q}`, ref, msg))
}

// payloadOf decodes a synthesized result so its wire fields can be asserted one
// by one — including the keys that must be absent.
func payloadOf(t *testing.T, ev events.NewEvent) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(ev.Payload, &obj); err != nil {
		t.Fatalf("result payload %s: %v", ev.Payload, err)
	}
	return obj
}

// TestDenialAnswersInTheDeniedCallsOwnFamily is the shape half of the gate. A
// client watching an MCP tool waits for an agent.mcp_tool_result keyed by
// mcp_tool_use_id; an agent.tool_result keyed by tool_use_id is not an answer it
// will ever see, however correct the database considers it. Nothing else catches
// that: answeredBy COALESCEs the three reference keys, so a wrong-family answer
// marks the call answered, unblocks the gate and replays fine — it is wrong only
// where a client is looking.
func TestDenialAnswersInTheDeniedCallsOwnFamily(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	for _, tc := range []struct {
		use    domain.EventType
		result domain.EventType
		refKey string
	}{
		{domain.EventAgentToolUse, domain.EventAgentToolResult, "tool_use_id"},
		{domain.EventAgentMCPToolUse, domain.EventAgentMCPToolResult, "mcp_tool_use_id"},
	} {
		t.Run(string(tc.use), func(t *testing.T) {
			sid := newSession(t, pool)
			id := toolUse(t, log, sid, tc.use, `"ask"`)

			results, denied, err := events.DenialResults(ctx, pool, sid,
				[]events.NewEvent{inDeny(id.String(), "not allowed")})
			if err != nil {
				t.Fatalf("DenialResults: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %v, want one", results)
			}
			if results[0].Type != tc.result {
				t.Errorf("result type = %s, want %s", results[0].Type, tc.result)
			}
			obj := payloadOf(t, results[0])
			if obj[tc.refKey] != id.String() {
				t.Errorf("%s = %v, want %s", tc.refKey, obj[tc.refKey], id)
			}
			// Exactly one reference key: a payload carrying two would answer the
			// call under whichever the reader happens to check first.
			for _, other := range []string{"tool_use_id", "custom_tool_use_id", "mcp_tool_use_id"} {
				if other == tc.refKey {
					continue
				}
				if _, set := obj[other]; set {
					t.Errorf("payload also carries %s: %s", other, results[0].Payload)
				}
			}
			// agent.mcp_tool_result and agent.tool_result both lack a
			// session_thread_id on the wire (verified against the SDK's event
			// types), so writing one would invent a field.
			if _, set := obj["session_thread_id"]; set {
				t.Errorf("payload carries session_thread_id: %s", results[0].Payload)
			}
			if obj["is_error"] != true {
				t.Errorf("is_error = %v, want true", obj["is_error"])
			}
			content, ok := obj["content"].([]any)
			if !ok || len(content) != 1 {
				t.Fatalf("content = %v, want one block", obj["content"])
			}
			if block := content[0].(map[string]any); block["type"] != "text" || block["text"] != "not allowed" {
				t.Errorf("content block = %v, want the deny_message as text", block)
			}
			if len(denied) != 1 || denied[0] != id.String() {
				t.Errorf("denied = %v, want [%s]", denied, id)
			}
			// Never stamped here: both families are answered by an agent.* event,
			// which the store stamps at insert. Stamping at synthesis instead
			// would record a time before the transaction that holds it committed.
			if results[0].ProcessedAt != nil {
				t.Errorf("ProcessedAt = %v, want nil (the store stamps an outbound type)", results[0].ProcessedAt)
			}
		})
	}
}

// TestDenialWithoutAMessageStillCarriesText pins the default. A Messages
// endpoint rejects an empty text block, and the denial is replayed into every
// later request the brain assembles, so an empty deny_message must not become an
// empty block.
func TestDenialWithoutAMessageStillCarriesText(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)
	id := toolUse(t, log, sid, domain.EventAgentMCPToolUse, `"ask"`)

	results, _, err := events.DenialResults(ctx, pool, sid, []events.NewEvent{inDeny(id.String(), "")})
	if err != nil {
		t.Fatalf("DenialResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	block := payloadOf(t, results[0])["content"].([]any)[0].(map[string]any)
	if block["text"] == "" || block["text"] == nil {
		t.Errorf("content block = %v, want a non-empty default text", block)
	}
}

// TestDenialTakesOnlyTheDeniedConfirmations: an allow is the client's approval
// and is answered by running the tool, not by a result; anything that is not a
// confirmation at all is another event in the same batch.
func TestDenialTakesOnlyTheDeniedConfirmations(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)
	allowed := toolUse(t, log, sid, domain.EventAgentToolUse, `"ask"`)
	denied := toolUse(t, log, sid, domain.EventAgentMCPToolUse, `"ask"`)

	batch := []events.NewEvent{
		inRaw(domain.EventUserMessage, `{"content":[]}`),
		inRaw(domain.EventUserToolConfirm, fmt.Sprintf(`{"result":"allow","tool_use_id":%q}`, allowed)),
		inDeny(denied.String(), "no"),
	}
	results, deniedIDs, err := events.DenialResults(ctx, pool, sid, batch)
	if err != nil {
		t.Fatalf("DenialResults: %v", err)
	}
	if len(results) != 1 || results[0].Type != domain.EventAgentMCPToolResult {
		t.Fatalf("results = %v, want just the MCP denial", results)
	}
	if len(deniedIDs) != 1 || deniedIDs[0] != denied.String() {
		t.Errorf("denied = %v, want [%s]", deniedIDs, denied)
	}
}

// TestDenialsFollowBatchOrder: the results are appended to the log in the order
// they come back, and a client reading the stream sees them against its own
// confirmations. Mixed families in one batch are the case that would expose a
// shared buffer or a map iteration.
func TestDenialsFollowBatchOrder(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)
	first := toolUse(t, log, sid, domain.EventAgentMCPToolUse, `"ask"`)
	second := toolUse(t, log, sid, domain.EventAgentToolUse, `"ask"`)
	third := toolUse(t, log, sid, domain.EventAgentMCPToolUse, `"ask"`)

	results, denied, err := events.DenialResults(ctx, pool, sid, []events.NewEvent{
		inDeny(first.String(), "a"), inDeny(second.String(), "b"), inDeny(third.String(), "c"),
	})
	if err != nil {
		t.Fatalf("DenialResults: %v", err)
	}
	wantTypes := []domain.EventType{
		domain.EventAgentMCPToolResult, domain.EventAgentToolResult, domain.EventAgentMCPToolResult,
	}
	wantText := []string{"a", "b", "c"}
	wantIDs := []string{first.String(), second.String(), third.String()}
	if len(results) != 3 {
		t.Fatalf("results = %v, want three", results)
	}
	for i, ev := range results {
		if ev.Type != wantTypes[i] {
			t.Errorf("results[%d] type = %s, want %s", i, ev.Type, wantTypes[i])
		}
		block := payloadOf(t, ev)["content"].([]any)[0].(map[string]any)
		if block["text"] != wantText[i] {
			t.Errorf("results[%d] text = %v, want %q", i, block["text"], wantText[i])
		}
	}
	for i, id := range wantIDs {
		if denied[i] != id {
			t.Errorf("denied[%d] = %s, want %s", i, denied[i], id)
		}
	}
}

// TestDenialRefusesWhatItCannotAnswer covers the two ways the lookup can fail to
// produce a family. Neither is reachable through the API — ValidateToolConfirmations
// runs first and rejects both — so this is the check that keeps them unreachable
// rather than silent: synthesizing nothing for a denial would leave the call
// unanswered on an append-only log, which wedges every later replay.
func TestDenialRefusesWhatItCannotAnswer(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	// The two failures are separate diagnoses and each is asserted by its own
	// wording, not merely by "some error mentioning the id" — that looser
	// assertion passes when either check fires, so it cannot tell an id nothing
	// names from one naming an event of the wrong kind, and a lost check would
	// read as covered.
	t.Run("an id that names nothing", func(t *testing.T) {
		sid := newSession(t, pool)
		_, _, err := events.DenialResults(ctx, pool, sid, []events.NewEvent{inDeny("sevt_nope", "no")})
		wantErrIs(t, err, `denied tool_use_id "sevt_nope" does not name an event in this session`)
	})

	t.Run("an id in another session", func(t *testing.T) {
		a, b := newSession(t, pool), newSession(t, pool)
		id := toolUse(t, log, b, domain.EventAgentMCPToolUse, `"ask"`)
		_, _, err := events.DenialResults(ctx, pool, a, []events.NewEvent{inDeny(id.String(), "no")})
		wantErrIs(t, err, fmt.Sprintf(`denied tool_use_id %q does not name an event in this session`, id))
	})

	t.Run("an event that is not a tool call", func(t *testing.T) {
		sid := newSession(t, pool)
		id := appendEvent(t, log, sid, "", domain.EventUserMessage, `{"content":[]}`)
		_, _, err := events.DenialResults(ctx, pool, sid, []events.NewEvent{inDeny(id.String(), "no")})
		wantErrIs(t, err, fmt.Sprintf(`no result event answers a user.message (%q)`, id))
	})

	t.Run("a payload that is not an object", func(t *testing.T) {
		sid := newSession(t, pool)
		_, _, err := events.DenialResults(ctx, pool, sid,
			[]events.NewEvent{inRaw(domain.EventUserToolConfirm, `"nope"`)})
		if err == nil {
			t.Error("a non-object confirmation payload should error")
		}
	})
}

// TestNoDenialsQueriesNothing: the common batch carries no denial at all, and the
// lookup must not cost a round trip for it. A closed pool is the cheapest proof
// that no query ran.
func TestNoDenialsQueriesNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	sid := newSession(t, pool)
	pool.Close()

	results, denied, err := events.DenialResults(context.Background(), pool, sid,
		[]events.NewEvent{inRaw(domain.EventUserMessage, `{"content":[]}`)})
	if err != nil || results != nil || denied != nil {
		t.Errorf("DenialResults over a batch with no denial = %v, %v, %v; want nil, nil, nil", results, denied, err)
	}
}

// TestDenialQueryErrorsAreWrapped: a dead pool must surface as a query failure,
// never as "this id names nothing" — the diagnosis a client would be told to act
// on.
func TestDenialQueryErrorsAreWrapped(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	id := toolUse(t, log, sid, domain.EventAgentToolUse, `"ask"`)
	pool.Close()

	_, _, err := events.DenialResults(context.Background(), pool, sid,
		[]events.NewEvent{inDeny(id.String(), "no")})
	wantErrHas(t, err, "denied tool uses:")
	if err != nil && (strings.Contains(err.Error(), "does not name") ||
		strings.Contains(err.Error(), "no result event answers")) {
		t.Errorf("a driver failure was reported as a client error: %v", err)
	}
}
