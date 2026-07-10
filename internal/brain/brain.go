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
	// LeaseTTL is the work-item lease; it must comfortably exceed one
	// provider round trip (the lease is re-extended mid-stream).
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
		// expiry hands it to another brain. Our appends stand (the log is
		// append-only); duplicated events are the documented cost of a
		// lease sized below a real turn.
		return true, fmt.Errorf("session %s: %w", item.SessionID, err)
	}
	return true, nil
}

func (b *Brain) runTurn(ctx context.Context, item *queue.Item) error {
	sid := item.SessionID

	var agentJSON []byte
	var status string
	var archivedAt *time.Time
	err := b.pool.QueryRow(ctx,
		`SELECT resolved_agent, status, archived_at FROM sessions WHERE id = $1`, sid.String()).
		Scan(&agentJSON, &status, &archivedAt)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if status != string(domain.SessionRunning) || archivedAt != nil {
		// Stale work: the session moved on — it settled idle and then the
		// settling brain lost the race to complete its item, or it was
		// archived (archiving freezes status, so the column alone can't
		// tell; an archived session rejects every append and would
		// otherwise reclaim-loop forever). Checked BEFORE any recovery
		// emission so a reclaim of finished work never flips an idle
		// session back to running.
		return b.queue.Complete(ctx, b.pool, item)
	}

	if item.Reclaimed {
		// The previous claimant died mid-turn. Surface the recovery on the
		// log, then run the turn normally — replay rebuilds everything.
		running := domain.SessionRunning
		if _, err := b.log.AppendWith(ctx, sid, []events.NewEvent{
			{Type: domain.EventSessionStatusRescheduled},
			{Type: domain.EventSessionStatusRunning},
		}, events.AppendOptions{SetStatus: &running}); err != nil {
			return fmt.Errorf("recovery events: %w", err)
		}
	}

	var agent domain.ResolvedAgent
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		return fmt.Errorf("decode resolved agent: %w", err)
	}

	history, err := b.log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	req, watermark, err := buildRequest(agent, history)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}

	p, err := b.registry.Provider(agent.Model.ID)
	if err != nil {
		// A model with no route is a configuration error, not a transient
		// fault: fail the turn visibly rather than retry forever.
		return b.failTurn(ctx, sid, item, watermark, fmt.Sprintf("no provider for model %q", agent.Model.ID))
	}

	sctx, span, err := b.log.StartModelRequest(ctx, sid)
	if err != nil {
		return fmt.Errorf("span start: %w", err)
	}

	turn, streamErr := b.streamTurn(sctx, sid, item, p, req)
	if streamErr != nil {
		_ = span.End(sctx, true, domain.ModelUsage{})
		return b.failTurn(ctx, sid, item, watermark, streamErr.Error())
	}

	// The buffered agent.message (closing its preview) and the tool-use
	// intents land before the span ends: the SDK accumulator closes all
	// open previews at span.model_request_end.
	var batch []events.NewEvent
	if len(turn.text) > 0 {
		content, err := json.Marshal(map[string]any{"content": turn.text})
		if err != nil {
			return err
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
			return err
		}
		batch = append(batch, events.NewEvent{Type: domain.EventAgentCustomToolUse, Payload: payload})
	}
	if len(batch) > 0 {
		if _, err := b.log.Append(ctx, sid, batch); err != nil {
			return fmt.Errorf("emit turn events: %w", err)
		}
	}
	if err := span.End(sctx, false, turn.usage); err != nil {
		return fmt.Errorf("span end: %w", err)
	}

	return b.settleTurn(ctx, sid, item, turn, watermark)
}

// resumeEventTypes are the inbound events whose arrival resumes a suspended
// turn — the same set the API's tool-result trigger fires on.
var resumeEventTypes = []string{
	string(domain.EventUserToolResult), string(domain.EventUserCustomToolRes),
}

// settleTurn drives the state machine after a successful model response and
// decides the work item's fate in the same transaction as the state it
// writes. That atomicity is the liveness guarantee: the API's triggers and
// this settlement serialize on the session row lock, so a tool result posted
// mid-settle either sees our live item (its enqueue is suppressed, and we see
// the result and requeue) or sees it completed (its enqueue succeeds) — never
// the gap where both sides stand down.
func (b *Brain) settleTurn(ctx context.Context, sid domain.ID, item *queue.Item, turn *turnResult, watermark int64) error {
	opts := events.AppendOptions{
		AddUsage:             &turn.usage,
		MarkProcessedThrough: watermark,
	}

	if turn.stopReason == "tool_use" {
		// Suspend: the turn resumes when the result event arrives (the
		// control plane enqueues the next model_turn on it). Session stays
		// running — awaiting a tool is still working, not awaiting input.
		// A result that already landed (its enqueue was suppressed by our
		// live item) is caught here and chains immediately.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			var arrived bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM events
				  WHERE session_id = $1 AND type = ANY($2) AND processed_at IS NULL AND seq > $3)`,
				sid.String(), resumeEventTypes, watermark).Scan(&arrived); err != nil {
				return err
			}
			if arrived {
				return b.queue.Requeue(ctx, tx, item)
			}
			return b.queue.Complete(ctx, tx, item)
		}
		_, err := b.log.AppendWith(ctx, sid, nil, opts)
		return err
	}

	// end_turn (and everything else — max_tokens, stop_sequence — treated
	// as a completed turn in v1): if input arrived mid-turn, chain straight
	// into the next turn; otherwise idle with end_turn. The pending check
	// and the settle commit share one transaction, session row locked
	// FIRST (the API trigger's own lock-then-decide shape): a user.message
	// committed between an unlocked check and the settle could otherwise
	// be stranded on an idle session.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE session_id = $1 AND type = $2 AND processed_at IS NULL AND seq > $3)`,
		sid.String(), string(domain.EventUserMessage), watermark).Scan(&pending); err != nil {
		return err
	}
	if pending {
		// Chain by handing our own item back to the queue (a fresh Enqueue
		// would be suppressed by this very item's live slot), atomically
		// with the watermark and usage.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
		if _, err := b.log.AppendInTx(ctx, tx, sid, nil, opts); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	idle := domain.SessionIdle
	opts.SetStatus = &idle
	opts.Then = func(ctx context.Context, tx pgx.Tx) error {
		return b.queue.Complete(ctx, tx, item)
	}
	payload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
	if err != nil {
		return err
	}
	if _, err := b.log.AppendInTx(ctx, tx, sid, []events.NewEvent{
		{Type: domain.EventSessionStatusIdle, Payload: payload},
	}, opts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// failTurn records a model-side failure on the log and idles the session
// with retries_exhausted. v1 has no automatic retry budget (documented in
// STATE.md): one failed request ends the turn, and the next user.message
// starts a fresh one.
func (b *Brain) failTurn(ctx context.Context, sid domain.ID, item *queue.Item, watermark int64, msg string) error {
	errPayload, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "model_request_failed_error", "message": msg,
		"retry_status": map[string]any{"type": "exhausted"},
	}})
	if err != nil {
		return err
	}
	idlePayload, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "retries_exhausted"}})
	if err != nil {
		return err
	}
	idle := domain.SessionIdle
	_, err = b.log.AppendWith(ctx, sid, []events.NewEvent{
		{Type: domain.EventSessionError, Payload: errPayload},
		{Type: domain.EventSessionStatusIdle, Payload: idlePayload},
	}, events.AppendOptions{
		SetStatus:            &idle,
		MarkProcessedThrough: watermark,
		Then: func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Complete(ctx, tx, item)
		},
	})
	return err
}
