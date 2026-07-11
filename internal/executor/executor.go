// Package executor is the hands' consumer: it pulls tool_exec work from the
// queue, runs the built-in toolset inside the session's sandbox, and appends
// the agent.tool_result events the brain resumes on. Platform-managed cloud
// and customer BYOC are the same pull protocol at two deployment points; this
// is the platform-managed one, embedding the Docker sandbox provider.
//
// The loop mirrors the brain's: Claim the oldest tool_exec item (reclaiming an
// expired lease), do the work, hand the item back. The brain, when a turn stops
// for a built-in tool, commits the agent.tool_use intents and enqueues one
// tool_exec item; this executor answers every unanswered agent.tool_use for the
// session, then — once the set is complete — enqueues the model_turn that wakes
// the brain to continue. The result append, the resume enqueue, and the item's
// completion are one transaction under the session row lock, so a concurrent
// trigger never sees a gap.
//
// At-most-once is the queue's lease, not a marker in the sandbox (which is
// agent-writable and disposable — see internal/sandbox/shell). A lease keeper
// holds the claim while tools run so two executors never run one session's
// tools at once; a crash mid-run lets the lease lapse, and the reclaiming
// executor re-runs only the still-unanswered tools — a committed result is
// never re-run, so a tool's result is exactly-once even though a non-idempotent
// command can run more than once across a crash. That residue is inherent to a
// disposable sandbox and is documented, not solved here.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config tunes the loop. Image is the sandbox base image (a deployment choice —
// the wire's environment config has no image field). LeaseTTL must comfortably
// exceed toolset.MaxTimeout: the lease keeper renews at TTL/3 while a tool runs,
// but the TTL is also the window a crashed executor's work waits before another
// reclaims it.
type Config struct {
	Image        string
	Workdir      string
	LeaseTTL     time.Duration
	PollInterval time.Duration
	BlockOnEmpty time.Duration
}

func (c Config) withDefaults() Config {
	if c.Image == "" {
		c.Image = "debian:stable-slim"
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 15 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.BlockOnEmpty <= 0 {
		c.BlockOnEmpty = c.PollInterval
	}
	return c
}

// Executor consumes tool_exec work over one Postgres pool and one sandbox
// provider.
type Executor struct {
	pool     *pgxpool.Pool
	log      *events.Log
	queue    *queue.Queue
	provider sandbox.Provider
	cfg      Config
	// onFault, when set, receives every per-item fault. Left nil in production
	// (the queue's reclaim is the recovery); tests set it to observe faults.
	onFault func(*queue.Item, error)
}

func New(pool *pgxpool.Pool, log *events.Log, q *queue.Queue, provider sandbox.Provider, cfg Config) *Executor {
	return &Executor{pool: pool, log: log, queue: q, provider: provider, cfg: cfg.withDefaults()}
}

// Run polls until the context is cancelled. It claims one tool_exec item at a
// time; an error processing one item is logged by returning it up to the caller
// only for a fatal claim failure — a per-item fault is swallowed so the loop
// keeps serving other sessions, and the faulted item is reclaimed after its
// lease lapses.
func (e *Executor) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := e.step(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if !worked {
			if err := sleep(ctx, e.cfg.BlockOnEmpty); err != nil {
				return nil
			}
		}
	}
}

// step claims and processes at most one item, reporting whether it found work.
// A per-item fault (sandbox gone, database hiccup, lost lease) is not fatal to
// the loop: the item keeps its lease until it lapses, then another claim
// reclaims and retries it. Only a claim failure is returned up.
func (e *Executor) step(ctx context.Context) (bool, error) {
	item, err := e.queue.Claim(ctx, queue.ToolExec, e.cfg.LeaseTTL)
	if err != nil {
		return false, err
	}
	if item == nil {
		return false, nil
	}
	if err := e.process(ctx, item); err != nil {
		e.report(item, err)
	}
	return true, nil
}

