// Package brain is the orchestration loop (the plan's component 3): a
// stateless harness that claims model_turn work, replays the session's event
// log into a provider request, streams the model's turn back into
// Anthropic-native events, and drives the session state machine at turn end.
// It never runs tools in-process — a tool call is an emitted intent event,
// and the turn resumes when the matching result event lands (a fresh
// model_turn item enqueued by the control plane). Any brain can pick up any
// turn: all durable state is the event log.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	cfg      Config
}

func New(pool *pgxpool.Pool, registry *provider.Registry, cfg Config) *Brain {
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
		cfg:      cfg,
	}
}

// Run claims and executes turns until the context ends. Infra errors are
// logged and retried — the turn's lease expires and is reclaimed.
func (b *Brain) Run(ctx context.Context) error {
	for {
		found, err := b.RunOnce(ctx)
		if err != nil {
			slog.Error("brain: turn failed, lease left to expire", "error", err)
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
func (b *Brain) RunOnce(ctx context.Context) (bool, error) {
	item, err := b.queue.Claim(ctx, queue.ModelTurn, b.cfg.LeaseTTL)
	if err != nil || item == nil {
		return false, err
	}
	// The claim is the start of time-to-first-token: replay and request assembly
	// are latency the user feels, so the clock starts here, not at the provider
	// call.
	claimedAt := time.Now()
	if err := b.runTurn(ctx, item, claimedAt); err != nil {
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

	agentJSON, live, err := b.claimLiveSession(ctx, item)
	if err != nil || !live {
		return err
	}

	if item.Reclaimed {
		// The previous claimant died mid-turn. Surface the recovery on the
		// log before replaying, with the lease asserted in the same
		// transaction: a claimant that already lost the item must not flip
		// a session another brain has since settled.
		running := domain.SessionRunning
		if _, err := b.log.AppendWith(ctx, sid, []events.NewEvent{
			{Type: domain.EventSessionStatusRescheduled},
			{Type: domain.EventSessionStatusRunning},
		}, events.AppendOptions{
			SetStatus: &running,
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
		return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("session agent state is corrupt: %v", err))
	}

	history, err := b.log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	req, watermark, err := buildRequest(agent, history)
	if err != nil {
		return b.failTurn(ctx, sid, item, nil, 0, fmt.Sprintf("replay: %v", err))
	}

	p, err := b.registry.Provider(agent.Model.ID)
	if err != nil {
		// A model with no route is a configuration error, not a transient
		// fault: fail the turn visibly rather than retry forever.
		return b.failTurn(ctx, sid, item, nil, watermark, fmt.Sprintf("no provider for model %q", agent.Model.ID))
	}

	// The route resolved above, named for telemetry. Provider() just succeeded,
	// so Describe cannot miss; an empty backend would only mean unlabelled
	// metrics, never a failed turn.
	desc, _ := b.registry.Describe(agent.Model.ID)
	sctx, span, err := b.log.StartModelRequest(ctx, sid,
		events.Backend{Provider: desc.Protocol, Model: desc.Model})
	if err != nil {
		return fmt.Errorf("span start: %w", err)
	}

	kctx, keeper := b.keepLease(sctx, item)
	turn, streamErr := b.streamTurn(kctx, sid, p, req)
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
	if err := keeper.close(); err != nil {
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
		return b.failTurn(sctx, sid, item, span, watermark, streamErr.Error())
	}
	if turn.stopReason == "tool_use" && len(turn.toolUses) == 0 {
		// A tool_use stop with no tool blocks has nothing to wait for and
		// nothing to chain — settling either way would wedge or spin.
		return b.failTurn(sctx, sid, item, span, watermark, "model stopped for tool_use without any tool_use block")
	}

	// Only a turn that actually called a tool needs the name→type and
	// name→policy maps; a text-only end_turn would otherwise re-expand the
	// whole toolset for nothing.
	var toolKind map[string]domain.EventType
	var policy map[string]domain.PermissionPolicyType
	if len(turn.toolUses) > 0 {
		kinds, pols, err := classify(agent)
		if err != nil {
			return b.failTurn(sctx, sid, item, span, watermark, fmt.Sprintf("classify tools: %v", err))
		}
		toolKind, policy = kinds, pols
	}
	// Settle under sctx (the span-carrying context), not ctx: a tool_use turn
	// enqueues the tool_exec item in commitTurn's Then, and the enqueue captures
	// the active span's trace context into the work item so the executor or BYOC
	// worker that runs it parents its tool spans on this turn — one trace across
	// the process boundary.
	return b.settleTurn(sctx, sid, item, span, turn, toolKind, policy, watermark)
}

// claimLiveSession loads the session under its row lock and settles stale
// work in the same transaction. A session that moved on — it settled idle
// and the settling brain then lost the race to complete its item, or it was
// archived (archiving freezes status, so the column alone can't tell; an
// archived session rejects every append and would otherwise reclaim-loop
// forever) — completes the item while no concurrent trigger can interleave:
// completing it unlocked could swallow a user.message whose enqueue this
// still-live item had suppressed.
func (b *Brain) claimLiveSession(ctx context.Context, item *queue.Item) ([]byte, bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var agentJSON []byte
	var status string
	var archivedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT resolved_agent, status, archived_at FROM sessions WHERE id = $1 FOR UPDATE`,
		item.SessionID.String()).Scan(&agentJSON, &status, &archivedAt)
	if err != nil {
		return nil, false, fmt.Errorf("load session: %w", err)
	}
	if status != string(domain.SessionRunning) || archivedAt != nil {
		if err := b.queue.Complete(ctx, tx, item); err != nil {
			return nil, false, err
		}
		return nil, false, tx.Commit(ctx)
	}
	return agentJSON, true, tx.Commit(ctx)
}

// leaseKeeper extends the work-item lease on a timer while the turn streams:
// a model can think far longer than any inter-chunk gap allows for (long
// time-to-first-token on a big replayed context), and a lease lapsing under
// a healthy turn would fork the session across two brains.
type leaseKeeper struct {
	cancel context.CancelFunc
	quit   chan struct{}
	done   chan struct{}
	failed error // written once by the goroutine before done closes
}

func (b *Brain) keepLease(ctx context.Context, item *queue.Item) (context.Context, *leaseKeeper) {
	kctx, cancel := context.WithCancel(ctx)
	k := &leaseKeeper{cancel: cancel, quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		t := time.NewTicker(b.cfg.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-k.quit:
				return
			case <-kctx.Done():
				return
			case <-t.C:
				// Bounded: close() waits for this goroutine, so an Extend
				// blocked on an exhausted pool or a stalled database would
				// otherwise hang the turn forever. The budget is the lease
				// the last renewal bought minus the tick we waited — an
				// Extend that overruns it has let the lease lapse anyway.
				// A duration, not the lease timestamp: the deadline must
				// not depend on agreement between the database clock and
				// this process's.
				ectx, ecancel := context.WithTimeout(kctx, b.cfg.LeaseTTL-b.cfg.LeaseTTL/3)
				err := b.queue.Extend(ectx, item, b.cfg.LeaseTTL)
				ecancel()
				if err != nil {
					k.failed = err
					k.cancel() // aborts the in-flight provider stream
					return
				}
			}
		}
	}()
	return kctx, k
}

// close stops the keeper and reports the first extension failure. The
// goroutine has exited when close returns, so the item's lease value is
// stable again for settlement to use as its ownership proof.
func (k *leaseKeeper) close() error {
	close(k.quit)
	<-k.done
	k.cancel()
	return k.failed
}

// pendingInputTypes are the inbound events whose arrival must chain the next
// turn rather than let the session idle past them: a user.message appended
// mid-turn (its trigger saw a running session and only appended) or a tool
// result whose enqueue this turn's live item suppressed.
var pendingInputTypes = []string{
	string(domain.EventUserMessage),
	string(domain.EventUserToolResult),
	string(domain.EventUserCustomToolRes),
}

func pendingInput(ctx context.Context, tx pgx.Tx, sid domain.ID, watermark int64) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE session_id = $1 AND type = ANY($2) AND processed_at IS NULL AND seq > $3)`,
		sid.String(), pendingInputTypes, watermark).Scan(&pending)
	return pending, err
}

// turnEvents renders the model's turn as its wire events: the buffered
// agent.message under the preview-reserved id, then one intent event per
// tool call. turn.text holds no empty blocks — the stream never opens one —
// so a "text" block always carries its required text field.
//
// Each platform tool_use is stamped with its evaluated_permission (the resolved
// policy: allow for always_allow, ask for always_ask); custom tools are
// client-executed and carry none. It reports whether any intent is a
// platform-executed tool (platform) and the pre-minted ids of the tool_use
// events whose policy is always_ask (askIDs) — the events a requires_action
// suspension blocks on. An ask intent's id is minted here rather than left to
// the store so the same id can name it in the status_idle stop_reason.
func turnEvents(turn *turnResult, toolKind map[string]domain.EventType, policy map[string]domain.PermissionPolicyType) (batch []events.NewEvent, platform bool, askIDs []domain.ID, err error) {
	if len(turn.text) > 0 {
		content, err := json.Marshal(map[string]any{"content": turn.text})
		if err != nil {
			return nil, false, nil, err
		}
		batch = append(batch, events.NewEvent{
			ID: turn.messageEventID, Type: domain.EventAgentMessage, Payload: content,
		})
	}
	for _, tu := range turn.toolUses {
		fields := map[string]any{
			"name": tu.Name, "input": tu.Input, "session_thread_id": nil,
		}
		typ := toolKind[tu.Name]
		if typ == "" {
			// A name the model was not offered — treat it as client-executed so
			// the platform never runs a tool it does not recognise as its own.
			typ = domain.EventAgentCustomToolUse
		}
		var id domain.ID
		if typ == domain.EventAgentToolUse {
			platform = true
			perm := domain.EvalPermAllow
			if policy[tu.Name] == domain.PolicyAlwaysAsk {
				perm = domain.EvalPermAsk
				id = domain.NewID("sevt")
				askIDs = append(askIDs, id)
			}
			fields["evaluated_permission"] = perm
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, false, nil, err
		}
		batch = append(batch, events.NewEvent{ID: id, Type: typ, Payload: payload})
	}
	return batch, platform, askIDs, nil
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
func (b *Brain) settleTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, turn *turnResult, toolKind map[string]domain.EventType, policy map[string]domain.PermissionPolicyType, watermark int64) error {
	err := b.commitTurn(ctx, sid, item, span, turn, toolKind, policy, watermark)
	span.Finish(ctx, false, err)
	if err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	return nil
}

func (b *Brain) commitTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, turn *turnResult, toolKind map[string]domain.EventType, policy map[string]domain.PermissionPolicyType, watermark int64) error {
	head, platform, askIDs, err := turnEvents(turn, toolKind, policy)
	if err != nil {
		return err
	}
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
	opts := events.AppendOptions{
		AddUsage:             &usage,
		MarkProcessedThrough: watermark,
	}

	if turn.stopReason == "tool_use" {
		if len(askIDs) > 0 {
			// A confirmation gate: at least one intent's policy is always_ask.
			// The whole turn suspends — the session idles with a
			// requires_action stop_reason naming the ask events, and NO
			// tool_exec is enqueued, so even the allow-policy tools wait. The
			// session resumes when a user.tool_confirmation resolves the last
			// ask (the API flips idle→running and enqueues the tool_exec that
			// runs the allowed tools plus the confirmed ones; a denial is
			// pre-answered with an error result). Resolving fewer than all
			// re-emits status_idle with the remainder — that is the API's job,
			// on the confirmation POST. Like the running-suspend below, this
			// commits under the lock with no chain-or-idle decision: the
			// session is genuinely blocked on human input, and any mid-turn
			// message stays unprocessed and replays when the gate clears.
			stop, err := json.Marshal(map[string]any{"stop_reason": map[string]any{
				"type": "requires_action", "event_ids": askIDs,
			}})
			if err != nil {
				return err
			}
			head = append(head, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: stop})
			idle := domain.SessionIdle
			opts.SetStatus = &idle
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Complete(ctx, tx, item)
			}
			return b.commitUnderLock(ctx, sid, head, opts)
		}

		// Suspend: the session stays running (awaiting a tool is still
		// working, not awaiting input) and the turn resumes when the full
		// result set is in — the control plane's trigger fires on the
		// completing result. Nothing can be chained here: the intents
		// commit in THIS transaction, and a result may only reference a
		// committed tool use, so none of them is answered yet. A result
		// for an earlier intent that landed mid-turn is not lost either —
		// it stays unprocessed and the resuming turn replays it.
		//
		// If any intent is a platform-executed built-in tool, enqueue the
		// tool_exec item in the same commit so an executor picks it up. A
		// turn of only client-executed custom tools enqueues nothing — the
		// client posts user.custom_tool_result and the control plane's
		// trigger schedules the resume.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			if platform {
				if _, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.ToolExec); err != nil {
					return err
				}
			}
			return nil
		}
		return b.commitUnderLock(ctx, sid, head, opts)
	}

	// end_turn (and everything else — max_tokens, stop_sequence — treated
	// as a completed turn in v1).
	return b.settle(ctx, sid, item, watermark, opts, func(chained bool) ([]events.NewEvent, error) {
		if chained {
			return head, nil
		}
		payload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
		if err != nil {
			return nil, err
		}
		return append(head, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: payload}), nil
	})
}

