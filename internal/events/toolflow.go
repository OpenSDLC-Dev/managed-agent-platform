package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	// evaluated_permission of "ask" and so be gated on user.tool_confirmation:
	// platform built-ins and MCP tools. Custom tools are client-executed and
	// never gated by the platform.
	confirmableToolUseTypes = []string{
		string(domain.EventAgentToolUse),
		string(domain.EventAgentMCPToolUse),
	}
)

// HasUnansweredToolUse reports whether any tool-use event in the session
// still lacks a matching result. extraRefs are treated as answered: the ids
// referenced by results that are validated but not yet inserted, so the API
// trigger can decide its batch before appending it.
func HasUnansweredToolUse(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) (bool, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	var unanswered bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM events tu
		   WHERE tu.session_id = $1 AND tu.type = ANY($2)
		     AND tu.id != ALL($4)
		     AND NOT EXISTS (
		       SELECT 1 FROM events r
		       WHERE r.session_id = $1 AND r.type = ANY($3)
		         AND COALESCE(r.payload->>'tool_use_id',
		                      r.payload->>'custom_tool_use_id',
		                      r.payload->>'mcp_tool_use_id') = tu.id
		     )
		 )`,
		sessionID.String(), toolUseTypes, toolResultTypes, extraRefs).Scan(&unanswered)
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
func ValidateToolResults(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent) error {
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

		var useType string
		var answered bool
		err = q.QueryRow(ctx,
			`SELECT tu.type,
			        EXISTS (
			          SELECT 1 FROM events r
			          WHERE r.session_id = $1 AND r.type = ANY($3)
			            AND COALESCE(r.payload->>'tool_use_id',
			                         r.payload->>'custom_tool_use_id',
			                         r.payload->>'mcp_tool_use_id') = tu.id
			        )
			 FROM events tu WHERE tu.session_id = $1 AND tu.id = $2`,
			sessionID.String(), ref, toolResultTypes).Scan(&useType, &answered)
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
		var confirmed bool
		err = q.QueryRow(ctx,
			`SELECT COALESCE(tu.payload->>'evaluated_permission', ''),
			        EXISTS (
			          SELECT 1 FROM events c
			          WHERE c.session_id = $1 AND c.type = $4
			            AND c.payload->>'tool_use_id' = tu.id
			        )
			 FROM events tu
			 WHERE tu.session_id = $1 AND tu.id = $2 AND tu.type = ANY($3)`,
			sessionID.String(), ref, confirmableToolUseTypes, string(domain.EventUserToolConfirm)).Scan(&perm, &confirmed)
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
		 ORDER BY tu.seq`,
		sessionID.String(), confirmableToolUseTypes, string(domain.EvalPermAsk),
		extraConfirmed, string(domain.EventUserToolConfirm))
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
