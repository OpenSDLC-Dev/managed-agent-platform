// Package brain is the orchestration loop (the plan's component 3): a
// stateless harness that claims model_turn work, replays the session's event
// log into a provider request, streams the model's turn back into
// Anthropic-native events, and drives the session state machine at turn end.
// It runs no agent tool in-process — such a call is an emitted intent event,
// and the turn resumes when the matching result event lands (a fresh
// model_turn item enqueued by the control plane). The delegation six are not
// agent tools but the platform's own, and delegate.go answers them inside the
// settlement that commits the turn calling them: they touch no sandbox, so
// there is nothing for a driver to run and nothing to wait for. Any brain can pick up any
// turn: all durable state is the event log.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the brain's OTel instrumentation scope.
const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"

// Config sizes the loop.
type Config struct {
	// LeaseTTL is the work-item lease; the lease keeper re-extends it at
	// TTL/3 for as long as the turn is streaming.
	LeaseTTL time.Duration
	// PollInterval is the idle wait between empty queue checks.
	PollInterval time.Duration
}

const (
	defaultLeaseTTL     = 2 * time.Minute
	defaultPollInterval = 250 * time.Millisecond
)

// Brain runs model turns. All instances are interchangeable ("cattle"):
// a crashed brain's lease expires and any other replays the session.
type Brain struct {
	pool     *pgxpool.Pool
	log      *events.Log
	queue    *queue.Queue
	registry *provider.Registry
	// blobs is read-only assembly input (file-rubric snapshots for the
	// grader); nil deploys grade file rubrics from the description alone.
	// The brain still never touches a sandbox.
	blobs blob.Store
	cfg   Config
}

func New(pool *pgxpool.Pool, registry *provider.Registry, blobs blob.Store, cfg Config) *Brain {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	return &Brain{
		pool:     pool,
		log:      events.NewLog(pool),
		queue:    queue.New(pool),
		registry: registry,
		blobs:    blobs,
		cfg:      cfg,
	}
}

// Run claims and executes turns until the context ends. Infra errors are
// logged and retried — the turn's lease expires and is reclaimed.
func (b *Brain) Run(ctx context.Context) error {
	for {
		found, err := b.RunOnce(ctx)
		if err != nil && !found && !errors.Is(err, context.Canceled) {
			// Nothing was claimed, so there is no turn and no span to hang this
			// on: the queue itself is unreachable. A claimed turn's own fault is
			// reported from inside its model_turn span, where a trace carries it.
			// A cancelled claim is this loop shutting down, not a fault, and the
			// select below is about to return — saying "retrying" there would be
			// a lie at ERROR level on every clean exit.
			slog.ErrorContext(ctx, "brain: claim failed, retrying", "error", err)
		}
		if found && err == nil {
			continue // drain the queue before idling
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// RunOnce claims at most one model_turn and runs it to completion,
// reporting whether there was work.
//
// The claimed turn runs under a model_turn consumer span — the brain's
// counterpart of the executor's tool_exec span and the BYOC worker's, the same
// two work-queue claimants. It opens on the claimed item and closes on its fate
// because the nested model_request span can carry neither half of a turn fault.
// Half the faults happen before that span exists at all — claimLiveSession, the
// reclaim-recovery append, replay, request assembly, provider resolution, which
// reach failTurn with a nil span. For the rest, runTurn hands back an error and
// nothing else: sctx never leaves it, and Finish has closed the span before the
// error arrives here. Unlike a tool_exec item there is no
// enqueuing trace to continue — queue.Enqueue deliberately stores none on a
// model_turn — so this span roots the turn's trace, and the tool_exec items the
// turn enqueues carry its model_request onward as their parent.
func (b *Brain) RunOnce(ctx context.Context) (found bool, err error) {
	item, err := b.queue.Claim(ctx, queue.ModelTurn, b.cfg.LeaseTTL)
	if err != nil || item == nil {
		return false, err
	}
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "model_turn",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("session.id", item.SessionID.String()),
			attribute.String("work.id", item.ID.String()),
		))
	// Only the brain's own faults reach here. A model that failed or an input
	// the model can be told about is settled onto the wire as a session.error
	// by failTurn and returns no error, so it never reddens this span: a turn
	// the platform handled correctly is not a platform failure.
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			// Inside the span, so the record lands on the model_turn it is
			// about — that red span is where an operator asks for it. The item's
			// fate is the queue's and is not stated here, because it depends on
			// how the turn failed: an infra fault leaves the lease to expire and
			// be reclaimed, while a lost lease means the item is already someone
			// else's or was cancelled outright by a user.interrupt.
			slog.ErrorContext(ctx, "brain: turn failed, its output was not committed",
				"item", item.ID, "session", item.SessionID, "error", err)
		}
		span.End()
	}()

	// The claim is the start of time-to-first-token: replay and request assembly
	// are latency the user feels, so the clock starts here, not at the provider
	// call.
	claimedAt := time.Now()
	if err = b.runTurn(ctx, item, claimedAt); err != nil {
		// Infra failure or a lost lease: the item is left to its lease —
		// expiry hands it to another brain. The turn's output never commits
		// on these paths (settlement carries the lease proof in the same
		// transaction), so a reclaim replays from a clean log.
		return true, fmt.Errorf("session %s: %w", item.SessionID, err)
	}
	return true, nil
}

// infraError marks a brain-side failure (database, queue, lost lease) that
// must not be reported on the wire as a model failure: the turn aborts
// without a session.error and the item's lease expiry hands it to another
// brain. Everything else that reaches failTurn is either the model side
// failing or a deterministic input problem, both of which retry loops can
// never fix.
type infraError struct{ err error }

func (e infraError) Error() string { return e.err.Error() }
func (e infraError) Unwrap() error { return e.err }

func infra(format string, args ...any) error {
	return infraError{fmt.Errorf(format, args...)}
}

// streamUsage is what the model reported, or nil when nobody said: the stream
// failed before it could, or the endpoint itself reported no usage. The
// distinction is the metric's: no reading and a zero reading are different
// facts, and only a real one belongs in the token histogram (#90).
func streamUsage(turn *turnResult) *domain.ModelUsage {
	if turn == nil {
		return nil
	}
	return turn.usage
}

