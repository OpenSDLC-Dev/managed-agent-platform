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
	// evaluated_permission of "ask" and so be gated on user.tool_confirmation:
	// the two the platform executes itself. The reference keys the confirmation
	// on tool_use_id for both — "The id of the agent.tool_use or
	// agent.mcp_tool_use event this result corresponds to" — so a human
	// approving an MCP call sends what they send for a built-in, and the MCP
	// tool use's own session_thread_id is documented as the value to echo back
	// on the confirmation. Custom tools are client-executed and never gated by
	// the platform: it cannot stop what it does not run.
	//
	// Each entry must have an answer in toolUseAnswer, since a denial has to be
	// answered in the family of the call it refuses; a white-box test holds the
	// two together.
	confirmableToolUseTypes = []string{
		string(domain.EventAgentToolUse),
		string(domain.EventAgentMCPToolUse),
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

// runnableToolUse narrows unansweredToolUse to the calls the platform may run
// now (plan 35 decision 5): evaluated_permission allow — a call stamped with
// none, from before the stamp existed, counts as allowed — or ask with an
// allow confirmation recorded for it, or named by $5 (the allow confirmations
// of a batch validated but not yet inserted, so the API can decide its
// resume before appending). Allow-only, so a deny that is not yet answered
// is never scheduled either. With sibling threads "unanswered" and
// "runnable" part ways — thread B's allow-policy call must not drag thread
// A's gated command into the shared sandbox — so every exec driver, the
// trigger arms that pick its kind and the tool_exec re-arm drain this set.
var runnableToolUse = unansweredToolUse + `
		     AND (COALESCE(tu.payload->>'evaluated_permission', 'allow') = 'allow'
		          OR tu.id = ANY($5)
		          OR EXISTS (
		            SELECT 1 FROM events c
		            WHERE c.session_id = $1 AND c.type = 'user.tool_confirmation'
		              AND c.payload->>'tool_use_id' = tu.id AND c.payload->>'result' = 'allow'
		          ))`

// threadClause scopes a tool-use predicate to one thread's own rows, bound as
// $5: NULL is the primary's (nullableID).
const threadClause = ` AND tu.thread_id IS NOT DISTINCT FROM $5`

// HasUnansweredToolUse reports whether any tool-use event in the session
// still lacks a matching result. extraRefs are treated as answered: the ids
// referenced by results that are validated but not yet inserted, so the API
// trigger can decide its batch before appending it.
func HasUnansweredToolUse(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) (bool, error) {
	return hasUnansweredToolUse(ctx, q, sessionID, toolUseTypes, extraRefs)
}

// HasUnansweredThreadToolUse is HasUnansweredToolUse over one thread's own
// rows (empty threadID = the primary's): whether that thread's turn may
// resume, which a sibling's outstanding call does not decide.
func HasUnansweredThreadToolUse(ctx context.Context, q Querier, sessionID, threadID domain.ID, extraRefs []string) (bool, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	var unanswered bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events tu WHERE`+unansweredToolUse+threadClause+`)`,
		sessionID.String(), toolUseTypes, toolResultTypes, extraRefs, nullableID(threadID)).Scan(&unanswered)
	if err != nil {
		return false, fmt.Errorf("unanswered thread tool_use check: %w", err)
	}
	return unanswered, nil
}

// ToolUseRef names one outstanding tool call: the tool-use event's id and its
// type, which together decide the shape of the result that answers it, and
// the thread it was written on (empty for the primary) with whether it was
// cross-posted — so its answer lands on the same surfaces.
type ToolUseRef struct {
	ID          string
	Type        domain.EventType
	ThreadID    domain.ID
	CrossPosted bool
}

// UnansweredToolUses lists, in log order, the tool calls HasUnansweredToolUse
// only counts — the set a user.interrupt has to answer before the session can be
// resumed, since the model protocol requires every tool_use answered and the log
// is append-only. extraRefs are treated as answered, exactly as above.
func UnansweredToolUses(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) ([]ToolUseRef, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	return scanToolUseRefs(q.Query(ctx,
		`SELECT tu.id, tu.type, COALESCE(tu.thread_id, ''), tu.cross_posted FROM events tu WHERE`+unansweredToolUse+` ORDER BY tu.seq`,
		sessionID.String(), toolUseTypes, toolResultTypes, extraRefs))
}

// UnansweredThreadToolUses is UnansweredToolUses for one thread's own rows
// (empty threadID = the primary's) — what a thread-scoped interrupt answers,
// and what archiving a child answers before it terminates. extraRefs as
// above.
func UnansweredThreadToolUses(ctx context.Context, q Querier, sessionID, threadID domain.ID, extraRefs []string) ([]ToolUseRef, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	return scanToolUseRefs(q.Query(ctx,
		`SELECT tu.id, tu.type, COALESCE(tu.thread_id, ''), tu.cross_posted FROM events tu WHERE`+unansweredToolUse+threadClause+` ORDER BY tu.seq`,
		sessionID.String(), toolUseTypes, toolResultTypes, extraRefs, nullableID(threadID)))
}

func scanToolUseRefs(rows pgx.Rows, err error) ([]ToolUseRef, error) {
	if err != nil {
		return nil, fmt.Errorf("unanswered tool_use list: %w", err)
	}
	defer rows.Close()
	var out []ToolUseRef
	for rows.Next() {
		var ref ToolUseRef
		var thread string
		if err := rows.Scan(&ref.ID, &ref.Type, &thread, &ref.CrossPosted); err != nil {
			return nil, err
		}
		ref.ThreadID = domain.ID(thread)
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

// HasRunnableMCPToolUse reports whether any MCP call still lacks a result.
// Every settlement asks it first, because the answer decides a work kind no
// other driver can serve: only the platform's MCP driver answers an
// agent.mcp_tool_use — a client may post neither the call nor its result, and a
// BYOC worker's contract has no MCP surface at all — so a session left with one
// outstanding and no mcp_exec queued waits forever. Asked over the runnable
// set (plan 35 decision 5): a gated MCP call still awaiting its human is
// nothing for the driver to run. extraAllowed are the ids a batch's
// validated-but-not-yet-inserted allow confirmations release.
func HasRunnableMCPToolUse(ctx context.Context, q Querier, sessionID domain.ID, extraRefs, extraAllowed []string) (bool, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	if extraAllowed == nil {
		extraAllowed = []string{}
	}
	var runnable bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events tu WHERE`+runnableToolUse+`)`,
		sessionID.String(), []string{string(domain.EventAgentMCPToolUse)}, toolResultTypes, extraRefs, extraAllowed).Scan(&runnable)
	if err != nil {
		return false, fmt.Errorf("runnable mcp tool_use check: %w", err)
	}
	return runnable, nil
}

