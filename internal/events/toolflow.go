package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Tool-flow checks for the control plane's POST /events: a tool result is
// validated against the log before it is accepted, and it schedules the next
// turn only once every outstanding tool call — a tool-use event with no
// result referencing it — has been answered. The model protocol requires
// every tool_use answered before the conversation continues, which makes
// these checks correctness, not bookkeeping: resuming on a partial result
// set replays a request the protocol rejects, and the log is append-only, so
// a bad reference can never be taken back.
//
// The brain does not consult these: a suspended turn's own intents commit
// with its settlement, so nothing can have answered them yet, and it simply
// completes its work item and waits for the trigger above.

// Querier is the slice of pgx shared by pools and transactions, so the
// checks can run inside a caller's transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	toolUseTypes = []string{
		string(domain.EventAgentToolUse),
		string(domain.EventAgentMCPToolUse),
		string(domain.EventAgentCustomToolUse),
	}
	toolResultTypes = []string{
		string(domain.EventUserToolResult),
		string(domain.EventUserCustomToolRes),
		string(domain.EventAgentToolResult),
		string(domain.EventAgentMCPToolResult),
	}
	// confirmableToolUseTypes are the tool-use events that can carry an
	// evaluated_permission of "ask" and so be gated on user.tool_confirmation.
	// v1 gates only platform built-ins (agent.tool_use): the brain stamps a
	// policy on nothing else, and a denial's result is emitted as an
	// agent.tool_result (the wrong shape for an MCP tool, whose result is
	// agent.mcp_tool_result / mcp_tool_use_id). MCP gating is slice-8+ work and
	// must extend the denial synthesis with it. Custom tools are
	// client-executed and never gated by the platform.
	confirmableToolUseTypes = []string{
		string(domain.EventAgentToolUse),
	}
)

// answeredBy renders the one definition of "this tool call has been answered":
// some result event in the same session references the tool-use row aliased tu,
// by whichever of the three reference keys its own shape uses. typesParam is the
// placeholder the caller binds the result-type list to (the session is always
// $1). Four queries ask this question — is anything outstanding, what is, may
// this result land, is this ask still blocking — and a drift between them would
// wedge a session, so they share one text.
func answeredBy(typesParam int) string {
	return fmt.Sprintf(`EXISTS (
		       SELECT 1 FROM events r
		       WHERE r.session_id = $1 AND r.type = ANY($%d)
		         AND COALESCE(r.payload->>'tool_use_id',
		                      r.payload->>'custom_tool_use_id',
		                      r.payload->>'mcp_tool_use_id') = tu.id
		     )`, typesParam)
}

// unansweredToolUse is one outstanding tool call, for the two queries that ask
// about it (does any exist, and which ones): a tool-use event of one of $2's
// types that no result of $3's types answers, and that $4 does not pre-answer.
// Written against the alias tu.
var unansweredToolUse = `
		   tu.session_id = $1 AND tu.type = ANY($2)
		     AND tu.id != ALL($4)
		     AND NOT ` + answeredBy(3)

// HasUnansweredToolUse reports whether any tool-use event in the session
// still lacks a matching result. extraRefs are treated as answered: the ids
// referenced by results that are validated but not yet inserted, so the API
// trigger can decide its batch before appending it.
func HasUnansweredToolUse(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) (bool, error) {
	return hasUnansweredToolUse(ctx, q, sessionID, toolUseTypes, extraRefs)
}

// ToolUseRef names one outstanding tool call: the tool-use event's id and its
// type, which together decide the shape of the result that answers it.
type ToolUseRef struct {
	ID   string
	Type domain.EventType
}