func (b *Brain) runTurn(ctx context.Context, item *queue.Item, claimedAt time.Time) error {
	sid := item.SessionID

	agentJSON, resourcesJSON, outcomesJSON, envKind, live, err := b.claimLiveSession(ctx, item)
	if err != nil || !live {
		return err
	}

	if item.Reclaimed {
		// The previous claimant died mid-turn. Surface the recovery on the
		// log before replaying, with the lease asserted in the same
		// transaction: a claimant that already lost the item must not flip
		// a session another brain has since settled. Both transitions are
		// forced (the pair is value-independent: claimLiveSession admitted
		// this turn only because the thread is already running) and the
		// session's net move is nothing, so AppendTransition counts nothing —
		// counting a running→running no-op as a session.status.transitions
		// event would inflate the metric on exactly the reclaim churn an
		// operator reads it to find.
		if _, err := b.log.AppendTransition(ctx, sid, nil, []events.ThreadTransition{
			{ThreadID: item.ThreadID, Status: domain.SessionRescheduling, Force: true},
			{ThreadID: item.ThreadID, Status: domain.SessionRunning, Force: true},
		}, events.AppendOptions{
			Then: func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Assert(ctx, tx, item)
			},
		}); err != nil {
			return fmt.Errorf("recovery events: %w", err)
		}
	}

	var agent domain.ResolvedAgent
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		// Deterministic: the same bytes fail the same way on every retry,
		// so a lease-expiry loop would grind forever without ever telling
		// anyone. Fail the turn visibly instead.
		return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("session agent state is corrupt: %v", err), envKind)
	}

	// A grading wake: the settlement that scheduled an evaluation cycle
	// flipped the outcome entry to evaluating and requeued this item, so the
	// claim runs the grader instead of an agent turn (two-phase — no model
	// call under a session lock; a reclaimed mid-grade item lands here too).
	var evals []domain.OutcomeEvaluation
	if len(outcomesJSON) > 0 {
		if err := json.Unmarshal(outcomesJSON, &evals); err != nil {
			return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("session outcome state is corrupt: %v", err), envKind)
		}
	}
	// Grading is the primary thread's (plan 35 decision 15): the evaluation
	// cycle is scheduled at the session's quiescence onto the primary's turn
	// with the coordinator's agent, so a child's claim never grades.
	if active, ok := events.ActiveOutcome(evals); ok && active.Result == domain.OutcomeResultEvaluating && item.ThreadID == "" {
		return b.runGrading(ctx, item, agent, evals)
	}

	// The session delegation bound (#447). It sits below the grading fork so a
	// grading item is never refused — grading is the platform answering a
	// client's own define_outcome, not autonomous delegation traffic — and
	// above everything that costs anything: no MCP catalog is loaded, no log is
	// replayed, and above all no span.model_request_start is appended, which is
	// what makes a refused claim free rather than merely short.
	if refused, err := b.cutExhaustedRun(ctx, sid, item, agent, envKind); err != nil {
		return fmt.Errorf("delegation bound: %w", err)
	} else if refused {
		return nil
	}

	// An MCP server the agent declares and this session has never reached has
	// tools nobody can name yet, so the turn is not assembled at all: it hands
	// the item back as an mcp_exec and the discovery driver chains the turn once
	// the listing is in. Suspending is what makes the tools *appear* rather than
	// silently miss the first turn — and it costs one round trip per session,
	// since only a server with no row at all gets here.
	//
	// It sits above the span start below, whose commit flips a pending outcome
	// to running, because a suspended turn is a turn that never begins: flipping
	// first would record that the agent started work on the outcome and then
	// produce nothing at all until a listing arrives.
	declared, err := declaredMCPServers(agent)
	if err != nil {
		// Deterministic, exactly as the agent decode above is: this is a spec
		// this platform stored, and re-reading it fails the same way on every
		// retry, so a lease-expiry loop would grind forever telling nobody.
		return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("session mcp server state is corrupt: %v", err), envKind)
	}
	cat, undiscovered, err := b.loadMCPCatalog(ctx, sid, item.ThreadID, declared)
	if err != nil {
		return fmt.Errorf("mcp catalog: %w", err)
	}
	if len(undiscovered) > 0 {
		return b.suspendForDiscovery(ctx, sid, item, undiscovered)
	}

	// Level-1 skill injection: resolve the agent's skills[] to a system-prompt
	// block at request-assembly time (plan design decision 5). Best-effort — an
	// unresolvable reference is a logged miss, not a failed turn.
	skillsBlock, skillsInjected, skillsMisses := b.resolveSkillsBlock(ctx, agent)
	// Count the misses now, before any early return below (a replay error, an
	// unrouted model, a failed span start) can abandon them: a resolve miss is a
	// fact of request assembly, independent of whether the turn later fails for
	// an unrelated reason. The span attributes wait for the span (below).
	recordResolveMisses(ctx, skillsMisses)
	// Mounted-file injection: a "Mounted files" block after the skills block so
	// the agent can find file mounts that live outside its workdir (plan slice
	// 3). Best-effort, mirroring skills — a dangling mount is a logged, counted
	// miss; the count is flushed now, before the early returns below, for the same
	// reason as the skills misses.
	filesBlock, filesInjected, filesMisses := b.resolveFilesBlock(ctx, resourcesJSON)
	recordFileResolveMisses(ctx, filesMisses)
	// Mounted-repository injection: a "Mounted repositories" block after the
	// files block, for cloud environments only (plan 25 decision 8). Every
	// rendered fact already lives in the stored resource, so there is no join
	// to miss.
	reposBlock, reposInjected := b.resolveReposBlock(ctx, sid, resourcesJSON, envKind)
	// Memory-store injection: a "Memory stores" block after the repositories
	// block, on both environment kinds (plan 36 decision 9; the worker's half
	// is slice 6's). A deleted store is a counted miss rendered hedged; the
	// count is flushed now for the skills' reason.
	memoryBlock, memoryInjected, memoryMisses := b.resolveMemoryBlock(ctx, resourcesJSON)
	recordMemoryResolveMisses(ctx, memoryMisses)
	// The tool surface: what the model may call, and what each name it calls
	// back means. Both come from the same resolution so the two cannot disagree,
	// and every tool the agent declared that the model was not offered is logged
	// with its reason. It sits below the injection counters deliberately — a
	// resolve miss is a fact of assembly whatever fails next (skillsinject_test).
	//
	// The delegation tools are injected by the thread's role rather than
	// declared by an agent (plan 35 decision 6): any child reports back, the
	// primary of a session whose snapshot carries a roster coordinates, and a
	// single-agent session is offered neither set. The child arm is tested
	// first because it is what holds the topology to one level — a child's own
	// snapshot never earns it the coordinator's four, whatever it carries.
	role := delegationNone
	switch {
	case item.ThreadID != "":
		role = delegationChild
	case hasRoster(agent.Multiagent):
		role = delegationCoordinator
	}
	toolDefs, class, notes, err := resolveTools(agent, cat, role)
	if err != nil {
		return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("resolve tools: %v", err), envKind)
	}
	logToolNotes(ctx, sid, notes)

	p, err := b.registry.Provider(agent.Model.ID)
	if err != nil {
		// A model with no route is a configuration error, not a transient
		// fault: fail the turn visibly rather than retry forever. No request
		// starts, so none consumes the thread's input: the failure stamps what
		// the thread held when it failed, which nothing else would, and chains
		// on anything posted since, as any failed turn chains on input past
		// what it read.
		head, herr := b.threadHead(ctx, sid, item.ThreadID)
		if herr != nil {
			return fmt.Errorf("no provider: %w", herr)
		}
		return b.failTurn(ctx, sid, item, nil, head, fmt.Sprintf("no provider for model %q", agent.Model.ID), envKind)
	}

	// The route resolved above, named for telemetry. Provider() just succeeded,
	// so Describe cannot miss; an empty backend would only mean unlabelled
	// metrics, never a failed turn.
	desc, _ := b.registry.Describe(agent.Model.ID)
	// The start's commit processes what this request consumes (#793), which a
	// client sees: it stamps the thread's inputs below it, and on the primary
	// it flips a pending outcome to running — every pending entry's
	// user.define_outcome committed with it, so it is below the start, and
	// this request is the one that reads it (the SDK: "pending" before the
	// agent begins work, "running" while producing or revising). Both carry
	// the lease proof, so a claimant that already lost the item writes
	// neither and stops here, before the model.
	sctx, span, err := b.log.StartModelRequestOn(ctx, sid, item.ThreadID,
		events.Backend{Provider: desc.Protocol, Model: desc.Model},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Assert(ctx, tx, item); err != nil {
				return err
			}
			if item.ThreadID != "" {
				return nil // a child's request reads no user.define_outcome
			}
			return events.BeginOutcomeWork(ctx, tx, sid)
		})
	if err != nil {
		return fmt.Errorf("span start: %w", err)
	}
	// Record the injection on the model_request span (bounded ints, no skill_id);
	// the miss counter was already flushed above, before the early returns.
	span.SetAttributes(
		attribute.Int("skills.injected", skillsInjected),
		attribute.Int("skills.block_chars", len(skillsBlock)),
		attribute.Int("files.injected", filesInjected),
		attribute.Int("files.block_chars", len(filesBlock)),
		attribute.Int("repos.injected", reposInjected),
		attribute.Int("repos.block_chars", len(reposBlock)),
		attribute.Int("memory.injected", memoryInjected),
		attribute.Int("memory.block_chars", len(memoryBlock)),
	)
	// The lease is kept from the start on: the history read and the build
	// below can take a while on a long log, and the lease they run under must
	// not lapse behind them.
	kctx, keeper := b.queue.KeepLease(sctx, item, b.cfg.LeaseTTL, 0)
	// The history is read once, here, after the start committed: the request
	// is then exactly what the start consumed, however late before it an input
	// landed. Nothing assembled above reads it.
	history, err := b.requestHistory(kctx, sid, item.ThreadID, span.StartSeq())
	if err != nil {
		if cerr := keeper.Close(); cerr != nil {
			span.Finish(sctx, true, cerr)
			return fmt.Errorf("lease keeper: %w", cerr)
		}
		// A fault of ours after the start committed — its inputs stamped, an
		// outcome perhaps flipped — so the item is not left to its lease with
		// a dangling start: the request is closed and the item handed straight
		// back, and the retry's own start stamps nothing new.
		rerr := b.releaseStartedTurn(sctx, sid, item, span)
		span.Finish(sctx, true, errors.Join(err, rerr))
		if rerr != nil {
			return fmt.Errorf("replay: %w", errors.Join(err, rerr))
		}
		slog.WarnContext(sctx, "brain: history read failed after the span start; item released for retry",
			"session_id", sid.String(), "error", err)
		return nil
	}
	req, watermark, err := buildRequest(agent.System, toolDefs, history, skillsBlock, filesBlock, reposBlock, memoryBlock)
	if err != nil {
		if cerr := keeper.Close(); cerr != nil {
			span.Finish(sctx, true, cerr)
			return fmt.Errorf("lease keeper: %w", cerr)
		}
		// Deterministic: the same log fails the same way on every retry, so
		// the watermark is zero and nothing chains.
		return b.failTurn(sctx, sid, item, span, 0, fmt.Sprintf("replay: %v", err), envKind)
	}
	req.Effort = agent.Model.Effort
	// The start proved the item was this claimant's, but an interrupt can have
	// stopped it since (queue.CancelSession), or a reclaim taken it, and the
	// keeper would not notice before its first renewal. A call made anyway is
	// billed for a turn nobody may commit, so ownership is proven once more
	// right before it.
	if err := b.queue.Assert(kctx, b.pool, item); err != nil {
		cerr := keeper.Close()
		span.Finish(sctx, true, errors.Join(err, cerr))
		return fmt.Errorf("model call: %w", err)
	}
	// The call to the model begins here, and its latency with it: the history
	// read and the replay above ran after the span start and are ours.
	span.ModelCalling()
	turn, streamErr := b.streamTurn(kctx, sid, item.ThreadID, p, req)
	// The call to the model ended here, whatever happens to the turn from now
	// on. Everything below is ours — leases, classification, a session-locked
	// settlement — and none of it belongs in a model-latency metric. The usage
	// goes with it: what the model spent is a fact of the call, and a turn that
	// streamed an answer and then lost its lease still cost real tokens.
	span.ModelDone(streamUsage(turn))
	// Time to first token, recorded whenever the model streamed content — even
	// if the turn later failed, the first token is a real fact once it arrived.
	// A turn that streamed nothing leaves firstTokenAt zero and records no
	// reading, the same absent-is-not-zero rule the token metric follows.
	if turn != nil && !turn.firstTokenAt.IsZero() {
		recordTTFT(sctx, events.Backend{Provider: desc.Protocol, Model: desc.Model}, turn.firstTokenAt.Sub(claimedAt))
	}
	if err := keeper.Close(); err != nil {
		// The lease is gone or unmaintainable: another brain may own the
		// turn already. Nothing of ours may commit — abandon quietly.
		span.Finish(sctx, true, err)
		return fmt.Errorf("lease keeper: %w", err)
	}
	if streamErr != nil {
		var ie infraError
		if errors.As(streamErr, &ie) {
			span.Finish(sctx, true, streamErr)
			return streamErr
		}
		return b.failTurn(sctx, sid, item, span, watermark, streamErr.Error(), envKind)
	}
	if turn.stopReason == "tool_use" && len(turn.toolUses) == 0 {
		// A tool_use stop with no tool blocks has nothing to wait for and
		// nothing to chain — settling either way would wedge or spin.
		return b.failTurn(sctx, sid, item, span, watermark, "model stopped for tool_use without any tool_use block", envKind)
	}
	if turn.stopReason == "refusal" && len(turn.toolUses) > 0 {
		// A refusal is terminal, and its tool calls are not ours to run: the
		// SDK's own agentic loop returns before executing them because they
		// "belong to a dead conversation — executing them fires side effects
		// the caller never confirmed and produces tool_results that cannot be
		// coherently replayed" (checked against anthropic-sdk-go v1.70.1 —
		// betatoolrunner.go determineNextStepFromStopReason and
		// betaToolRunnerBase.executeTools). Dropping them goes one step further
		// than the SDK, deliberately: it keeps the refused message, blocks and
		// all, in a history it has marked complete and will never send again,
		// while this log is durable and every later turn replays it — so a
		// committed intent would be one nothing may answer and nothing may run,
		// which is #181 re-opened for this one stop reason. The log loses what
		// the model asked for; the drop is logged so it stays auditable, and
		// the alternative (committing the intents against synthesized error
		// results, as a denied confirmation does) is recorded and rejected in
		// docs/DIVERGENCES.md. The turn settles on its text like any other
		// tool-less turn.
		slog.WarnContext(sctx, "brain: refusal stop reason, tool blocks dropped unexecuted",
			"session_id", sid.String(), "tool_blocks", len(turn.toolUses))
		turn.toolUses = nil
	}

	// Settle under sctx (the span-carrying context), not ctx: a tool_use turn
	// enqueues the tool_exec item in commitTurn's Then, and the enqueue captures
	// the active span's trace context into the work item so the executor or BYOC
	// worker that runs it parents its tool spans on this turn — one trace across
	// the process boundary.
	return b.settleTurn(sctx, sid, item, agent, span, turn, class, watermark, envKind)
}

