package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	sdk "github.com/anthropics/anthropic-sdk-go"
)

// ToolExecConfig is the sandbox shape a worker provisions for a session's tools.
// A self_hosted environment's wire config carries no image (the sandbox image is
// a deployment choice, not part of the domain model), so Image and Workdir come
// from the worker's own configuration — mirroring the platform executor's
// Config. Networking is the session's egress policy, read from the session's
// environment and threaded in by the caller.
type ToolExecConfig struct {
	Image      string
	Workdir    string
	Networking domain.Networking
	// Hardening is the containment the worker's sandboxes are created with, the
	// BYOC twin of the platform executor's (#65). cmd/worker resolves it from
	// the same environment variables the executor reads, so a customer-hosted
	// sandbox is capped the same way a platform-managed one is.
	Hardening sandbox.Hardening
	// Coordinator is set when the session's snapshot carries a multiagent
	// roster, which the caller reads with its liveness gate. It is the whole of
	// what this thread-unaware driver knows about threads (plan 35 decision 13
	// iv): it widens the scan to the log's start and makes the run re-scan
	// until nothing is left, because concurrent threads can commit calls while
	// this item is live and their enqueue is a no-op against it.
	Coordinator bool
	// Progress, when set, is called each time the run finishes a step — the
	// sandbox provisioned, the skills or files materialized, a tool answered.
	// The lease loop watches it to tell a long run from a wedged one (#383);
	// a caller that runs the driver directly leaves it nil.
	Progress func()
	// AnsweredBeat, when > 0, is how often a call in flight re-checks whether
	// the log has already answered it; otherwise answeredBeat. A test seam —
	// production leaves it 0, because the cadence is not a deployment choice
	// the way the sandbox shape above is.
	AnsweredBeat time.Duration
}

// toolUse is one unanswered agent.tool_use the worker must run: the tool-use
// event's id (which the result references and which scopes the bash shell's
// per-call state), the tool name, and its input.
type toolUse struct {
	id    domain.ID
	name  string
	input json.RawMessage
}

