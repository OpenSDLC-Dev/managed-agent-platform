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
	if err := b.runTurn(ctx, item); err != nil {
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

func (b *Brain) runTurn(ctx context.Context, item *queue.Item) error {
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

	sctx, span, err := b.log.StartModelRequest(ctx, sid)
	if err != nil {
		return fmt.Errorf("span start: %w", err)
	}

	kctx, keeper := b.keepLease(sctx, item)
	turn, streamErr := b.streamTurn(kctx, sid, p, req)
	if err := keeper.close(); err != nil {
		// The lease is gone or unmaintainable: another brain may own the
		// turn already. Nothing of ours may commit — abandon quietly.
		span.Finish(true, err)
		return fmt.Errorf("lease keeper: %w", err)
	}
	if streamErr != nil {
		var ie infraError
		if errors.As(streamErr, &ie) {
			span.Finish(true, streamErr)
			return streamErr
		}
		return b.failTurn(ctx, sid, item, span, watermark, streamErr.Error())
	}
	if turn.stopReason == "tool_use" && len(turn.toolUses) == 0 {
		// A tool_use stop with no tool blocks has nothing to wait for and
		// nothing to chain — settling either way would wedge or spin.
		return b.failTurn(ctx, sid, item, span, watermark, "model stopped for tool_use without any tool_use block")
	}

	return b.settleTurn(ctx, sid, item, span, turn, watermark)
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
				if err := b.queue.Extend(kctx, item, b.cfg.LeaseTTL); err != nil {
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
// tool call. Text blocks that ended empty are dropped — a "text" block
// without its text field is malformed on the wire, and replay would feed it
// back to the model on every future turn.
func turnEvents(turn *turnResult) ([]events.NewEvent, error) {
	var batch []events.NewEvent
	var text []domain.ContentBlock
	for _, blk := range turn.text {
		if blk.Text != "" {
			text = append(text, blk)
		}
	}
	if len(text) > 0 {
		content, err := json.Marshal(map[string]any{"content": text})
		if err != nil {
			return nil, err
		}
		batch = append(batch, events.NewEvent{
			ID: turn.messageEventID, Type: domain.EventAgentMessage, Payload: content,
		})
	}
	for _, tu := range turn.toolUses {
		payload, err := json.Marshal(map[string]any{
			"name": tu.Name, "input": tu.Input, "session_thread_id": nil,
		})
		if err != nil {
			return nil, err
		}
		batch = append(batch, events.NewEvent{Type: domain.EventAgentCustomToolUse, Payload: payload})
	}
	return batch, nil
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
func (b *Brain) settleTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, turn *turnResult, watermark int64) error {
	err := b.commitTurn(ctx, sid, item, span, turn, watermark)
	span.Finish(false, err)
	if err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	return nil
}

func (b *Brain) commitTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, turn *turnResult, watermark int64) error {
	batch, err := turnEvents(turn)
	if err != nil {
		return err
	}
	endEv, err := span.EndEvent(false, turn.usage)
	if err != nil {
		return err
	}
	batch = append(batch, endEv)
	opts := events.AppendOptions{
		AddUsage:             &turn.usage,
		MarkProcessedThrough: watermark,
	}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}

	if turn.stopReason == "tool_use" {
		// Suspend: the session stays running (awaiting a tool is still
		// working, not awaiting input) and the turn resumes when the full
		// result set is in — the control plane's trigger fires on the
		// completing result. If every intent is already answered by the
		// time this commits (a producer that appends results outside the
		// API — slice 6's executor — can race the settle), chain
		// immediately: that result's enqueue was suppressed by our item.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			unanswered, err := events.HasUnansweredToolUse(ctx, tx, sid, nil)
			if err != nil {
				return err
			}
			if !unanswered {
				return b.queue.Requeue(ctx, tx, item)
			}
			return b.queue.Complete(ctx, tx, item)
		}
	} else {
		// end_turn (and everything else — max_tokens, stop_sequence —
		// treated as a completed turn in v1): if input arrived mid-turn,
		// chain straight into the next turn; otherwise idle with end_turn.
		pending, err := pendingInput(ctx, tx, sid, watermark)
		if err != nil {
			return err
		}
		if pending {
			// Chain by handing our own item back to the queue (a fresh
			// Enqueue would be suppressed by this very item's live slot).
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Requeue(ctx, tx, item)
			}
		} else {
			idle := domain.SessionIdle
			opts.SetStatus = &idle
			payload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
			if err != nil {
				return err
			}
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: payload})
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Complete(ctx, tx, item)
			}
		}
	}

	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// failTurn records a model-side or deterministic failure on the log. If no
// input is pending past the watermark, the session idles with
// retries_exhausted (v1 has no automatic retry budget — documented in
// STATE.md); input that arrived mid-turn instead chains a fresh turn, so a
// failed request cannot strand an accepted message on an idle session. Span
// end, error, status, and item fate commit atomically under the session
// lock, with the lease proof, exactly like a successful settle.
func (b *Brain) failTurn(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg string) error {
	err := b.commitFailure(ctx, sid, item, span, watermark, msg)
	if span != nil {
		span.Finish(true, err)
	}
	return err
}

func (b *Brain) commitFailure(ctx context.Context, sid domain.ID, item *queue.Item, span *events.ModelRequest, watermark int64, msg string) error {
	var batch []events.NewEvent
	if span != nil {
		endEv, err := span.EndEvent(true, domain.ModelUsage{})
		if err != nil {
			return err
		}
		batch = append(batch, endEv)
	}
	errPayload, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "model_request_failed_error", "message": msg,
		"retry_status": map[string]any{"type": "exhausted"},
	}})
	if err != nil {
		return err
	}
	batch = append(batch, events.NewEvent{Type: domain.EventSessionError, Payload: errPayload})
	opts := events.AppendOptions{MarkProcessedThrough: watermark}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}

	// Only a turn that consumed input can chain on pending input: a
	// watermark of zero means the failure hit before replay even resolved
	// (corrupt state), where requeueing on the session's own unprocessed
	// events would loop the same failure forever.
	pending := false
	if watermark > 0 {
		if pending, err = pendingInput(ctx, tx, sid, watermark); err != nil {
			return err
		}
	}
	if pending {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	} else {
		idlePayload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "retries_exhausted"}})
		if err != nil {
			return err
		}
		batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: idlePayload})
		idle := domain.SessionIdle
		opts.SetStatus = &idle
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Complete(ctx, tx, item)
		}
	}

	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