// claimLiveSession loads the session under its row lock and settles stale
// work in the same transaction. A session that moved on — it settled idle
// and the settling brain then lost the race to complete its item, or it was
// archived (archiving freezes status, so the column alone can't tell; an
// archived session rejects every append and would otherwise reclaim-loop
// forever) — completes the item while no concurrent trigger can interleave:
// completing it unlocked could swallow a user.message whose enqueue this
// still-live item had suppressed.
// The environment's kind rides the same locked read (the executor's
// sessionForRun precedent) because the repositories block is emitted only for
// cloud environments — nothing materializes repositories on self_hosted, so
// asserting a checkout there would be a false statement to the model (plan 25
// decision 8).
func (b *Brain) claimLiveSession(ctx context.Context, item *queue.Item) (agentJSON, resourcesJSON, outcomesJSON []byte, envKind string, live bool, err error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The turn is one thread's (plan 35 decision 5): its status, not the
	// session's fold, is the liveness gate — a sibling's run keeps the session
	// running while this thread may have been interrupted or archived — and a
	// child runs its own agent snapshot where the primary reads the session's
	// resolved agent through (decision 1). A session from before the thread
	// resource, with no primary row, reads the session's status.
	tid := item.ThreadID
	if tid == "" {
		tid = domain.PrimaryThreadID(item.SessionID)
	}
	var status string
	var archivedAt, threadArchivedAt *time.Time
	var threadAgent []byte
	err = tx.QueryRow(ctx,
		`SELECT s.resolved_agent, s.resources, s.outcome_evaluations, COALESCE(t.status, s.status),
		        s.archived_at, t.archived_at, t.agent, e.kind
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		   LEFT JOIN session_threads t ON t.session_id = s.id AND t.id = $2
		  WHERE s.id = $1 FOR UPDATE OF s`,
		item.SessionID.String(), tid.String()).Scan(&agentJSON, &resourcesJSON, &outcomesJSON, &status,
		&archivedAt, &threadArchivedAt, &threadAgent, &envKind)
	if err != nil {
		return nil, nil, nil, "", false, fmt.Errorf("load session: %w", err)
	}
	if item.ThreadID != "" {
		if threadAgent == nil {
			// No child row: nothing to run (only the session's delete removes
			// one, and that cascades the item too — a stray is drained).
			status = string(domain.SessionTerminated)
		}
		agentJSON = threadAgent
	}
	if status != string(domain.SessionRunning) || archivedAt != nil || threadArchivedAt != nil {
		if err := b.queue.Complete(ctx, tx, item); err != nil {
			return nil, nil, nil, "", false, err
		}
		return nil, nil, nil, "", false, tx.Commit(ctx)
	}

	// A stale/reclaimed model item is not permission to replay past tools.
	// This also repairs a legacy running wait when its old item survived.
	flow, err := b.log.AdvanceThreadTools(ctx, tx, item.SessionID, item.ThreadID, func(name string) bool { return toolset.IsWebTool(name) || toolset.IsDelegationTool(name) })
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if flow.Unsettled {
		moved, err := b.log.SettleToolFlow(ctx, tx, item.SessionID, item.ThreadID, flow)
		if err != nil {
			return nil, nil, nil, "", false, err
		}
		if err := b.queue.Complete(ctx, tx, item); err != nil {
			return nil, nil, nil, "", false, err
		}
		if err := b.enqueueRunnableTools(ctx, tx, item); err != nil {
			return nil, nil, nil, "", false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, nil, "", false, err
		}
		if moved != nil {
			events.RecordSessionStatus(ctx, *moved)
		}
		return nil, nil, nil, "", false, nil
	}
	return agentJSON, resourcesJSON, outcomesJSON, envKind, true, tx.Commit(ctx)
}