// RunSessionTools is the BYOC worker's tool-exec driver: given a session whose
// turn has suspended for built-in tool calls, it runs every runnable tool call
// in the session's sandbox and posts a user.tool_result for each back through
// the session events API. It is the self_hosted twin of the platform executor's
// per-item processing, with two deployment differences: the transport is HTTP
// (the worker has no database), and the result event is user.tool_result, not
// agent.tool_result — the control plane resumes the brain when a result
// completes the outstanding set, so the worker never enqueues a turn itself.
//
// cfg.Coordinator makes the run multi-pass: on a session whose threads run
// concurrently, one pass is not the item's whole work, because a sibling
// thread's calls can land while this pass runs and its enqueue is a no-op
// against the live item. So the found set is answered and the scan repeated
// until one comes back empty, over the sandbox the first pass provisioned. On a
// single-agent session the driver keeps its single pass exactly, the window
// there being closed by the session's own serialization.
//
// Results are posted per tool as each completes, so a backend fault partway
// through leaves the tools that did run answered on the log; a reclaiming pass
// re-derives only the still-unanswered ones. This matches the executor's
// partial-commit-on-fault: a tool-level failure (missing file, nonzero exit)
// still yields a result the model must see, and only a backend fault (sandbox
// gone) stops the set with the rest left for the reclaim.
//
// The sandbox is provisioned only when there is unanswered work, so a call
// against an already-answered session (a redundant reclaim) is one bounded read
// with nothing to run.
//
// Session liveness is the caller's gate, not this driver's. The platform
// executor refuses to run a stale session's tools by loading its status under
// the session row lock (executor.sessionForRun) before provisioning — but it
// does so in its per-item orchestration, not in its runTools core, which this
// driver is the analog of. The BYOC caller (the lease loop, PR C2b) owns the
// same session load: it must read the session (for the same reason it must load
// the egress policy this cfg.Networking carries) and skip a session that is not
// running or is archived, mirroring sessionForRun. The control plane is only a
// partial backstop here — a post to an archived session is refused (400), but a
// post to a merely not-running one appends without resuming — so the complete
// gate belongs in the caller, not in a reliance on the append being rejected.
func RunSessionTools(ctx context.Context, client sdk.Client, provider sandbox.Provider, sessionID string, cfg ToolExecConfig) error {
	// Called at each step below, so the caller's stall guard measures the run's
	// silence rather than its length (#383). Nil-safe: a caller that does not
	// watch the run passes none. Named report, not progress — the package's
	// progress type is the tracker this ends up feeding.
	report := cfg.Progress
	if report == nil {
		report = func() {}
	}
	// Before the scan, not after the caller: whatever the caller did last — the
	// session liveness read, a wire round trip of its own — ends here, so it does
	// not share a silent interval with the paging scan below. The budget's floor
	// covers neither (#383).
	report()
	uses, err := unansweredToolUses(ctx, client, sessionID, cfg.Coordinator, report)
	if err != nil {
		return err
	}
	if len(uses) == 0 {
		return nil
	}
	// The scan pages over the wire, and provisioning below can pull a cold
	// image: two steps the budget must clear one at a time, not together.
	report()
	sb, err := provider.Provision(ctx, sandbox.Spec{
		SessionID:  domain.ID(sessionID),
		Image:      cfg.Image,
		Workdir:    cfg.Workdir,
		Networking: cfg.Networking,
		Hardening:  cfg.Hardening,
	})
	if err != nil {
		return fmt.Errorf("provision sandbox: %w", err)
	}
	report()
	if err := SetupSkills(ctx, client, sessionID, sb, cfg.Workdir, report); err != nil {
		return err
	}
	report()
	if err := SetupFiles(ctx, client, sessionID, sb, cfg.Workdir, report); err != nil {
		return err
	}
	report()
	runner := toolset.Runner{Sandbox: sb, Session: domain.ID(sessionID), Workdir: cfg.Workdir}
	for {
		for _, u := range uses {
			rctx, stop := answeredWatch(ctx, client, sessionID, u.id, cfg.AnsweredBeat)
			res, err := runner.Run(rctx, u.id, u.name, u.input)
			if stop() {
				// Answered while it ran, so whatever it returned — or failed
				// with — is a late result for a call the log already closed.
				// Skipping is not a fault: the call has its answer, and posting
				// a second one is what the control plane refuses (#441).
				report()
				continue
			}
			if err != nil {
				// Backend fault: stop here. The results posted so far stay answered;
				// this tool and any after it are re-derived on a reclaiming pass.
				return fmt.Errorf("tool %s (%s): %w", u.name, u.id, err)
			}
			// The run and the post are two steps, and reporting only after both puts
			// them in one silent interval — which the floor does not cover. A `bash`
			// call may legitimately take toolset.MaxTimeout, and posting its result is
			// a wire round trip to the control plane with no bound of its own, so the
			// pair can outlast a budget neither half comes close to. The run would then
			// be cancelled *after* its side effects had happened and *before* its
			// result was posted, and the reclaim would run the same command again
			// (#383).
			report()
			if err := postToolResult(ctx, client, sessionID, u.id, res); err != nil {
				return err
			}
			report()
		}
		// A single-agent session's window cannot open: its next turn cannot start
		// before this set's last result lands, so one pass is the whole item and
		// re-scanning would only cost a round trip.
		if !cfg.Coordinator {
			return nil
		}
		// A coordinator's threads run concurrently, so a sibling may have
		// committed calls while the pass above ran — and its enqueue was a no-op
		// against this live item, leaving nobody else to serve them. Re-scan
		// until a pass comes back empty. It terminates on progress: every call
		// a non-empty pass returns ends the pass answered — by this driver, or
		// by whoever answered it under the watch above — and the calls this
		// driver never answers (web, ask-gated, delegation) are filtered out of
		// the found set rather than returned unanswered. The sandbox, skills and
		// files stay one-shot above — wire-expensive, idempotent and
		// session-scoped, so nothing a later pass finds can change them.
		uses, err = unansweredToolUses(ctx, client, sessionID, true, report)
		if err != nil {
			return err
		}
		if len(uses) == 0 {
			return nil
		}
		// The scan's own boundary, so its last page does not share a silent
		// interval with the first tool run of the next pass (#383).
		report()
	}
}