// UnansweredToolUses lists, in log order, the tool calls HasUnansweredToolUse
// only counts — the set a user.interrupt has to answer before the session can be
// resumed, since the model protocol requires every tool_use answered and the log
// is append-only. extraRefs are treated as answered, exactly as above.
func UnansweredToolUses(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) ([]ToolUseRef, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	rows, err := q.Query(ctx,
		`SELECT tu.id, tu.type FROM events tu WHERE`+unansweredToolUse+` ORDER BY tu.seq`,
		sessionID.String(), toolUseTypes, toolResultTypes, extraRefs)
	if err != nil {
		return nil, fmt.Errorf("unanswered tool_use list: %w", err)
	}
	defer rows.Close()
	var out []ToolUseRef
	for rows.Next() {
		var ref ToolUseRef
		if err := rows.Scan(&ref.ID, &ref.Type); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// HasUnansweredPlatformToolUse reports whether any platform-executed built-in
// tool use (agent.tool_use) still lacks a result. The executor runs only these,
// so a confirmation resume enqueues a tool_exec only when one is outstanding: a
// turn whose remaining unanswered tools are all client-executed (custom) has no
// platform work and waits on the client's result instead — enqueuing a tool_exec
// there would provision a sandbox for nothing.
func HasUnansweredPlatformToolUse(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) (bool, error) {
	return hasUnansweredToolUse(ctx, q, sessionID, []string{string(domain.EventAgentToolUse)}, extraRefs)
}

// UnansweredPlatformToolNames lists, in log order, the tool names of the
// platform built-in calls HasUnansweredPlatformToolUse counts. The name is
// what routes the work: web_fetch/web_search run in the executor's own
// process as web_exec while the other built-ins ride tool_exec to a sandbox
// (docs/plan/15_web-tools.md), so a resume trigger picks its work kind from
// this list. extraRefs are treated as answered, exactly as above.
func UnansweredPlatformToolNames(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) ([]string, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	rows, err := q.Query(ctx,
		`SELECT COALESCE(tu.payload->>'name', '') FROM events tu WHERE`+unansweredToolUse+` ORDER BY tu.seq`,
		sessionID.String(), []string{string(domain.EventAgentToolUse)}, toolResultTypes, extraRefs)
	if err != nil {
		return nil, fmt.Errorf("unanswered platform tool names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func hasUnansweredToolUse(ctx context.Context, q Querier, sessionID domain.ID, useTypes, extraRefs []string) (bool, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	var unanswered bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events tu WHERE`+unansweredToolUse+`)`,
		sessionID.String(), useTypes, toolResultTypes, extraRefs).Scan(&unanswered)
	if err != nil {
		return false, fmt.Errorf("unanswered tool_use check: %w", err)
	}
	return unanswered, nil
}

// ToolResultRefs collects the tool-use ids referenced by a batch's inbound
// tool-result events, in batch order.
func ToolResultRefs(evs []NewEvent) []string {
	var refs []string
	for _, ev := range evs {
		if key := resultRefKey(ev.Type); key != "" {
			if ref, err := payloadString(ev.Payload, key); err == nil {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// ValidateToolResults rejects an inbound tool result that does not reference
// an outstanding tool call: the id must name an existing tool-use event of
// the matching kind with no result yet, in the log or earlier in the same
// batch. The log is append-only — one accepted bad reference would poison
// every future replay with a request the model protocol rejects, wedging the
// session permanently.
//
// platformOwned, when non-nil, names the built-in tools only the platform may
// answer (the API injects toolset.IsWebTool): a client result for such a call
// is rejected even while it is still unanswered, closing the double-answer
// window between the executor's web scan and its commit (#222,
// docs/plan/16_one-answer-per-tool-call.md). It must never cover the sandbox
// six — a self_hosted worker answering those via user.tool_result is the
// BYOC pull protocol.
func ValidateToolResults(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent, platformOwned func(name string) bool) error {
	seen := map[string]bool{}
	for i, ev := range evs {
		refKey := resultRefKey(ev.Type)
		if refKey == "" {
			continue
		}
		wantUse := domain.EventAgentToolUse
		if ev.Type == domain.EventUserCustomToolRes {
			wantUse = domain.EventAgentCustomToolUse
		}
		ref, err := payloadString(ev.Payload, refKey)
		if err != nil {
			return fmt.Errorf("events[%d]: %w", i, err)
		}
		if seen[ref] {
			return fmt.Errorf("events[%d]: duplicate result for %s %q in one request", i, refKey, ref)
		}
		seen[ref] = true

		var useType, name, perm string
		var answered, confirmed bool
		err = q.QueryRow(ctx,
			`SELECT tu.type,
			        COALESCE(tu.payload->>'name', ''),
			        COALESCE(tu.payload->>'evaluated_permission', ''),
			        `+answeredBy(3)+`,
			        EXISTS (
			          SELECT 1 FROM events c
			          WHERE c.session_id = $1 AND c.type = $4
			            AND c.payload->>'tool_use_id' = tu.id
			        )
			 FROM events tu WHERE tu.session_id = $1 AND tu.id = $2`,
			sessionID.String(), ref, toolResultTypes, string(domain.EventUserToolConfirm)).Scan(&useType, &name, &perm, &answered, &confirmed)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("events[%d]: %s %q does not name a tool use in this session", i, refKey, ref)
		}
		if err != nil {
			return fmt.Errorf("validate tool result: %w", err)
		}
		if domain.EventType(useType) != wantUse {
			return fmt.Errorf("events[%d]: %s %q references a %s event, not %s", i, refKey, ref, useType, wantUse)
		}
		if answered {
			return fmt.Errorf("events[%d]: tool use %q already has a result", i, ref)
		}
		// A platform-owned call is never the client's to answer, answered or
		// not: while the executor's web pass runs between its scan and its
		// commit, the call is still unanswered and every other arm here would
		// wave the client's result through — the second answer then commits
		// with the executor's settlement (#222).
		if platformOwned != nil && wantUse == domain.EventAgentToolUse && platformOwned(name) {
			return fmt.Errorf("events[%d]: tool use %q (%s) is platform-executed and cannot be answered by a client result", i, ref, name)
		}
		// An ask-gated tool must be confirmed before any result answers it: a
		// premature result would bypass the human approval and, on a later
		// denial, leave the tool use double-answered on the append-only log.
		if perm == string(domain.EvalPermAsk) && !confirmed {
			return fmt.Errorf("events[%d]: tool use %q is awaiting confirmation and cannot be answered yet", i, ref)
		}
	}
	return nil
}

// ToolConfirmationRefs collects the tool-use ids a batch's
// user.tool_confirmation events resolve, in batch order.
func ToolConfirmationRefs(evs []NewEvent) []string {
	var refs []string
	for _, ev := range evs {
		if ev.Type != domain.EventUserToolConfirm {
			continue
		}
		if ref, err := payloadString(ev.Payload, "tool_use_id"); err == nil {
			refs = append(refs, ref)
		}
	}
	return refs
}

// ValidateToolConfirmations rejects an inbound user.tool_confirmation that does
// not name a tool use still awaiting confirmation: the id must reference an
// ask-gated tool-use event (evaluated_permission "ask") in this session that no
// prior confirmation has resolved, and not appear twice in one request. Like a
// tool result, an accepted bad confirmation cannot be taken back from the
// append-only log, so a wrong reference is the client's 400.
func ValidateToolConfirmations(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent) error {
	seen := map[string]bool{}
	for i, ev := range evs {
		if ev.Type != domain.EventUserToolConfirm {
			continue
		}
		ref, err := payloadString(ev.Payload, "tool_use_id")
		if err != nil {
			return fmt.Errorf("events[%d]: %w", i, err)
		}
		if seen[ref] {
			return fmt.Errorf("events[%d]: duplicate confirmation for tool_use_id %q in one request", i, ref)
		}
		seen[ref] = true

		var perm string
		var confirmed, answered bool
		err = q.QueryRow(ctx,
			`SELECT COALESCE(tu.payload->>'evaluated_permission', ''),
			        EXISTS (
			          SELECT 1 FROM events c
			          WHERE c.session_id = $1 AND c.type = $4
			            AND c.payload->>'tool_use_id' = tu.id
			        ),
			        `+answeredBy(5)+`
			 FROM events tu
			 WHERE tu.session_id = $1 AND tu.id = $2 AND tu.type = ANY($3)`,
			sessionID.String(), ref, confirmableToolUseTypes, string(domain.EventUserToolConfirm),
			toolResultTypes).Scan(&perm, &confirmed, &answered)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("events[%d]: tool_use_id %q does not name a tool use in this session", i, ref)
		}
		if err != nil {
			return fmt.Errorf("validate tool confirmation: %w", err)
		}
		if perm != string(domain.EvalPermAsk) {
			return fmt.Errorf("events[%d]: tool use %q was not gated for confirmation", i, ref)
		}
		if confirmed {
			return fmt.Errorf("events[%d]: tool use %q is already confirmed", i, ref)
		}
		// A gated call that already has a result was abandoned by a
		// user.interrupt, which answers everything outstanding without asking
		// anyone. Confirming it now would let a denial synthesize a second
		// result for the same call onto the append-only log — the double-answer
		// ValidateToolResults refuses from the other direction.
		if answered {
			return fmt.Errorf("events[%d]: tool use %q was already answered and can no longer be confirmed", i, ref)
		}
	}
	return nil
}

// UnconfirmedAskEvents returns, in log order, the ids of the session's ask-gated
// tool-use events that no user.tool_confirmation has resolved yet — the set a
// requires_action suspension is still blocked on. extraConfirmed are the ids a
// validated-but-not-yet-inserted confirmation batch resolves, so the API can
// decide its resume before appending: an empty result means every ask is
// answered and the session may run; a non-empty result is the remainder to
// re-emit on session.status_idle.
//
// A gated call that already carries a result is not blocking either, however it
// got one. Only a user.interrupt produces that (it abandons everything
// outstanding without asking anyone), and the gate has to let go of it: an ask
// left "blocked" forever after its call was answered would wedge the session on
// the very resume the interrupt exists to restore.
func UnconfirmedAskEvents(ctx context.Context, q Querier, sessionID domain.ID, extraConfirmed []string) ([]string, error) {
	if extraConfirmed == nil {
		extraConfirmed = []string{}
	}
	rows, err := q.Query(ctx,
		`SELECT tu.id FROM events tu
		 WHERE tu.session_id = $1 AND tu.type = ANY($2)
		   AND tu.payload->>'evaluated_permission' = $3
		   AND tu.id != ALL($4)
		   AND NOT EXISTS (
		     SELECT 1 FROM events c
		     WHERE c.session_id = $1 AND c.type = $5
		       AND c.payload->>'tool_use_id' = tu.id
		   )
		   AND NOT `+answeredBy(6)+`
		 ORDER BY tu.seq`,
		sessionID.String(), confirmableToolUseTypes, string(domain.EvalPermAsk),
		extraConfirmed, string(domain.EventUserToolConfirm), toolResultTypes)
	if err != nil {
		return nil, fmt.Errorf("unconfirmed ask events: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unconfirmed ask events: %w", err)
	}
	return ids, nil
}

func resultRefKey(typ domain.EventType) string {
	switch typ {
	case domain.EventUserToolResult:
		return "tool_use_id"
	case domain.EventUserCustomToolRes:
		return "custom_tool_use_id"
	}
	return ""
}

func payloadString(payload json.RawMessage, key string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(obj[key], &s); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// interruptAnswer maps a tool-use event type to the result event that answers it
// when a user.interrupt abandons the call, and to the field that names the call.
// The families have to match — a custom tool's result is a
// user.custom_tool_result keyed by custom_tool_use_id, an MCP tool's an
// agent.mcp_tool_result keyed by mcp_tool_use_id — for the reason the denial
// synthesis is confined to agent.tool_use (confirmableToolUseTypes above): a
// result of the wrong shape is not the answer a client watching that tool's
// family is waiting for. thread marks the one shape whose wire object carries a
// session_thread_id; the two agent.* results have no such field (verified
// against the SDK's event types).
var interruptAnswer = map[domain.EventType]struct {
	result domain.EventType
	refKey string
	thread bool
}{
	domain.EventAgentToolUse:       {domain.EventAgentToolResult, "tool_use_id", false},
	domain.EventAgentCustomToolUse: {domain.EventUserCustomToolRes, "custom_tool_use_id", true},
	domain.EventAgentMCPToolUse:    {domain.EventAgentMCPToolResult, "mcp_tool_use_id", false},
}

// InterruptResultText is what an abandoned call is answered with. Never an empty
// text block: a Messages endpoint rejects one, and that request is what every
// later replay of this session sends.
const InterruptResultText = "The user interrupted this tool call before it returned a result."

// InterruptResults answers each tool call a user.interrupt abandons — the set
// UnansweredToolUses returns — with an error result. The turn cannot end without
// them: the model protocol requires every tool_use answered before the
// conversation continues, so an abandoned call on the append-only log would make
// every future replay a request the model rejects — the wedge the interrupt
// exists to undo.
//
// The results are stamped processed on the spot. The platform wrote them, and
// the one that lands under an inbound type would otherwise render with a null
// processed_at, indistinguishable from a client event still queued behind
// earlier ones, and would look to a settling brain like input to chain a turn on.
//
// It lives here rather than beside the control plane's denial synthesis because
// what it encodes is event-shape knowledge this package already owns — which
// result type answers which use type, under which reference key — and because
// one definition is what keeps a second caller (the brain's tests drive the same
// trigger) from drifting from the first.
//
// That an interrupt answers the calls at all, and in this shape, is an inference:
// the reference documents the interrupt's stop reason, not what it writes for
// the calls it abandons (docs/DIVERGENCES.md).
func InterruptResults(uses []ToolUseRef) ([]NewEvent, error) {
	now := time.Now().UTC()
	out := make([]NewEvent, 0, len(uses))
	for _, use := range uses {
		answer, ok := interruptAnswer[use.Type]
		if !ok {
			// Unreachable while this map and toolUseTypes agree; the check is
			// what keeps them agreeing, since a new tool-use type with no answer
			// here would strand exactly the call this function exists to release.
			return nil, fmt.Errorf("no result event answers a %s tool use", use.Type)
		}
		fields := map[string]any{
			answer.refKey: use.ID,
			"content":     []map[string]any{{"type": "text", "text": InterruptResultText}},
			"is_error":    true,
		}
		if answer.thread {
			fields["session_thread_id"] = nil
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		out = append(out, NewEvent{Type: answer.result, Payload: payload, ProcessedAt: &now})
	}
	return out, nil
}