// requestHistory reads what the request its span start opened consumes: the
// thread's own rows (plan 35 decision 5) — a sibling's events, a child's
// cross-posted ask included, are not this conversation's — below the start
// (#793). Seq is allocated under the session row lock, so every row below the
// start committed before it did; a row past it landed mid-request and is the
// next request's, though this read, made after the start, can see it.
func (b *Brain) requestHistory(ctx context.Context, sid, threadID domain.ID, startSeq int64) ([]domain.Event, error) {
	return b.log.List(ctx, sid, events.ListQuery{Scope: events.ScopeThread, ThreadID: threadID, BeforeSeq: &startSeq})
}

// releaseStartedTurn closes a request that stopped short of the model on a
// fault of the platform's, with an errored span.model_request_end, and hands
// the item back to the queue in the same commit, under the lease proof
// Requeue carries: the retry claims it at once instead of after the lease,
// and no session.error is written for a fault that was not the model's.
func (b *Brain) releaseStartedTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest) error {
	endEv, err := span.EndEvent(true, domain.ModelUsage{})
	if err != nil {
		return err
	}
	_, err = b.log.AppendWith(ctx, sid, []events.NewEvent{endEv}, events.AppendOptions{
		ThreadID: item.ThreadID,
		Then:     func(ctx context.Context, tx pgx.Tx) error { return b.queue.Requeue(ctx, tx, item) },
	})
	return err
}