// toolScanPageSize is how many events one page of the scan below requests. A
// single-agent walk reads the trailing turn only, which for a turn of N
// parallel tools is N+1 events on a fresh suspension (the uses plus the
// boundary result) and at most 2N+1 on a reclaim (their answered results are on
// the wire too) — so one page of 20 finishes in a single round trip up to N=19
// fresh, N=9 reclaimed, and anything wider simply pages on. A coordinator's
// walk has no boundary and pages to the log's start, so it reads every one of
// those events whatever the page size — and there the only thing the size
// changes is how many round trips it costs. Hence two sizes: the bounded scan
// keeps 20, where capping the over-read is the cost that matters (tool inputs
// and results carry file contents), and the full walk asks for the largest page
// the wire allows, which is the same 1000 the reference runner requests. Both
// are sent explicitly rather than left to the server's default: the bound is
// this worker's, and it holds against any wire-compatible control plane.
const (
	toolScanPageSize        = 20
	coordinatorScanPageSize = 1000
)

// scanPageSize is the page one walk asks for, by the mode it runs in.
func scanPageSize(coordinator bool) int64 {
	if coordinator {
		return coordinatorScanPageSize
	}
	return toolScanPageSize
}

// unansweredToolUses reads the session's event log over the wire and returns the
// tool calls this worker must run, oldest first: the agent.tool_use events that
// no result answers and no human still owes a verdict on. It mirrors the
// executor's diff exactly. Answered means either an agent.tool_result (a
// platform executor) or a user.tool_result (this worker), both referencing the
// use by tool_use_id, so both count. Runnable narrows that to the calls the
// platform may run now (plan 35 decision 5): evaluated_permission allow — a
// call carrying none, from before the stamp existed, counts as allowed — or
// ask with an allow user.tool_confirmation recorded for it. That narrowing is
// not a multiagent nicety, and so is not narrowed to a coordinator's sessions
// the way the boundary below is: one turn can suspend on two asks and be
// released one at a time on any session at all, and running the sibling a human
// has not answered executes a command nobody approved. This is the third
// expression of one rule — the canonical set is events.HasUnansweredToolUse
// (the SQL the control plane resumes on) and events.RunnableToolUses (which
// every platform driver drains through); the types that answer and the one
// that releases are the shared domain constants below, so a new one must be
// added in all three. A drift here re-runs an answered tool every reclaim,
// re-posting a result the control plane's ValidateToolResults rejects.
//
// Events are parsed from each event's raw wire JSON into a minimal local shape
// rather than the SDK's typed event union: the union tracks the live API's tip
// and carries post-slice surface the worker has no need for, so decoding only
// the six fields this diff needs keeps a schema drift from breaking it.
//
// Cost: the worker has no database, so it cannot ask the executor's one EXISTS
// and there is no unanswered-only wire endpoint to ask instead. On a
// single-agent session it bounds the read by walking newest-first and stopping
// at the trailing turn, rather than paging the session's whole tool history
// (#76). Two platform invariants make that exact for a turn that suspended on
// its tools: the brain commits a turn's tool_use events in ONE append
// (brain.commitTurn), and no later turn's uses reach the log until every
// outstanding one is answered — every enqueue of a model_turn that follows tool
// work is gated on events.HasUnansweredToolUse. So in this stream the unanswered
// set is the newest contiguous run of tool uses, and the first result older than
// that run is the boundary — everything beyond it is answered. A result always
// outranks the use it answers (per-session seq is assigned under the session row
// lock, and a result may only reference a committed use), so walking down, every
// use meets its answer first — and a confirmation likewise outranks the call it
// releases, so the walk has the verdict before it reads the call.
//
// coordinator drops that boundary, because both of its premises hold per thread
// rather than per session (plan 35 decision 13 iv): one thread's turn can
// suspend while a sibling's calls are still outstanding, so a runnable call can
// sit arbitrarily far down the log with an answered pair above it, and no stop
// condition short of the log's start finds it. A coordinator session therefore
// pays a full walk per claimed item — what the reference pays per attach, and
// the price of a thread-unaware worker serving child threads at all. The calls
// it walks are the session view's, which on a self_hosted environment carries
// every child thread's (decision 13 i).
//
// One path used to strand a use outside that run — a turn whose stop reason was
// not tool_use committed its tool_use events but enqueued no tool_exec, and the
// API's user.message-on-idle resume was the one enqueue after tool work not
// gated on the unanswered set — and no bounded scan can find an arbitrarily old
// stranded use, so this scan and the executor's DB-side diff disagreed on that
// shape alone. Both halves are closed (#181): the brain suspends on tool blocks
// whatever stop reason arrives with them, and that resume now gates like the
// others. On any log the current code can produce, the bound is exact and the
// two diffs agree; a log a pre-#181 binary already stranded is the residue
// neither reaches.
func unansweredToolUses(ctx context.Context, client sdk.Client, sessionID string, coordinator bool, progress func()) ([]toolUse, error) {
	iter := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: []string{
			string(domain.EventAgentToolUse),
			string(domain.EventAgentToolResult),
			string(domain.EventUserToolResult),
			string(domain.EventUserToolConfirm),
		},
		Order: sdk.BetaSessionEventListParamsOrderDesc,
		Limit: sdk.Int(scanPageSize(coordinator)),
	})
	answered := map[string]bool{}
	allowed := map[string]bool{}
	var out []toolUse
	var sawUse bool
	var head string // the newest event the walk saw, its anchor for the re-check below
