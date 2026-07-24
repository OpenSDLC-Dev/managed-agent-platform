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
	"log/slog"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the executor's OTel instrumentation scope.
const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"

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
	return c
}

// Executor consumes tool_exec work over one Postgres pool and one sandbox
// provider.
type Executor struct {
	pool     *pgxpool.Pool
	log      *events.Log
	queue    *queue.Queue
	provider sandbox.Provider
	// blobs sources skill archives for materialization; nil (a storage-less
	// deploy) skips materialization with a log line, never a fault.
	blobs blob.Store
	cfg   Config
	// onFault, when set, receives every per-item fault. Left nil in production
	// (the queue's reclaim is the recovery); tests set it to observe faults.
	onFault func(*queue.Item, error)
}

func New(pool *pgxpool.Pool, log *events.Log, q *queue.Queue, provider sandbox.Provider, blobs blob.Store, cfg Config) *Executor {
	return &Executor{pool: pool, log: log, queue: q, provider: provider, blobs: blobs, cfg: cfg.withDefaults()}
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
			if err := sleep(ctx, e.cfg.PollInterval); err != nil {
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
	// A fault is reported by process itself, from inside its span — see report.
	_ = e.process(ctx, item)
	return true, nil
}

// process runs one tool_exec item to completion.
func (e *Executor) process(ctx context.Context, item *queue.Item) (err error) {
	// Parent the work on the turn that enqueued it, whose trace context the
	// queue captured onto the item — so a session's model turns and the tools
	// they trigger are one trace, the same guarantee the BYOC worker already
	// gets from the poll response.
	//
	// The span opens on a claimed item and closes when the item is done with,
	// which is what a consumer span stands for: the handling of one message,
	// end to end. Both edges matter. Everything below can fail — the session
	// lookup, the tools, the commit — and every one of those leaves the item for
	// reclaim to retry next lease period, so a span that covered only the middle
	// would omit exactly the recurring faults an operator opens the trace to
	// find. The tools' own timing is toolset's duration metric, not this span's
	// business.
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(
		telemetry.Extract(ctx, item.TraceContext), "tool_exec",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("session.id", item.SessionID.String()),
			attribute.String("work.id", item.ID.String()),
		))
	// Every failure this function reports is the platform's own — a tool the
	// model can read and recover from (a missing file, a nonzero exit) never
	// reaches here: it rides the log verbatim and the toolset metric's
	// error.type, and erroring the span for it would light up every trace view
	// on ordinary agent behaviour.
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			// Inside the span, so the record lands on the tool_exec the fault is
			// about — that red span is where an operator asks for it.
			e.report(ctx, item, err)
		}
		span.End()
	}()

	// Drain work for a session that is no longer live before doing anything
	// expensive. Archiving freezes the status and makes every append the run
	// would commit fail, so without this the item reclaim-loops forever,
	// re-running its tools each lease period; this mirrors the brain's
	// claimLiveSession. Loading the egress policy under the same lock keeps it
	// to one round trip.
	sess, live, err := e.sessionForRun(ctx, item)
	if err != nil || !live {
		return err
	}

	// Keep the lease alive from before provisioning through the tool run: an
	// image pull can be slow, and a fixed TTL would otherwise let the lease
	// lapse mid-provision and a second executor reclaim and double-run the
	// session's tools. Provisioning and every tool run happen under kctx, so
	// losing the lease cancels the work.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)

	results, faultErr, runErr := e.provisionAndRun(kctx, item, sess)
	if kerr := keeper.Close(); kerr != nil {
		// The lease is gone — another executor may already own this item.
		// Nothing of ours may commit; the results we ran are re-derived on the
		// reclaiming pass (a committed result is never re-run).
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}

	// Commit the results, the resume, and the item's fate together under the
	// session lock. The item is completed only when every tool ran: a backend
	// fault leaves it live so a reclaim retries the tools still unanswered
	// (the ones that did run are now committed and are skipped).
	complete := faultErr == nil
	opts := events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			// Every state write this claimant makes must prove it still owns the
			// item. The complete path proves it through Complete; the fault path
			// commits partial results with nothing else, so it asserts the lease
			// explicitly — otherwise a claim lost while blocked on the session
			// lock could still commit a result a reclaiming executor also writes,
			// duplicating it on the append-only log.
			if !complete {
				if err := e.queue.Assert(ctx, tx, item); err != nil {
					return err
				}
			}
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