// threadHead is the thread's newest seq: the watermark of a failure that
// stops a turn before it reads its history, which stands for what the thread
// held when the turn failed.
func (b *Brain) threadHead(ctx context.Context, sid, threadID domain.ID) (int64, error) {
	var head int64
	err := b.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1 AND thread_id IS NOT DISTINCT FROM $2`,
		sid.String(), events.NullableThread(threadID)).Scan(&head)
	return head, err
}

// pendingInputTypes are the inbound events whose arrival must chain the next
// turn rather than let the session idle past them: a consumed input a client
// posts (domain.ConsumedInputs' inbound half) — a user.message appended
// mid-turn, whose trigger saw a running session and only appended, or a
// user.define_outcome, which the agent begins work on immediately — and a tool
// result whose enqueue this turn's live item suppressed. The consumed input
// the platform writes, agent.thread_message_received, is chainInput's to find,
// by seq.
var pendingInputTypes = func() []string {
	out := []string{string(domain.EventUserToolResult), string(domain.EventUserCustomToolRes)}
	for _, t := range domain.ConsumedInputs {
		if t.Inbound() {
			out = append(out, string(t))
		}
	}
	return out
}()

// pendingInput asks it for one thread's own rows (plan 35 decision 5): a
// sibling's queued input is the sibling's turn to read. It is the probe for a
// settlement with no watermark of its own (the grading chain, the delegation
// bound): an unprocessed row is one no request of the thread has started on
// since it landed — a request's start stamps what it consumes (#793) — and a
// tool result counts, processed or not, while no request has started since it
// landed.
func pendingInput(ctx context.Context, tx pgx.Tx, sid, threadID domain.ID) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE session_id = $1 AND type = ANY($2)
		    AND (processed_at IS NULL OR (type IN ('user.tool_result','user.custom_tool_result')
 AND seq>COALESCE((SELECT max(start.seq) FROM events start
 WHERE start.session_id=$1 AND start.thread_id IS NOT DISTINCT FROM $3 AND start.type='span.model_request_start'),0)))
		    AND thread_id IS NOT DISTINCT FROM $3)`,
		sid.String(), pendingInputTypes, events.NullableThread(threadID)).Scan(&pending)
	return pending, err
}

// stampThread marks a turn's events as its thread's own (plan 35 decision 2);
// on a child, what a client must answer is cross-posted — the ask-gated calls
// and external tool-result waits — since the session view is where the human,
// client or self_hosted worker who answers them reads (decision 9). An allowed
// platform-executed call stays on the child's own log.
//
// It owns the turn's OWN events and nothing else: an event this settlement
// writes on another thread's log — a spawned child's first input, a report on
// the primary's — carries its own ThreadID and must be appended past this
// call, or it is silently re-homed onto the thread that is settling.
func stampThread(evs []events.NewEvent, threadID domain.ID, askIDs []domain.ID) {
	for i := range evs {
		evs[i].ThreadID = threadID
		if threadID != "" && (evs[i].Type == domain.EventAgentCustomToolUse || slices.Contains(askIDs, evs[i].ID)) {
			evs[i].CrossPosted = true
		}
	}
}