scan:
	for iter.Next() {
		// Per event, because the walk auto-pages: a turn wide enough to span
		// several pages would otherwise spend every one of those round trips
		// inside a single silent step (#383).
		progress()
		var ev struct {
			ID                  string          `json:"id"`
			Type                string          `json:"type"`
			Name                string          `json:"name"`
			Input               json.RawMessage `json:"input"`
			ToolUseID           string          `json:"tool_use_id"`
			EvaluatedPermission string          `json:"evaluated_permission"`
			Result              string          `json:"result"`
		}
		if err := json.Unmarshal([]byte(iter.Current().RawJSON()), &ev); err != nil {
			return nil, fmt.Errorf("parse session event: %w", err)
		}
		if head == "" {
			head = ev.ID
		}
		switch domain.EventType(ev.Type) {
		case domain.EventAgentToolUse:
			sawUse = true
			// A web call (web_fetch/web_search) is never this worker's: it runs
			// in the platform executor's web driver for every environment kind,
			// and the enqueue hold-back keeps a polled item from coexisting with
			// an unanswered one. A delegation call is never anyone's to run: the
			// settlement that emitted it answers it in the same commit. Both
			// filters guard the stray case, so the six-tool Runner is never fed
			// a name it must answer unknown-tool — an answer the control plane
			// would then refuse, faulting the item into a reclaim loop. They
			// still mark sawUse — the call belongs to the trailing turn's run,
			// and the boundary is about turns, not this worker's share.
			if toolset.IsWebTool(ev.Name) || toolset.IsDelegationTool(ev.Name) {
				continue
			}
			// The runnable narrowing, spelled in Go: an absent verdict reads as
			// allow (the SQL's COALESCE), and an ask waits for a human's allow.
			// A deny is skipped too — the platform synthesizes its error result,
			// and running it meanwhile would execute what the policy refused.
			//
			// The release is bound to "ask" rather than to any confirmation,
			// which is the whole of the rule: a confirmation is client-supplied
			// input on the events API, and only the API's own
			// ValidateToolConfirmations knows it may not name a call that was
			// never gated. This scan reads the log over the wire and cannot see
			// that check, so it states the invariant itself rather than
			// inheriting it — a gate at a trust boundary that depends on a
			// remote validation is a gate that stops working the day the
			// validation moves.
			runnable := ev.EvaluatedPermission == "" ||
				ev.EvaluatedPermission == string(domain.EvalPermAllow) ||
				(ev.EvaluatedPermission == string(domain.EvalPermAsk) && allowed[ev.ID])
			if !runnable {
				continue
			}
			// Not "stop at the first answered use": a turn's tools can be
			// answered out of order — a denial's result lands at once while an
			// allowed sibling is still outstanding — so the whole run is read.
			if !answered[ev.ID] {
				out = append(out, toolUse{id: domain.ID(ev.ID), name: ev.Name, input: ev.Input})
			}
		case domain.EventAgentToolResult, domain.EventUserToolResult:
			if sawUse && !coordinator {
				break scan // older than the trailing turn: everything past here is answered
			}
			answered[ev.ToolUseID] = true
		case domain.EventUserToolConfirm:
			// Never touches sawUse or the boundary: a verdict is not a turn's
			// end, and one recorded above a trailing run belongs to it.
			if ev.Result == "allow" {
				allowed[ev.ToolUseID] = true
			}
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	if coordinator && len(out) > 0 && head != "" {
		var err error
		if out, err = dropAnsweredSince(ctx, client, sessionID, head, out, progress); err != nil {
			return nil, err
		}
	}
	slices.Reverse(out) // the walk collected newest-first; run them in log order
	return out, nil
}

// dropAnsweredSince removes from uses any call answered while the walk that
// found them was still running, and it exists because the walk pages by a
// cursor on the sequence it started from: page two and everything after it read
// strictly older events, so a result appended above the cursor mid-walk is
// invisible to the rest of that walk. The call it answers sits below, still
// looking unanswered, and the driver would run a command that had already been
// settled — the plan's answered-means-cancelled rule (decision 9), which the
// executor keeps under its lock and this worker must keep over the wire.
//
// Only a coordinator's walk needs it. A single-agent scan stops at the trailing
// turn's boundary and spans one turn's events, so its window is a page; a
// coordinator's spans the whole log by decision 13 (iv), and on a long-lived
// session that is an arbitrarily wide window for a thread-scoped interrupt —
// which synthesizes results for exactly these calls — to land inside.
//
// One pass, not a loop to quiescence: it reads down to the walk's own anchor, so
// what remains uncovered is a result landing during this read, which is the same
// page-wide window the single-agent path has always had and the same one the
// executor's pre-run check leaves. Removing only ever shrinks the runnable set,
// so a call this pass misses is started — and then cancelled by answeredWatch on
// its next beat, or, for an answer landing after the last beat, dropped by
// postToolResult when the control plane refuses the second result.
func dropAnsweredSince(ctx context.Context, client sdk.Client, sessionID, head string,
	uses []toolUse, progress func()) ([]toolUse, error) {
	// The same type filter the walk used, not just the result types: the anchor
	// is whatever event the walk saw first, and a filter that could exclude it
	// would leave this pass with no stop condition but the start of the log —
	// the very cost it exists to avoid.
	iter := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: []string{
			string(domain.EventAgentToolUse),
			string(domain.EventAgentToolResult),
			string(domain.EventUserToolResult),
			string(domain.EventUserToolConfirm),
		},
		Order: sdk.BetaSessionEventListParamsOrderDesc,
		Limit: sdk.Int(coordinatorScanPageSize),
	})
	answered := map[string]bool{}
	for iter.Next() {
		progress()
		var ev struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal([]byte(iter.Current().RawJSON()), &ev); err != nil {
			return nil, fmt.Errorf("parse session event: %w", err)
		}
		if ev.ID == head {
			break // everything from here down was already in the walk's own view
		}
		switch domain.EventType(ev.Type) {
		case domain.EventAgentToolResult, domain.EventUserToolResult:
			answered[ev.ToolUseID] = true
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	if len(answered) == 0 {
		return uses, nil
	}
	kept := uses[:0]
	for _, u := range uses {
		if !answered[u.id.String()] {
			kept = append(kept, u)
		}
	}
	return kept, nil
}

// postToolResult sends one user.tool_result answering a tool use. Empty tool
// output posts the reference runner's toolset.NoOutput text block (v1.63.1),
// never an empty one: the Sessions API rejects an empty text block, and so
// does the Messages endpoint the brain's replay hands the content to.
// is_error is carried through so the model sees a tool-level failure as an
// error result.
func postToolResult(ctx context.Context, client sdk.Client, sessionID string, useID domain.ID, res toolset.Result) error {
	ev := sdk.BetaManagedAgentsEventParamsOfUserToolResult(useID.String())
	// The convenience constructor sets only tool_use_id; the wire requires the
	// event's type discriminator, which the union marshaler does not fill in.
	ev.OfUserToolResult.Type = sdk.BetaManagedAgentsUserToolResultEventParamsTypeUserToolResult
	ev.OfUserToolResult.IsError = sdk.Bool(res.IsError)
	text := res.Content
	if text == "" {
		text = toolset.NoOutput
	}
	ev.OfUserToolResult.Content = []sdk.BetaManagedAgentsUserToolResultEventParamsContentUnion{{
		OfText: &sdk.BetaManagedAgentsTextBlockParam{
			Text: text,
			Type: sdk.BetaManagedAgentsTextBlockTypeText,
		},
	}}
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, sdk.BetaSessionEventSendParams{
		Events: []sdk.BetaManagedAgentsEventParamsUnion{ev},
	})
	if err == nil {
		return nil
	}
	// The one refusal that is not this run's fault: the call was answered
	// between answeredWatch's last beat and this post, so the result the
	// control plane already holds is the answer and ours is the second one it
	// must refuse (ValidateToolResults: "already has a result"). Returning the
	// error would abort the whole pass over a call that is *done*, holding
	// every sibling behind it for a lease TTL — the executor drops such a
	// result under its session lock and carries on, and so must this (#441).
	//
	// Asked of the log rather than read off the message: the status is the
	// contract, the wording is not, and a 400 has other causes (an ask still
	// awaiting its human) that must still fault the run. One bounded read, and
	// only on a path that is already failing.
	if isStatus(err, 400) {
		if answered, aerr := isAnswered(ctx, client, sessionID, useID); aerr == nil && answered {
			slog.InfoContext(ctx, "worker: tool result dropped, call already answered",
				"session", sessionID, "tool_use", useID)
			return nil
		}
	}
	return fmt.Errorf("post tool result for %s: %w", useID, err)
}