// provisionAndRun provisions the session's sandbox and runs its unanswered
// tools under ctx (the lease-kept context). It returns the result events to
// append, the first backend fault a tool hit (nil if every tool ran), and a
// setup error from provisioning or reading the log — which stops the item with
// nothing committed, distinct from a tool fault, which commits what did run.
func (e *Executor) provisionAndRun(ctx context.Context, item *queue.Item, sess sessionRun) ([]events.NewEvent, error, error) {
	env, err := e.sandboxEnv(ctx, item.SessionID, sess.vaultIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve vault credentials: %w", err)
	}
	sb, err := e.provider.Provision(ctx, sandbox.Spec{
		SessionID:  item.SessionID,
		Image:      e.cfg.Image,
		Workdir:    e.cfg.Workdir,
		Networking: sess.networking,
		Env:        env,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("provision sandbox: %w", err)
	}
	e.materializeSkills(ctx, sb, item.SessionID, sess.skills)
	e.materializeFiles(ctx, sb, item.SessionID, sess.files)
	uses, err := e.unansweredToolUses(ctx, item.SessionID)
	if err != nil {
		return nil, nil, err
	}
	results, faultErr := e.runTools(ctx, sb, item.SessionID, uses)
	return results, faultErr, nil
}

// sandboxEnv resolves the session's attached vaults into the environment
// variables the sandbox is provisioned with: one secret_name=placeholder entry
// per active environment_variable credential (vaultresolve). The placeholders
// are opaque and inert on their own — the per-session gate substitutes the real
// secrets at egress time (a later slice). No attached vaults, or none carrying
// env-var credentials, yields a nil map: an ordinary sandbox.
//
// Placeholders are derived per (session, secret_name), so a re-provision of the
// same session resolves the identical tokens — matching what the create-bound
// Spec.Env already holds (Provision adopts a running sandbox without re-applying
// a changed Env) rather than drifting to fresh values the gate could no longer
// substitute.
//
// A credential whose secret_name is not a valid environment-variable name
// cannot be injected as an env var (ValidateEnv would fail the whole provision
// and the item would reclaim-loop), so it is skipped here rather than delivered
// — the "a bad credential surfaces [later] and does not block the session" arm
// of the resolution model. Only a resolution I/O error faults the item, which
// then retries on reclaim like any other transient failure.
func (e *Executor) sandboxEnv(ctx context.Context, sessionID domain.ID, vaultIDs []string) (map[string]string, error) {
	bindings, err := vaultresolve.Bindings(ctx, e.pool, sessionID.String(), vaultIDs)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	for _, b := range bindings {
		if !sandbox.ValidEnvName(b.SecretName) {
			continue
		}
		if env == nil {
			env = make(map[string]string, len(bindings))
		}
		env[b.SecretName] = b.Placeholder
	}
	return env, nil
}

// runTools runs each unanswered tool use in order, returning the result events
// to append and the first backend fault encountered (nil if all ran). A tool
// that fails at the tool level (missing file, nonzero exit) still yields a
// result event — that is the model's to see; only a backend fault (sandbox
// gone, daemon unreachable) stops the set and leaves the rest unanswered.
func (e *Executor) runTools(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, uses []toolUse) ([]events.NewEvent, error) {
	// Workdir must match the one the sandbox was provisioned with, so the file
	// tools resolve a relative path against the same directory bash runs in.
	// Empty resolves to sandbox.DefaultWorkdir on both sides.
	runner := toolset.Runner{Sandbox: sb, Session: sid, Workdir: e.cfg.Workdir}
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
// tool_use_id + content blocks + is_error, matching the wire's
// BetaManagedAgentsAgentToolResultEvent and what replay reads back. Empty
// output (a read of an empty file) becomes an empty content array, never a text
// block with an empty string — a Messages endpoint rejects an empty text block,
// and that request is what the brain replays every resume, wedging the session.
func toolResultEvent(useID domain.ID, res toolset.Result) (events.NewEvent, error) {
	content := []map[string]any{}
	if res.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": res.Content})
	}
	payload, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     content,
		"is_error":    res.IsError,
	})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventAgentToolResult, Payload: payload}, nil
}