// process runs one tool_exec item to completion.
func (e *Executor) process(ctx context.Context, item *queue.Item) error {
	net, err := e.sessionNetworking(ctx, item.SessionID)
	if err != nil {
		return fmt.Errorf("session networking: %w", err)
	}

	sb, err := e.provider.Provision(ctx, sandbox.Spec{
		SessionID:  item.SessionID,
		Image:      e.cfg.Image,
		Workdir:    e.cfg.Workdir,
		Networking: net,
	})
	if err != nil {
		return fmt.Errorf("provision sandbox: %w", err)
	}

	uses, err := e.unansweredToolUses(ctx, item.SessionID)
	if err != nil {
		return err
	}

	// Keep the lease alive while tools run: a single tool can take
	// toolset.MaxTimeout, and a set of them far longer, so a fixed TTL would
	// otherwise let a second executor reclaim and double-run the session's
	// tools mid-flight.
	kctx, keeper := e.keepLease(ctx, item)
	results, faultErr := e.runTools(kctx, sb, item.SessionID, uses)
	if kerr := keeper.close(); kerr != nil {
		// The lease is gone — another executor may already own this item.
		// Nothing of ours may commit; the results we ran are re-derived on the
		// reclaiming pass (a committed result is never re-run).
		return fmt.Errorf("lease keeper: %w", kerr)
	}

	// Commit the results, the resume, and the item's fate together under the
	// session lock. The item is completed only when every tool ran: a backend
	// fault leaves it live so a reclaim retries the tools still unanswered
	// (the ones that did run are now committed and are skipped).
	complete := faultErr == nil
	opts := events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			unanswered, err := events.HasUnansweredToolUse(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if !unanswered {
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
					return err
				}
			}
			if complete {
				return e.queue.Complete(ctx, tx, item)
			}
			return nil
		},
	}
	if _, err := e.log.AppendWith(ctx, item.SessionID, results, opts); err != nil {
		return fmt.Errorf("append tool results: %w", err)
	}
	return faultErr
}

// runTools runs each unanswered tool use in order, returning the result events
// to append and the first backend fault encountered (nil if all ran). A tool
// that fails at the tool level (missing file, nonzero exit) still yields a
// result event — that is the model's to see; only a backend fault (sandbox
// gone, daemon unreachable) stops the set and leaves the rest unanswered.
func (e *Executor) runTools(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, uses []toolUse) ([]events.NewEvent, error) {
	runner := toolset.Runner{Sandbox: sb, Session: sid}
	var results []events.NewEvent
	for _, u := range uses {
		res, err := runner.Run(ctx, u.id, u.name, u.input)
		if err != nil {
			// Backend fault: stop here. The results gathered so far are still
			// appended so a retry does not re-run them; this tool and any after
			// it stay unanswered for the reclaim.
			return results, fmt.Errorf("tool %s (%s): %w", u.name, u.id, err)
		}
		ev, err := toolResultEvent(u.id, res)
		if err != nil {
			return results, err
		}
		results = append(results, ev)
	}
	return results, nil
}

// toolResultEvent renders a Result as an agent.tool_result event body:
// tool_use_id + a single text content block + is_error, matching the wire's
// BetaManagedAgentsAgentToolResultEvent and what replay reads back.
func toolResultEvent(useID domain.ID, res toolset.Result) (events.NewEvent, error) {
	payload, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     []map[string]any{{"type": "text", "text": res.Content}},
		"is_error":    res.IsError,
	})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventAgentToolResult, Payload: payload}, nil
}

// sessionNetworking loads the egress policy the session's environment sets, so
// the sandbox is provisioned with the same isolation the environment declares.
func (e *Executor) sessionNetworking(ctx context.Context, sid domain.ID) (domain.Networking, error) {
	var configJSON []byte
	err := e.pool.QueryRow(ctx,
		`SELECT e.config FROM sessions s JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`,
		sid.String()).Scan(&configJSON)
	if err != nil {
		return domain.Networking{}, err
	}
	var cfg domain.EnvironmentConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return domain.Networking{}, err
	}
	return cfg.Networking, nil
}

// report is where per-item faults surface. The queue's reclaim, not a log
// line, is the recovery, so production leaves onFault nil.
func (e *Executor) report(item *queue.Item, err error) {
	if e.onFault != nil {
		e.onFault(item, err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