const (
	// answeredBeat is how often a call in flight asks whether the log has
	// already answered it. The platform executor asks on LeaseTTL/3; the work
	// API's lease TTL defaults to 30s, so ten seconds is that same cadence —
	// written as this driver's own constant because a worker learns the
	// server's TTL only from a heartbeat response, and this check must not
	// ride the heartbeat: the beat answers for the lease and the stall budget
	// alone (#383), and threading the running call's id into it would make it
	// answer for a third thing.
	answeredBeat = 10 * time.Second
	// answeredScanPageSize is how many of the session's newest tool results one
	// such check reads. It is a single page, never an auto-pager: the answer
	// this looks for is newer than the call it watches, so anything older than
	// the page's tail cannot be it. Twenty is generous for that window, because
	// sandbox tool execution across sibling threads is serial — one
	// session-keyed tool_exec item at a time, this one — so the only results
	// that can land beside a running call are the web and MCP drivers' and a
	// settlement's delegation answers. Overrunning it costs one missed beat,
	// never a wrong cancel: a result is matched by tool_use_id, so the check
	// can fail to see an answer but can never invent one.
	answeredScanPageSize = 20
)

// answeredWatch is the BYOC twin of the platform executor's answered-means-
// cancelled check for a call in flight (plan 35 decision 9): a thread-scoped
// interrupt answers its thread's calls itself and never stops the shared exec
// item, so neither the item's lease nor this worker's heartbeat can tell the
// driver — this does, on its own beat. It returns a context cancelled once a
// result references the use, and a stop func the caller runs when the call
// returns, which reports whether the watch was what cancelled it.
//
// It is a per-call watcher and not a line in the heartbeat loop for the same
// reason the executor's is not in its lease keeper: the beat would have to be
// told which call is running, and it already answers for the lease and the
// stall budget. The check is best-effort — a failed read is one missed beat,
// never a cancelled call — and runs under the watch's own context, so stopping
// the watch never waits on a read in flight.
func answeredWatch(ctx context.Context, client sdk.Client, sessionID string, useID domain.ID, beat time.Duration) (context.Context, func() bool) {
	wctx, cancel := context.WithCancel(ctx)
	if beat <= 0 {
		beat = answeredBeat
	}
	done := make(chan struct{})
	answered := false
	go func() {
		defer close(done)
		t := time.NewTicker(beat)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if ok, err := isAnswered(wctx, client, sessionID, useID); err == nil && ok {
					answered = true
					cancel()
					return
				}
			}
		}
	}()
	return wctx, func() bool {
		cancel()
		<-done // the write to answered happens before this, so reading it is safe
		return answered
	}
}

// isAnswered reports whether some result on the session's log already answers
// useID, over the wire and in one bounded read. The two types it asks for are
// the two the scan counts as answering a sandbox call — a platform executor's
// agent.tool_result and this worker's own user.tool_result — for the reason
// stated there: those are the only kinds a call this driver may run is ever
// answered by.
func isAnswered(ctx context.Context, client sdk.Client, sessionID string, useID domain.ID) (bool, error) {
	page, err := client.Beta.Sessions.Events.List(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: []string{
			string(domain.EventAgentToolResult),
			string(domain.EventUserToolResult),
		},
		Order: sdk.BetaSessionEventListParamsOrderDesc,
		Limit: sdk.Int(answeredScanPageSize),
	})
	if err != nil {
		return false, fmt.Errorf("list tool results: %w", err)
	}
	for _, e := range page.Data {
		var ev struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON()), &ev); err != nil {
			return false, fmt.Errorf("parse session event: %w", err)
		}
		if ev.ToolUseID == useID.String() {
			return true, nil
		}
	}
	return false, nil
}