// sessionRun is the per-run session state sessionForRun loads under the row
// lock: the egress policy, the snapshot's skills and file mounts, and the
// attached vault ids that drive credential resolution.
type sessionRun struct {
	networking domain.Networking
	skills     []skillRef
	files      []fileRef
	vaultIDs   []string
}

// sessionForRun loads the session's egress policy, its snapshot's skills
// references, its file-mount resources, and its attached vault ids under the
// session's row lock, and reports whether the session is still live for tool
// execution. A session that is not running, or has been archived, is stale: its
// tool_exec item is completed here and false is returned, so a dead session
// cannot reclaim-loop (every append the run would make is rejected). A session
// that no longer exists took its cascade-deleted work item with it, so there is
// nothing to drain. Mirrors the brain's claimLiveSession. Reading skills,
// resources, and vault ids here keeps the run to one session read — a second,
// later read would add a transient-failure point that faults the whole item.
func (e *Executor) sessionForRun(ctx context.Context, item *queue.Item) (sessionRun, bool, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return sessionRun{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var archivedAt *time.Time
	var configJSON, agentJSON, resourcesJSON []byte
	var vaultIDs []string
	err = tx.QueryRow(ctx,
		`SELECT s.status, s.archived_at, e.config, s.resolved_agent, s.resources, s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 FOR UPDATE OF s`,
		item.SessionID.String()).Scan(&status, &archivedAt, &configJSON, &agentJSON, &resourcesJSON, &vaultIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionRun{}, false, nil
	}
	if err != nil {
		return sessionRun{}, false, err
	}

	if status != string(domain.SessionRunning) || archivedAt != nil {
		if err := e.queue.Complete(ctx, tx, item); err != nil {
			return sessionRun{}, false, err
		}
		return sessionRun{}, false, tx.Commit(ctx)
	}

	var cfg domain.EnvironmentConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return sessionRun{}, false, err
	}
	var agent struct {
		Skills []skillRef `json:"skills"`
	}
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		return sessionRun{}, false, err
	}
	var resources []fileRef
	if err := json.Unmarshal(resourcesJSON, &resources); err != nil {
		return sessionRun{}, false, err
	}
	return sessionRun{
		networking: cfg.Networking,
		skills:     agent.Skills,
		files:      resources,
		vaultIDs:   vaultIDs,
	}, true, tx.Commit(ctx)
}

// report is where per-item faults surface. The queue's reclaim is the recovery
// — the item keeps its lease until it lapses, then another claim retries it —
// but the fault is logged so an operator debugging "the tools never run" (a
// Docker daemon down faults every item) sees it rather than a silent stall.
// onFault is nil in production; tests set it to observe faults.
//
// Called from process's deferred exit rather than from step, so ctx still
// carries the tool_exec span. Reporting from step would work and correlate to
// the right *trace*, but only to the enqueuing turn's span — leaving the red
// tool_exec span, the one an operator clicks, with no log under it.
func (e *Executor) report(ctx context.Context, item *queue.Item, err error) {
	slog.ErrorContext(ctx, "executor: tool_exec item faulted, lease left to expire",
		"item", item.ID, "session", item.SessionID, "error", err)
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