// turnEvents renders the model's turn as its wire events: the buffered
// agent.message under the preview-reserved id, then one intent event per
// tool call. turn.text holds no empty blocks — the stream never opens one —
// so a "text" block always carries its required text field.
//
// Each platform tool_use is stamped with its evaluated_permission (the resolved
// policy: allow for always_allow, ask for always_ask); custom tools are
// client-executed and carry none, and an MCP call is a platform call like any
// other — the third party running the tool changes who answers it, not who
// authorised it. It reports the work kind the settlement must enqueue (empty
// when every intent is client-executed and there is nothing for this platform to
// run) and the pre-minted ids of the tool_use events whose policy is always_ask
// (askIDs) — the events a requires_action suspension blocks on. An ask intent's
// id is minted here rather than left to the store so the same id can name it in
// the status_idle stop_reason.
//
// A delegation call is the fourth shape (plan 35 decision 6): it commits as an
// agent.tool_use stamped allow like any platform call, but escalates no work
// kind — no driver can run it — and comes back as delegated for the settlement
// to resolve and answer in this same commit. Its id is minted here for the same
// reason an ask's is: the answer has to name it.
//
// A name absent from class entirely — the model was not offered it under any
// role — takes the same shape as a delegation call rather than a fifth of its
// own: delegatedCall.unoffered marks it so d.run answers it "unknown tool"
// instead of dispatching one of the six (#567). This is deliberately distinct
// from the settlement-executed-but-unoffered case just above (a coordinator
// reaching for a worker's tool, or the reverse): that name IS in class — every
// role classes all six, offered or not — so it keeps going to wrongRole.
func turnEvents(turn *turnResult, class map[string]toolClass) (batch []events.NewEvent, kind queue.Kind, askIDs []domain.ID, delegated []delegatedCall, err error) {
	if len(turn.text) > 0 {
		content, err := json.Marshal(map[string]any{"content": turn.text})
		if err != nil {
			return nil, "", nil, nil, err
		}
		batch = append(batch, events.NewEvent{
			ID: turn.messageEventID, Type: domain.EventAgentMessage, Payload: content,
		})
	}
	for _, tu := range turn.toolUses {
		fields := map[string]any{
			"name": tu.Name, "input": tu.Input, "session_thread_id": nil,
		}
		c, offered := class[tu.Name]
		if !offered {
			// A name the model was not offered: not a declared custom tool (a
			// client would answer that one), not a platform tool this thread's
			// role owns. The platform must still never run it, but parking it as
			// agent.custom_tool_use strands the thread on a client that will
			// never post a result (#567) — so it takes a delegation call's
			// shape instead: an ordinary agent.tool_use, stamped deny (below,
			// where perm is picked) and answered inline by the settlement rather
			// than by any driver.
			c = toolClass{kind: domain.EventAgentToolUse, settlement: true}
		}
		id := domain.NewID("sevt")
		gated := false
		switch c.kind {
		case domain.EventAgentCustomToolUse:
			askIDs = append(askIDs, id)
		case domain.EventAgentToolUse:
			gated = true
			// A delegation call escalates nothing: the settlement resolves it
			// and answers it in this same commit, so there is no work for a
			// driver to claim and a work item enqueued for one would find
			// nothing runnable.
			if c.settlement {
				break
			}
			kind = escalate(kind, queue.ToolExec)
			if toolset.IsWebTool(tu.Name) {
				kind = escalate(kind, queue.WebExec)
			}
		case domain.EventAgentMCPToolUse:
			gated = true
			kind = escalate(kind, queue.MCPExec)
			// The wire carries the server and the bare tool in two fields; the
			// prefixed name the model was offered exists only inside a provider
			// request (resolveTools), and putting it on the log would make every
			// consumer of the session stream decode a naming scheme of ours.
			fields["name"] = c.tool
			fields["mcp_server_name"] = c.server
		}
		if gated {
			perm := domain.EvalPermAllow
			switch {
			case !offered:
				// #567: a name the model was not offered was denied, not
				// allowed — the settlement answers it "unknown" in this same
				// commit, but that answer is only this platform's own; a
				// self_hosted worker built to the reference contract streams
				// agent.tool_use directly and may own this very name in its
				// own registered toolset (a disabled "edit" is still "edit"
				// to a worker running the standard set), and does not treat
				// an agent.tool_result as answered (it dedups on
				// user.tool_result/user.custom_tool_result alone). An "allow"
				// stamp would tell such a worker to run what this platform
				// already refused; "deny" is the value the reference
				// documents such a worker never executes but still yields.
				perm = domain.EvalPermDeny
			case c.policy == domain.PolicyAlwaysAsk:
				perm = domain.EvalPermAsk
				askIDs = append(askIDs, id)
			}
			fields["evaluated_permission"] = perm
		}
		if c.settlement {
			// Inline settlement uses the same preallocated event identity.
			delegated = append(delegated, delegatedCall{
				eventID: id, name: tu.Name, input: tu.Input, unoffered: !offered,
			})
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, "", nil, nil, err
		}
		batch = append(batch, events.NewEvent{ID: id, Type: c.kind, Payload: payload})
	}
	return batch, kind, askIDs, delegated, nil
}

// delegatedCall is one settlement-executed delegation call, as turnEvents
// committed it: the id its agent.tool_result must answer, the tool the model
// named, and the input it sent. Model order, because the calls of one turn are
// resolved in the order the model made them.
type delegatedCall struct {
	eventID domain.ID
	name    string
	input   json.RawMessage
	// unoffered marks a name absent from class entirely (#567) — not one of
	// the six, whichever half this thread owns. d.run answers it before
	// wrongRole or the six-way switch ever sees it: both assume a genuine
	// delegation call, and dispatching an arbitrary hallucinated name into
	// createAgent or the rest on their say-so is not what either promises.
	unoffered bool
}

// escalate picks the work kind a turn's whole set of platform calls settles to.
// A turn may mix families, and only one item is enqueued: whichever driver runs
// answers its own family and chains the next needed, so the order here is which
// family must be answered first rather than which is most common.
//
// mcp_exec outranks both, because only this platform's MCP driver answers an
// agent.mcp_tool_use — a client posts neither the call nor its result, and a
// BYOC worker's contract has no MCP surface. web_exec outranks tool_exec for the
// same shape of reason: a tool_exec is visible to a BYOC worker, whose official
// toolset implements the six sandbox tools alone and would fail a web call as an
// unknown tool. The same precedence is spelled out at the confirmation resume
// (internal/api/events.go) and in each driver's own settlement.
func escalate(have, want queue.Kind) queue.Kind {
	if execRank(want) > execRank(have) {
		return want
	}
	return have
}

// execRank is that order. Anything else ranks below every kind named here,
// including the zero Kind that stands for "this turn has enqueued nothing yet".
func execRank(k queue.Kind) int {
	switch k {
	case queue.MCPExec:
		return 3
	case queue.WebExec:
		return 2
	case queue.ToolExec:
		return 1
	default:
		return 0
	}
}

// settleTurn commits the turn: the emitted events (message, tool intents),
// the span end, the status change, the usage fold, the watermark, and the
// work item's fate — one transaction under the session row lock, with the
// queue's lease proof inside it via the item-fate call. That single commit
// is both the liveness guarantee (the API's triggers serialize on the same
// lock, so a tool result posted mid-settle either sees our live item and is
// suppressed, or sees it completed and enqueues — never the gap where both
// sides stand down) and the integrity guarantee (a brain that lost its claim
// rolls the whole turn back; the log never carries a loser's half-turn,
// whose duplicate tool intents would poison every future replay).
//
// agent is the turn's own — the session's resolved agent on the primary, a
// child's snapshot on a child — which the delegation branch reads for the
// roster it spawns from and the name a child reports under (plan 35 decision 6).
func (b *Brain) settleTurn(ctx context.Context, sid domain.ID, item *queue.Item, agent domain.ResolvedAgent, span *events.ModelRequest, turn *turnResult, class map[string]toolClass, watermark int64, envKind string) error {
	err := b.commitTurn(ctx, sid, item, agent, span, turn, class, watermark, envKind)
	span.Finish(ctx, false, err)
	if err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	return nil
}