// settle is the one place a finished turn decides its own end: under the
// session row lock it asks whether input arrived mid-turn, lets the caller
// build the events that outcome calls for, and commits them together with
// the status, the watermark, and the work item's fate. Chaining hands our
// own item back to the queue (a fresh Enqueue would be suppressed by this
// very item's live slot) and leaves the session running; idling completes
// the item. Both success and failure settle here so the two can never drift.
func (b *Brain) settle(ctx context.Context, sid domain.ID, item *queue.Item, watermark int64,
	opts events.AppendOptions, build func(chained bool) ([]events.NewEvent, error)) error {

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
	// anything (corrupt state): chaining on the session's own unprocessed
	// events would loop the same failure forever.
	chained := false
	if watermark > 0 {
		if chained, err = pendingInput(ctx, tx, sid, watermark); err != nil {
			return err
		}
	}
	batch, err := build(chained)
	if err != nil {
		return err
	}
	if chained {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	} else {
		idle := domain.SessionIdle
		opts.SetStatus = &idle
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Complete(ctx, tx, item)
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

// commitUnderLock commits a batch and its options with the session row
// locked first, for the settlement that has no chain-or-idle decision.
func (b *Brain) commitUnderLock(ctx context.Context, sid domain.ID, batch []events.NewEvent, opts events.AppendOptions) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
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

// failTurn records a model-side or deterministic failure on the log. If no
// input is pending past the watermark, the session idles with
// retries_exhausted (v1 has no automatic retry budget — documented in
// docs/DIVERGENCES.md); input that arrived mid-turn instead chains a fresh turn, so a
// failed request cannot strand an accepted message on an idle session. Span
// end, error, status, and item fate commit atomically under the session
// lock, with the lease proof, exactly like a successful settle.
func (b *Brain) failTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg string) error {
	err := b.commitFailure(ctx, sid, item, span, watermark, msg)
	if span != nil {
		span.Finish(ctx, true, err)
	}
	return err
}

func (b *Brain) commitFailure(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg string) error {
	var head []events.NewEvent
	if span != nil {
		endEv, err := span.EndEvent(true, domain.ModelUsage{})
		if err != nil {
			return err
		}
		head = append(head, endEv)
	}

	return b.settle(ctx, sid, item, watermark, events.AppendOptions{MarkProcessedThrough: watermark},
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
			batch := append(head, events.NewEvent{Type: domain.EventSessionError, Payload: errPayload})
			if chained {
				return batch, nil
			}
			idlePayload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "retries_exhausted"}})
			if err != nil {
				return nil, err
			}
			return append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: idlePayload}), nil
		})
}
