package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Tool-flow checks shared by the control plane's tool-result trigger and the
// brain's settlement. Both sides gate turn scheduling on one definition of an
// outstanding tool call — a tool-use event with no result referencing it —
// so they can never disagree about whether a session is ready to resume. The
// model protocol requires every tool_use answered before the conversation
// continues, which makes these checks correctness, not bookkeeping: resuming
// on a partial result set replays a request the protocol rejects.

// Querier is the slice of pgx shared by pools and transactions, so the
// checks can run inside a caller's transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
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