func (b *Brain) commitTurn(ctx context.Context, sid domain.ID, item *queue.Item, agent domain.ResolvedAgent, span *events.ModelRequest, turn *turnResult, class map[string]toolClass, watermark int64, envKind string) error {
	head, workKind, askIDs, delegated, err := turnEvents(turn, class)
	if err != nil {
		return err
	}
	askIDs = events.ToolWaitIDs(head, envKind, func(name string) bool {
		return toolset.IsWebTool(name) || toolset.IsDelegationTool(name)
	})
	// Absent usage settles as zeroes here, deliberately: the wire schema wants
	// a model_usage object on every span.model_request_end, and the session's
	// cumulative usage must still be folded (a skipped fold would also skip the
	// session row's updated_at). Only the metric distinguishes absent from zero
	// — that path runs through streamUsage into ModelDone (#90).
	usage := domain.ModelUsage{}
	if turn.usage != nil {
		usage = *turn.usage
	}
	endEv, err := span.EndEvent(false, usage)
	if err != nil {
		return err
	}
	head = append(head, endEv)
	stampThread(head, item.ThreadID, askIDs)
	// No MarkProcessedThrough: the request's start stamped everything this
	// turn replayed (the watermark is below the start), and what landed
	// since is the next request's to stamp.
	opts := events.AppendOptions{
		ThreadID: item.ThreadID,
		AddUsage: &usage,
	}

	// A turn that called tools suspends on them, whatever stop reason came
	// with them — the classification is the blocks, not the label. Nothing in
	// the Messages schema ties the two: max_tokens, stop_sequence, refusal and
	// a non-compliant end_turn can all arrive over a complete tool block. The
	// SDK's own agentic loop read the blocks too (checked against
	// anthropic-sdk-go v1.66.0 — betatoolrunner.go
	// betaToolRunnerBase.executeTools), though it declined to run *any* call of
	// a cut-off turn (max_tokens, model_context_window_exceeded), complete ones
	// included (since anthropic-sdk-go v1.62.0 — betatoolrunner.go
	// betaToolRunnerBase.executeTools), and it runs tools on a tool_use stop
	// alone (since anthropic-sdk-go v1.67.0 — betatoolrunner.go
	// determineNextStepFromStopReason). Here a truncated block never reaches
	// this point (below), so the complete ones run (docs/DIVERGENCES.md, #181).
	// The intents commit either way (turnEvents emits one tool-intent event per
	// block), so classifying on the label would idle the session with calls
	// nothing ever enqueues and leave every later replay carrying a tool_use no
	// result answers. runTurn has already resolved the two shapes that are not
	// tool turns: a tool_use stop with no blocks fails, and a refusal arrives
	// here with its blocks dropped.
	//
	// A block truncated mid-input never reaches here — streamTurn rejects a
	// tool input that is not a complete JSON object, and a proper prefix of an
	// object never parses as one. A block cut before its first input delta is
	// the exception: it arrives as {} and runs, which the tool answers with
	// its own required-argument error, recoverable where a stranded call is
	// not (docs/DIVERGENCES.md).
	if len(turn.toolUses) > 0 {
		if len(delegated) > 0 {
			// A delegation call — or a name the model was not offered at all
			// (#567) — is answered in the commit that emits it, whatever else
			// the turn holds — no driver can run either — so this branch owns
			// every shape a turn with one takes, the ask gate and the mixed exec
			// turn included (plan 35 decision 6). The turn is settlement-only
			// when every call was one of these: a client-executed custom tool
			// escalates no work kind either, and chaining past one would replay
			// a tool_use nothing answered.
			return b.commitDelegatedTurn(ctx, sid, item, agent, head, opts, workKind, askIDs, delegated,
				len(delegated) == len(turn.toolUses), watermark, envKind)
		}
		// The log owns the ordered tool wait for cloud and external execution.
		// Appending the intents before settling lets every path use the same
		// received/processed distinction, including inline denial results.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			moved, err := b.settleTools(ctx, tx, item)
			if err != nil {
				return err
			}
			return b.enqueueIdleHarvest(ctx, tx, item, sid, moved, envKind)
		}
		return b.commitUnderLock(ctx, sid, func(context.Context, pgx.Tx) ([]events.NewEvent, events.AppendOptions, error) { return head, opts, nil })
	}

	// A turn that called no tool: end_turn, and everything else —
	// max_tokens, stop_sequence — treated as a completed turn in v1. An
	// active outcome intercepts the idle: the work cycle ended, so the
	// settlement schedules an evaluation cycle instead (plan 21 slice 3).
	return b.settleEndTurn(ctx, sid, item, watermark, opts, head)
}