// UnansweredPlatformToolNames lists, in log order, the tool names of the
// platform built-in calls HasUnansweredPlatformToolUse counts. The name is
// what routes the work: web_fetch/web_search run in the executor's own
// process as web_exec while the other built-ins ride tool_exec to a sandbox
// (docs/plan/15_web-tools.md), so a resume trigger picks its work kind from
// this list. extraRefs are treated as answered, exactly as above.
func UnansweredPlatformToolNames(ctx context.Context, q Querier, sessionID domain.ID, extraRefs []string) ([]string, error) {
	return platformToolNames(ctx, q, sessionID, unansweredToolUse, extraRefs)
}

// RunnablePlatformToolNames is UnansweredPlatformToolNames over the runnable
// set — what the exec enqueue sites route on, so an ask-gated sibling call
// never schedules (or re-arms) a driver that may not run it. extraAllowed as
// in HasRunnableMCPToolUse.
func RunnablePlatformToolNames(ctx context.Context, q Querier, sessionID domain.ID, extraRefs, extraAllowed []string) ([]string, error) {
	if extraAllowed == nil {
		extraAllowed = []string{}
	}
	return platformToolNames(ctx, q, sessionID, runnableToolUse, extraRefs, extraAllowed)
}

func platformToolNames(ctx context.Context, q Querier, sessionID domain.ID, predicate string, extraRefs []string, more ...any) ([]string, error) {
	if extraRefs == nil {
		extraRefs = []string{}
	}
	args := append([]any{sessionID.String(), []string{string(domain.EventAgentToolUse)}, toolResultTypes, extraRefs}, more...)
	rows, err := q.Query(ctx,
		`SELECT COALESCE(tu.payload->>'name', '') FROM events tu WHERE`+predicate+` ORDER BY tu.seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("platform tool names: %w", err)
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

// ExecClass is the family of exec work the session's runnable calls still
// need, in the precedence every settlement shares: MCP first — only the
// platform's MCP driver answers an agent.mcp_tool_use, and a tool_exec is
// the one kind a BYOC worker claims, so it must never be handed a log with
// one outstanding; then web — a worker's toolset has no web tools; then the
// sandbox built-ins. The queue kind is the caller's to name (the events
// package does not know the queue).
type ExecClass int

const (
	ExecNone ExecClass = iota
	ExecMCP
	ExecWeb
	ExecTool
)

// RunnableExecClass reports which exec family the session's runnable calls
// call for next (plan 35 decision 5). isWebTool tells the web built-ins from
// the sandbox ones (the API injects toolset.IsWebTool); answered are the ids
// the caller's batch already answers, extraAllowed the ones its allow
// confirmations release.
func RunnableExecClass(ctx context.Context, q Querier, sessionID domain.ID, answered, extraAllowed []string, isWebTool func(string) bool) (ExecClass, error) {
	mcp, err := HasRunnableMCPToolUse(ctx, q, sessionID, answered, extraAllowed)
	if err != nil {
		return ExecNone, err
	}
	if mcp {
		return ExecMCP, nil
	}
	names, err := RunnablePlatformToolNames(ctx, q, sessionID, answered, extraAllowed)
	if err != nil {
		return ExecNone, err
	}
	if len(names) == 0 {
		return ExecNone, nil
	}
	for _, name := range names {
		if isWebTool(name) {
			return ExecWeb, nil
		}
	}
	return ExecTool, nil
}

// RunnableToolUse is one call an exec driver runs: the use event's id and
// payload (name and input, or the MCP server and tool), its thread (empty
// for the primary) and whether it was cross-posted — the result answering it
// is written on the same thread and cross-posted alike.
type RunnableToolUse struct {
	ID          domain.ID
	Payload     json.RawMessage
	ThreadID    domain.ID
	CrossPosted bool
}

// RunnableToolUses lists, in log order, the runnable calls of one tool-use
// type — the set a driver drains, across every thread, and re-scans before
// it completes its item so a call committed under the live item is never
// stranded.
func RunnableToolUses(ctx context.Context, q Querier, sessionID domain.ID, useType domain.EventType) ([]RunnableToolUse, error) {
	rows, err := q.Query(ctx,
		`SELECT tu.id, tu.payload, COALESCE(tu.thread_id, ''), tu.cross_posted FROM events tu WHERE`+runnableToolUse+` ORDER BY tu.seq`,
		sessionID.String(), []string{string(useType)}, toolResultTypes, []string{}, []string{})
	if err != nil {
		return nil, fmt.Errorf("runnable tool uses: %w", err)
	}
	defer rows.Close()
	var out []RunnableToolUse
	for rows.Next() {
		var u RunnableToolUse
		var id, thread string
		if err := rows.Scan(&id, &u.Payload, &thread, &u.CrossPosted); err != nil {
			return nil, err
		}
		u.ID, u.ThreadID = domain.ID(id), domain.ID(thread)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Answered reports whether a result already references the tool use — the
// drivers' answered-means-cancelled check (decision 9): a thread-scoped
// interrupt answers the thread's calls itself, so a call found answered just
// before it runs is skipped, and one answered mid-run is cancelled.
func Answered(ctx context.Context, q Querier, sessionID domain.ID, useID domain.ID) (bool, error) {
	var answered bool
	err := q.QueryRow(ctx,
		`SELECT `+answeredBy(3)+` FROM events tu WHERE tu.session_id = $1 AND tu.id = $2`,
		sessionID.String(), useID.String(), toolResultTypes).Scan(&answered)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("answered check: %w", err)
	}
	return answered, nil
}

// AnsweredSet is Answered over many ids in one query: the subset of ids that
// some result already answers.
func AnsweredSet(ctx context.Context, q Querier, sessionID domain.ID, ids []domain.ID) (map[domain.ID]bool, error) {
	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			strs = append(strs, id.String())
		}
	}
	out := map[domain.ID]bool{}
	if len(strs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		`SELECT tu.id FROM events tu WHERE tu.session_id = $1 AND tu.id = ANY($2) AND `+answeredBy(3),
		sessionID.String(), strs, toolResultTypes)
	if err != nil {
		return nil, fmt.Errorf("answered check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[domain.ID(id)] = true
	}
	return out, rows.Err()
}

// ResumableThreads lists the session's running threads whose own calls are
// all answered — the threads a driver's pass has brought to the point of
// their next model_turn (decision 5: the executor wakes per thread as each
// thread's calls become answered). The primary is the empty id, and first. A
// thread's running status is the gate: an idle thread has a human verdict or
// a message outstanding, not a tool. A session from before the thread
// resource, with no rows, is its primary alone at the session's status.
func ResumableThreads(ctx context.Context, q Querier, sessionID domain.ID) ([]domain.ID, error) {
	rows, err := q.Query(ctx,
		`WITH threads AS (
		   SELECT CASE WHEN parent_thread_id IS NULL THEN '' ELSE id END AS id, status, created_at
		     FROM session_threads WHERE session_id = $1 AND archived_at IS NULL
		   UNION ALL
		   SELECT '', s.status, s.created_at FROM sessions s
		    WHERE s.id = $1 AND NOT EXISTS (SELECT 1 FROM session_threads WHERE session_id = $1)
		 )
		 SELECT t.id FROM threads t
		  WHERE t.status = 'running'
		    AND NOT EXISTS (
		      SELECT 1 FROM events tu WHERE`+unansweredToolUse+`
		        AND tu.thread_id IS NOT DISTINCT FROM NULLIF(t.id, '')
		    )
		  ORDER BY t.id = '' DESC, t.created_at, t.id`,
		sessionID.String(), toolUseTypes, toolResultTypes, []string{})
	if err != nil {
		return nil, fmt.Errorf("resumable threads: %w", err)
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.ID(id))
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
	return unconfirmedAskEvents(ctx, q, sessionID, extraConfirmed, "")
}

// UnconfirmedThreadAskEvents is UnconfirmedAskEvents over one thread's own
// rows (empty threadID = the primary's): the gate that holds that thread,
// which a sibling's ask does not.
func UnconfirmedThreadAskEvents(ctx context.Context, q Querier, sessionID, threadID domain.ID, extraConfirmed []string) ([]string, error) {
	return unconfirmedAskEvents(ctx, q, sessionID, extraConfirmed, ` AND tu.thread_id IS NOT DISTINCT FROM $7`, nullableID(threadID))
}

func unconfirmedAskEvents(ctx context.Context, q Querier, sessionID domain.ID, extraConfirmed []string, scope string, scopeArgs ...any) ([]string, error) {
	if extraConfirmed == nil {
		extraConfirmed = []string{}
	}
	args := append([]any{sessionID.String(), confirmableToolUseTypes, string(domain.EvalPermAsk),
		extraConfirmed, string(domain.EventUserToolConfirm), toolResultTypes}, scopeArgs...)
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
		   AND NOT `+answeredBy(6)+scope+`
		 ORDER BY tu.seq`, args...)
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

// toolUseAnswer maps a tool-use event type to the result event that answers it
// when the platform has to answer it itself — a user.interrupt abandoning the
// call, or a human denying it — and to the field that names the call. The
// families have to match: a custom tool's result is a user.custom_tool_result
// keyed by custom_tool_use_id, an MCP tool's an agent.mcp_tool_result keyed by
// mcp_tool_use_id, and a result of the wrong shape is not the answer a client
// watching that tool's family is waiting for. Nothing else catches that — the
// answered-ness queries COALESCE the three reference keys, so a wrong-family
// answer unblocks the gate and replays cleanly.
//
// thread marks the one shape whose wire object carries a session_thread_id; the
// two agent.* results have no such field (verified against the SDK's event
// types).
var toolUseAnswer = map[domain.EventType]struct {
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
// It lives here beside DenialResults, rather than beside the control plane's
// confirmation handling, because what both encode is event-shape knowledge this
// package already owns — which result type answers which use type, under which
// reference key — and one definition is what keeps them from drifting apart.
//
// That an interrupt answers the calls at all, and in this shape, is an inference:
// the reference documents the interrupt's stop reason, not what it writes for
// the calls it abandons (docs/DIVERGENCES.md).
func InterruptResults(uses []ToolUseRef) ([]NewEvent, error) {
	now := time.Now().UTC()
	out := make([]NewEvent, 0, len(uses))
	for _, use := range uses {
		answer, ok := toolUseAnswer[use.Type]
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
		out = append(out, NewEvent{Type: answer.result, Payload: payload, ProcessedAt: &now,
			ThreadID: use.ThreadID, CrossPosted: use.CrossPosted})
	}
	return out, nil
}

// DenialResultText is what a refused call is answered with when the client gives
// no deny_message. Never an empty text block, for the reason InterruptResultText
// is not: a Messages endpoint rejects one, and the denial is replayed into every
// later request this session assembles.
const DenialResultText = "The user declined this tool call."

// DenialResults answers each tool call a batch's user.tool_confirmation events
// refuse, with an error result carrying the client's deny_message. The model
// protocol requires every tool_use answered before the turn resumes, so a denied
// call must have a result or the next replay is a request the model rejects. It
// also returns the ids it answered, which the caller passes on as already-answered
// to the queries that decide what work the resume schedules.
//
// The result is written in the family of the call that was refused — the same
// mapping an interrupt answers under, for the same reason. The family is not in
// the confirmation, which names its call by tool_use_id whichever kind it is, so
// it is read from the log: one query for the whole batch, since a batch may deny
// a built-in and an MCP call at once and each needs its own shape.
//
// Nothing is stamped processed_at here, unlike InterruptResults: every confirmable
// family is answered by an agent.* event, which the store stamps as it inserts.
// The white-box table test is what keeps that true.
//
// The denial's result shape is an inference: the reference documents the
// confirmation event, not the result a denial produces (docs/DIVERGENCES.md).
func DenialResults(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent) ([]NewEvent, []string, error) {
	type denial struct{ id, msg string }
	var denials []denial
	for _, ev := range evs {
		if ev.Type != domain.EventUserToolConfirm {
			continue
		}
		var c struct {
			Result      string `json:"result"`
			ToolUseID   string `json:"tool_use_id"`
			DenyMessage string `json:"deny_message"`
		}
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, nil, err
		}
		if c.Result != "deny" {
			continue
		}
		msg := c.DenyMessage
		if msg == "" {
			msg = DenialResultText
		}
		denials = append(denials, denial{c.ToolUseID, msg})
	}
	if len(denials) == 0 {
		return nil, nil, nil
	}

	ids := make([]string, len(denials))
	for i, d := range denials {
		ids[i] = d.id
	}
	uses, err := toolUsesByID(ctx, q, sessionID, ids)
	if err != nil {
		return nil, nil, err
	}

	results := make([]NewEvent, 0, len(denials))
	answered := make([]string, 0, len(denials))
	for _, d := range denials {
		use, found := uses[d.id]
		typ := use.Type
		if !found {
			// Unreachable through the API: ValidateToolConfirmations rejects a
			// confirmation naming nothing in this session before the batch gets
			// here. Refusing rather than skipping is what keeps it unreachable —
			// a denial that synthesized no result would leave the call unanswered
			// on an append-only log, which is the wedge this function prevents.
			return nil, nil, fmt.Errorf("denied tool_use_id %q does not name an event in this session", d.id)
		}
		answer, ok := toolUseAnswer[typ]
		if !ok {
			return nil, nil, fmt.Errorf("no result event answers a %s (%q)", typ, d.id)
		}
		payload, err := json.Marshal(map[string]any{
			answer.refKey: d.id,
			"content":     []map[string]any{{"type": "text", "text": d.msg}},
			"is_error":    true,
		})
		if err != nil {
			return nil, nil, err
		}
		// On the refused call's thread, cross-posted as it was: the denial
		// answers a cross-posted ask on the session view too.
		results = append(results, NewEvent{Type: answer.result, Payload: payload,
			ThreadID: use.ThreadID, CrossPosted: use.CrossPosted})
		answered = append(answered, d.id)
	}
	return results, answered, nil
}

// toolUsesByID reads the event types and threads of ids in one round trip.
// Ids absent from the session are absent from the result rather than an error
// here — the caller decides what a miss means.
func toolUsesByID(ctx context.Context, q Querier, sessionID domain.ID, ids []string) (map[string]ToolUseRef, error) {
	rows, err := q.Query(ctx,
		`SELECT id, type, COALESCE(thread_id, ''), cross_posted FROM events WHERE session_id = $1 AND id = ANY($2)`,
		sessionID.String(), ids)
	if err != nil {
		return nil, fmt.Errorf("denied tool uses: %w", err)
	}
	defer rows.Close()
	out := make(map[string]ToolUseRef, len(ids))
	for rows.Next() {
		var ref ToolUseRef
		var thread string
		if err := rows.Scan(&ref.ID, &ref.Type, &thread, &ref.CrossPosted); err != nil {
			return nil, fmt.Errorf("denied tool uses: %w", err)
		}
		ref.ThreadID = domain.ID(thread)
		out[ref.ID] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("denied tool uses: %w", err)
	}
	return out, nil
}