// settle is the failed turn's settlement: under the session row lock it asks
// whether input arrived mid-turn, lets the caller build the events that
// outcome calls for, and commits them together with the status, the
// watermark, and the work item's fate. A child that idles out of retries
// tells its coordinator so on the way (plan 35 decision 7): nothing else
// would, and the coordinator would wait for a report that never comes.
// Chaining hands our own item back to the queue (a fresh Enqueue would be
// suppressed by this very item's live slot) and leaves the session running;
// idling completes the item.
// Successful tool-less turns settle in settleEndTurn and grading cycles in
// settleVerdict/settleGraderError (plan 21 slice 3) — same lock, same
// chain-or-idle contract, restated there because outcomes fork the idle
// path.
func (b *Brain) settle(ctx context.Context, sid domain.ID, item *queue.Item, watermark int64, envKind string,
	opts events.AppendOptions, idleStop *domain.StopReason, build func(chained bool) ([]events.NewEvent, error)) error {

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}

	// A watermark of zero means the turn failed before replay resolved
	// anything (corrupt state): chaining on the thread's own unprocessed
	// events would loop the same failure forever.
	chained := false
	if watermark > 0 {
		if chained, err = chainInput(ctx, tx, sid, item.ThreadID, watermark); err != nil {
			return err
		}
	}
	batch, err := build(chained)
	if err != nil {
		return err
	}
	stampThread(batch, item.ThreadID, nil)
	opts.ThreadID = item.ThreadID
	if chained {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	} else {
		// A child whose turn is out of retries stops for good, and only a
		// message on the primary's log says so (plan 35 decision 7). It is
		// appended past stampThread deliberately — that call owns the turn's
		// own events, and a cross-thread event carries its own thread — and
		// the primary is woken before the child idles, so a session whose last
		// running thread is this one never folds idle in between.
		wakeParent := false
		var wokeTo *domain.SessionStatus
		if item.ThreadID != "" && idleStop != nil && idleStop.Type == domain.StopRetriesExhausted {
			notice, err := childEndedNotice(ctx, tx, sid, item.ThreadID, func(agentName string) string {
				return fmt.Sprintf("[agent %s stopped: its turn exhausted its retries]\n\n"+
					"Archive it to free the slot, or spawn a replacement.", agentName)
			})
			if err != nil {
				return err
			}
			if notice != nil {
				// Without the wake a coordinator parked on this child waits for a
				// report that will never come; with a sibling still working there
				// is one coming, and that one's arrival wakes it (the rule
				// events.DeliverThreadEnded applies, shared by every ending). It
				// hangs off the notice because both answer one question — whether
				// a coordinator is owed anything by this ending.
				told, err := events.DeliverThreadEnded(ctx, tx, sid, item.ThreadID, *notice)
				if err != nil {
					return err
				}
				batch = append(batch, told.Events()...)
				wakeParent, wokeTo = told.Woke(), told.Moved
			}
		}
		pair, moved, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
			ThreadID: item.ThreadID, Status: domain.SessionIdle, Stop: idleStop})
		if err != nil {
			return err
		}
		batch = append(batch, pair...)
		if moved == nil {
			// The net of the two transitions, which is what the post-commit
			// metric counts: the wake may have moved the column that the
			// child's own idle then left where it was.
			moved = wokeTo
		}
		opts.SetStatus = moved
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			if err := b.enqueueIdleHarvest(ctx, tx, item, sid, moved, envKind); err != nil {
				return err
			}
			if !wakeParent {
				return nil
			}
			_, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, "", queue.ModelTurn)
			return err
		}
	}

	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if opts.SetStatus != nil {
		events.RecordSessionStatus(ctx, *opts.SetStatus)
	}
	return nil
}

// commitUnderLock commits a settlement with the session row locked first,
// for the settlement that has no chain-or-idle decision: build runs under
// the lock — a status transition folds the session there (plan 35 decision
// 4) — and returns the batch and options to append.
func (b *Brain) commitUnderLock(ctx context.Context, sid domain.ID,
	build func(ctx context.Context, tx pgx.Tx) ([]events.NewEvent, events.AppendOptions, error)) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}
	before, err := sessionStatusNow(ctx, tx, sid)
	if err != nil {
		return err
	}
	batch, opts, err := build(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	after, err := sessionStatusNow(ctx, tx, sid)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if before != after {
		events.RecordSessionStatus(ctx, after)
	}
	return nil
}

// maxFailureMessage bounds the text a session.error carries. Generous enough
// that no real explanation is cut, small enough that none of them is a payload.
const maxFailureMessage = 8 << 10

// failTurn records a model-side or deterministic failure on the log. If no
// input is pending past the watermark, the session idles with
// retries_exhausted (v1 has no automatic retry budget — documented in
// docs/DIVERGENCES.md); input that arrived mid-turn instead chains a fresh turn, so a
// failed request cannot strand an accepted message on an idle session. Span
// end, error, status, and item fate commit atomically under the session
// lock, with the lease proof, exactly like a successful settle.
func (b *Brain) failTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg, envKind string) error {
	// The message may quote endpoint bytes (an error body, a stream error),
	// and a NUL there would fault the session.error append — the failure
	// path failing, the same wedge one level up (#228).
	msg = toolset.SanitizeText(msg)
	// Every string a message is built from belongs to somebody else, and none of
	// them is capped where it is stored: an endpoint's error body, a server name
	// off the agent spec, a permission policy type as the spec spelled it. The
	// event sits on the session stream for as long as the session does and every
	// client replays it, so a message past this is a payload rather than an
	// explanation.
	if len(msg) > maxFailureMessage {
		msg = toolset.TruncateRunes(msg, maxFailureMessage) + "[truncated]"
	}
	err := b.commitFailure(ctx, sid, item, span, watermark, msg, envKind)
	if span != nil {
		span.Finish(ctx, true, err)
	}
	return err
}

func (b *Brain) commitFailure(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg, envKind string) error {
	var head []events.NewEvent
	if span != nil {
		endEv, err := span.EndEvent(true, domain.ModelUsage{})
		if err != nil {
			return err
		}
		head = append(head, endEv)
	}

	// The stamp is for the one failure no request follows — no provider
	// routes the model (span == nil, watermark the thread's head when it
	// failed) — where nothing else would ever stamp the thread's input and
	// pendingInput would read it as queued forever. After a start there is
	// nothing to stamp: the start did.
	var opts events.AppendOptions
	if span == nil {
		opts.MarkProcessedThrough = watermark
	}
	return b.settle(ctx, sid, item, watermark, envKind, opts,
		&domain.StopReason{Type: domain.StopRetriesExhausted},
		func(chained bool) ([]events.NewEvent, error) {
			// retry_status tells the client whether the platform will make
			// another attempt. A chained turn is one: the session stays
			// running and the pending input gets its answer, so calling it
			// "exhausted" — the terminal variant — would tell a client the
			// session is dead while it is still producing events.
			retry := "exhausted"
			if chained {
				retry = "retrying"
			}
			errPayload, err := json.Marshal(map[string]any{"error": map[string]any{
				"type": "model_request_failed_error", "message": msg,
				"retry_status": map[string]any{"type": retry},
			}})
			if err != nil {
				return nil, err
			}
			return append(head, events.NewEvent{Type: domain.EventSessionError, Payload: errPayload}), nil
		})
}
