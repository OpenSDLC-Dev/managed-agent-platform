package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the worker's OTel instrumentation scope.
const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/worker"

// noHeartbeat is the wire sentinel a worker's first heartbeat sends as
// expected_last_heartbeat to claim an unclaimed lease; every later heartbeat
// echoes the server's prior last_heartbeat value.
const noHeartbeat = "NO_HEARTBEAT"

// pollBlockMs is the long-poll window the worker requests. The reference sends
// 999 (the server's cap), and our control plane holds an empty poll open up to
// the window, ending the wait early on a work-items NOTIFY (#74);
// EmptyPollSleep still spaces the polls after an empty answer, as the
// reference client's own jitter sleep does.
const pollBlockMs = 999

const (
	// heartbeatFloor/Cap bound the derived heartbeat cadence (server TTL / 2),
	// matching the reference; stopTimeout bounds the final force-stop.
	heartbeatFloor = 1 * time.Second
	heartbeatCap   = 30 * time.Second
	stopTimeout    = 10 * time.Second
)

// Config configures the BYOC worker lease loop. The worker owns its sandbox
// shape (Image/Workdir/Networking) rather than loading a per-session egress
// policy — a self_hosted worker runs on the customer's own compute and the wire
// exposes no per-session networking to it, so this mirrors the platform
// executor's Config, whose sandbox settings are likewise a deployment choice.
type Config struct {
	EnvironmentID string
	// WorkerID identifies this worker for the control plane's poll metrics
	// (Anthropic-Worker-ID). Auto-generated as "<hostname>-<random>" when empty,
	// as the reference does.
	WorkerID   string
	Image      string
	Workdir    string
	Networking domain.Networking
	// Hardening caps every sandbox this worker provisions (#65) — the BYOC twin
	// of the platform executor's. cmd/worker reads the same SANDBOX_* variables
	// the executor does; the zero value hardens nothing, so a test builds a
	// Config without acquiring a limit it did not ask for.
	Hardening sandbox.Hardening
	// EmptyPollSleep is the wait between empty polls (default 1s), on top of
	// the server-side block_ms hold — kept because the reference client
	// sleeps between empty polls the same way with block_ms set, so an idle
	// worker's cadence stays wire-identical: one poll per block + sleep.
	EmptyPollSleep time.Duration
	// HeartbeatInterval, when > 0, fixes the heartbeat cadence; otherwise it is
	// derived from each heartbeat response's ttl (ttl/2, clamped to
	// [heartbeatFloor, heartbeatCap]) as the reference does. Tests set a small
	// value; production leaves it 0.
	HeartbeatInterval time.Duration
	// StallTimeout bounds how long a run may report no progress before the
	// heartbeat gives up on it — cancelling the run and, deliberately, beating
	// no more, so the lease lapses server-side and the control plane re-offers
	// the item (#383). It is the BYOC twin of the platform executor's knob, for
	// the same wedge: a sandbox call that never returns leaves this worker
	// heartbeating a run that will never finish, so the item is neither
	// progressing nor reclaimable. Progress is a step finishing, not a byte
	// moving, so the budget must clear the longest single step a healthy run
	// takes: toolset.MaxTimeout for one `bash`, a cold image pull, a large
	// mount. No two steps share one interval: the liveness read is reported at
	// the top of RunSessionTools, ahead of the paging unanswered-use scan, so
	// two bounded control-plane calls do not add up inside one silence.
	// It bounds *silence*, never duration — a run that keeps finishing
	// steps runs as long as it likes. Not an off switch: 0 takes the default
	// (WORKER_STALL_TIMEOUT).
	StallTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.WorkerID == "" {
		c.WorkerID = defaultWorkerID()
	}
	if c.Image == "" {
		// Match the platform executor's default; an empty Workdir resolves to the
		// sandbox default downstream, as it does there.
		c.Image = "debian:stable-slim"
	}
	if c.EmptyPollSleep <= 0 {
		c.EmptyPollSleep = time.Second
	}
	// The executor's default, from the same constant rather than a copy of its
	// number: the two binaries answer the same wedge and must not drift apart
	// because one of them was edited.
	if c.StallTimeout <= 0 {
		c.StallTimeout = toolset.DefaultStallBudget
	}
	return c
}

// Worker is the BYOC lease loop, the self_hosted twin of the platform executor.
// It polls the control plane's self_hosted work queue over HTTP, acknowledges an
// item, keeps its lease alive with heartbeats while the C2a tool-exec driver
// runs the session's tools in a local sandbox, and force-stops the item when the
// run ends. One session at a time, mirroring the reference `ant beta:worker`.
type Worker struct {
	client   sdk.Client
	provider sandbox.Provider
	cfg      Config
	// onItemDone, when set, fires after each work item is fully handled —
	// whether it was force-stopped (a genuine finish) or left live for reclaim
	// (a fault). Left nil in production; tests use it to observe that the loop
	// finished with an item without racing on the queue state.
	onItemDone func(workID string)
}

// NewWorker builds a worker over an SDK client (see NewClient) and a local
// sandbox provider (the customer's Docker/K8s).
func NewWorker(client sdk.Client, provider sandbox.Provider, cfg Config) *Worker {
	return &Worker{client: client, provider: provider, cfg: cfg.withDefaults()}
}

// Run polls until ctx is cancelled, handling one work item at a time. A poll
// that fails with an auth error (a bad environment key) is fatal and returns the
// error; any other poll error backs off and retries, so a transient network
// blip does not kill the worker. Cancellation (SIGINT/SIGTERM via the caller's
// signal context) ends the loop with a nil error.
func (w *Worker) Run(ctx context.Context) error {
	fails := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		work, parent, err := w.pollAck(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isAuthError(err) {
				return err
			}
			// Jittered, escalating backoff so a persistently bad item or a
			// down control plane cannot hot-loop this worker, and a recovering
			// fleet does not re-poll in lockstep.
			fails++
			slog.Error("worker: poll failed, backing off", "attempt", fails, "err", err)
			if sleep(ctx, backoff(fails)) != nil {
				return nil
			}
			continue
		}
		fails = 0
		if work == nil {
			// The server already held the poll for block_ms; the jittered sleep
			// on top matches the reference client and spreads a fleet's
			// reconnections rather than synchronizing them.
			if sleep(ctx, jitter(w.cfg.EmptyPollSleep)) != nil {
				return nil
			}
			continue
		}
		w.handleItem(ctx, work, parent)
	}
}

// pollAck polls for the oldest queued item and acknowledges it (queued →
// starting), returning the acked item or nil when the queue is empty. Both a
// poll error and an ack error are returned to Run to back off on.
//
// An ack failure must NOT force-stop the item. A transient ack error leaves the
// item either queued (the ack never applied) or starting (it applied but the
// response was lost); Poll re-offers a queued item once its reservation lapses
// and reclaims a starting item once its startup lease lapses, so either way it
// recovers. Force-stopping it would instead move it to the terminal stopped
// state that no reclaim recovers, permanently stranding the session's
// outstanding tool work over a single hiccup. Run's backoff keeps a genuinely
// un-ackable item from hot-looping. A 404 here is the one non-transient case: an
// ack so delayed that the control plane re-offered the item under a fresh
// identity (#62), which another worker now owns. That is routine hand-off, not a
// fault, so it is dropped as an empty poll rather than returned as an error —
// see the branch below.
func (w *Worker) pollAck(ctx context.Context) (*sdk.BetaSelfHostedWork, map[string]string, error) {
	var resp *http.Response
	work, err := w.client.Beta.Environments.Work.Poll(ctx, w.cfg.EnvironmentID, sdk.BetaEnvironmentWorkPollParams{
		BlockMs:           sdk.Int(pollBlockMs),
		AnthropicWorkerID: sdk.String(w.cfg.WorkerID),
	}, option.WithResponseInto(&resp))
	if err != nil {
		return nil, nil, err
	}
	if work == nil || work.ID == "" {
		return nil, nil, nil
	}
	if _, err := w.client.Beta.Environments.Work.Ack(ctx, work.ID, sdk.BetaEnvironmentWorkAckParams{
		EnvironmentID: w.cfg.EnvironmentID,
	}); err != nil {
		if isStatus(err, 404) {
			// Not a fault, so it must not reach Run's error path: reporting it
			// there would log a control-plane failure at Error and escalate the
			// backoff for what is routine hand-off. Returning no item takes the
			// empty-poll branch — space one poll and try again.
			slog.InfoContext(ctx, "worker: acked item was re-offered to another worker, dropping",
				"work", work.ID, "session", work.Data.ID)
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("ack %s: %w", work.ID, err)
	}
	return work, pollTraceContext(resp), nil
}

// pollTraceContext lifts the enqueue-time W3C trace context the control plane
// stamped on the poll response headers (see the API's pollWork) into a carrier
// the worker parents its tool-exec span on, so the session's model turns and
// this worker's tool runs join one OTel trace across the process boundary. It is
// empty when the item was enqueued with no active span (no header) or on the
// unreachable nil response.
func pollTraceContext(resp *http.Response) map[string]string {
	if resp == nil {
		return nil
	}
	carrier := map[string]string{}
	for _, k := range []string{"traceparent", "tracestate"} {
		if v := resp.Header.Get(k); v != "" {
			carrier[k] = v
		}
	}
	return carrier
}

// itemOutcome is what runItem decided to do with a work item.
type itemOutcome int

const (
	// outcomeReclaim: an uncertain result (liveness unknown, tools faulted with
	// work unanswered, or the run was cancelled) — leave the item live so it can
	// be reclaimed. Once its heartbeat lease lapses, queue.Poll reclaims the
	// stranded starting/active item (resets it to a fresh queued reservation) and
	// a worker re-runs its still-unanswered tools. Force-stopping instead would
	// move it to the terminal stopped state that no reclaim recovers.
	outcomeReclaim itemOutcome = iota
	// outcomeDrain: the session is definitively dead (archived/terminated) — run
	// nothing and force-stop the item. Safe regardless of lease ownership: a dead
	// session's item can disrupt nothing live.
	outcomeDrain
	// outcomeComplete: every tool was answered — force-stop the item, but only if
	// this worker still exclusively owns the lease (see handleItem).
	outcomeComplete
)

// handleItem runs one acked work item under a heartbeat kept alive from before
// the run through the end (so a slow tool cannot let the lease lapse and a second
// worker reclaim the session). The heartbeat is started before the liveness
// check for the same reason the reference starts it first: the poll already
// acked the item, and every moment between the ack and the first heartbeat is a
// window in which the control plane sees no liveness signal.
//
// Force-stop discipline — the worker force-stops only an item it may safely
// stop:
//   - stop requested: the control plane moved the item to stopping, so the stop
//     is one it asked for and finishing it (stopping → stopped) is this worker's
//     job, whatever the run's own outcome — the cancellation that ended the run
//     WAS the wind-down, not a fault. Nothing else would finish it: Poll never
//     re-offers a stopping item, which is also why the item is still exclusively
//     this worker's and stopping it can disrupt no one (#25).
//   - drain: the session is dead, so stopping its item disrupts nothing live.
//   - complete: every tool was answered — force-stop to clear the item (which
//     otherwise lingers active and blocks the session's next tool turn), UNLESS
//     the heartbeat gave up ownership — it observed the lease lost, or gave it up
//     on a stall (#383) — in which case another worker may now own the item and
//     stopping it could terminate that worker's run. This
//     lease-lost guard covers the common case; the tightest residual race (a
//     worker whose delayed ack let a second worker reclaim, then completes before
//     its own claim-beat 412s) is not closed by it — the wire's stop carries no
//     ownership proof — and is closed on the control-plane side instead, where
//     every re-hand-out mints a fresh work id (#62): once another worker holds
//     the item, a stop under the identity this one was handed can only 404.
//   - reclaim: leave the item live (mirrors the executor completing only when
//     faultErr is nil).
func (w *Worker) handleItem(ctx context.Context, work *sdk.BetaSelfHostedWork, parent map[string]string) {
	sessCtx, cancel := context.WithCancel(ctx)
	// The run's progress, watched by the heartbeat: the beat is the one loop
	// that keeps ticking while the run is wedged, so it is where the item's
	// silence is bounded (#383).
	prog := newProgress()
	hbDone := make(chan struct{})
	var hb hbExit
	go func() {
		defer close(hbDone)
		hb = w.heartbeat(sessCtx, cancel, work.ID, prog)
	}()

	// Parent the tool-exec span on the turn that enqueued the work (its trace
	// context rode the poll response), so a session's model turns and this
	// worker's tool runs join one OTel trace across the process boundary. Only
	// the tool run is spanned — the heartbeat is lease bookkeeping, not tool work.
	runCtx, span := otel.GetTracerProvider().Tracer(tracerName).Start(
		telemetry.Extract(sessCtx, parent), "tool_exec",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("session.id", work.Data.ID),
			attribute.String("work.id", work.ID),
		))
	outcome, fault := w.runItem(runCtx, work, prog)
	if fault != nil {
		// Mirror the platform executor (internal/executor/executor.go): only the
		// platform's own faults — the control plane unreachable for the liveness
		// check, a tool backend fault — mark the span errored, so the red span is
		// the one an operator opens to find why the tools never ran. A tool-level
		// failure the model recovers from and an ordinary cancellation leave it
		// unset; runItem already reduced those to a nil fault (see reclaimFault).
		span.SetStatus(codes.Error, fault.Error())
	}
	span.End()

	cancel()
	<-hbDone

	if hb == hbExitStopRequested {
		// The run has wound down; finish the stop the control plane asked for.
		w.forceStop(work.ID, work.Data.ID)
	} else {
		switch outcome {
		case outcomeDrain:
			w.forceStop(work.ID, work.Data.ID)
		case outcomeComplete:
			if hb.ownershipGone() {
				// runCtx, not ctx: the span has ended but its context has not, so the
				// record still lands on the tool_exec span it is about.
				slog.WarnContext(runCtx, "worker: completed work but no longer holds the lease, not stopping", "work", work.ID)
			} else {
				w.forceStop(work.ID, work.Data.ID)
			}
		case outcomeReclaim:
			// leave the item live for reclaim
		}
	}
	if w.onItemDone != nil {
		w.onItemDone(work.ID)
	}
}

// runItem does the item's work and reports what to do with it (see itemOutcome),
// alongside the platform fault to record on the tool_exec span — nil for a clean
// run, a drain, and a reclaim caused by cancellation; non-nil only for the
// platform's own faults (see reclaimFault). handleItem sets the span's status
// from it.
func (w *Worker) runItem(ctx context.Context, work *sdk.BetaSelfHostedWork, prog *progress) (itemOutcome, error) {
	sessionID := work.Data.ID
	live, coordinator, err := w.sessionLive(ctx, sessionID)
	if err != nil {
		// Could not determine liveness (a transient control-plane error, say):
		// leave the item for reclaim rather than discarding a possibly-live
		// session's work terminally.
		slog.ErrorContext(ctx, "worker: session liveness check failed, leaving item for reclaim",
			"session", sessionID, "work", work.ID, "err", err)
		return outcomeReclaim, reclaimFault(ctx, err)
	}
	if !live {
		// A session that is not running or has been archived is stale: run
		// nothing and drain the item (force-stop), so a dead session's tools
		// never fire on customer compute and the item does not reclaim-loop. This
		// is the worker's equivalent of the executor's sessionForRun drain — the
		// executor completes the item under the DB lock; the worker force-stops.
		slog.InfoContext(ctx, "worker: session not live, draining work item", "session", sessionID, "work", work.ID)
		return outcomeDrain, nil
	}
	if err := RunSessionTools(ctx, w.client, w.provider, sessionID, ToolExecConfig{
		Image:       w.cfg.Image,
		Workdir:     w.cfg.Workdir,
		Networking:  w.cfg.Networking,
		Hardening:   w.cfg.Hardening,
		Coordinator: coordinator,
		Progress:    prog.report,
	}); err != nil {
		// A tool backend-faulted (or the heartbeat cancelled the run): some tools
		// may be unanswered. Leave the item live for reclaim, matching the
		// executor's partial-fault semantics — do not force-stop it terminally.
		slog.ErrorContext(ctx, "worker: session tools did not complete, leaving item for reclaim",
			"session", sessionID, "work", work.ID, "err", err)
		return outcomeReclaim, reclaimFault(ctx, err)
	}
	return outcomeComplete, nil
}

// reclaimFault classifies why an item is being left for reclaim, so handleItem
// records only the platform's own faults on the tool_exec span. A genuine fault
// — the control plane unreachable for the liveness check, a tool backend fault —
// is returned to be recorded, reddening the span an operator opens to find why
// the tools never ran. An ordinary cancellation is not a fault and reduces to
// nil: the heartbeat gives the lease up (a second worker reclaims) or the worker
// is shutting down, and the in-flight run unwinds with its context cancelled;
// erroring the span for that would redden a trace view on routine teardown.
//
// It classifies on the ambient context state (ctx.Err()), not the error's own
// kind, so a run cancelled through a provider that does not preserve
// context.Canceled in its error chain is still recognised as teardown. The cost
// is a narrow, self-healing bias toward under-reporting: a genuine fault that
// races an unrelated cancellation reduces to nil, but that path is outcomeReclaim,
// so the item is reclaimed and re-run and a persistent fault reddens next pass.
// The reverse — a cancellation reddening the span — cannot happen: a
// cancellation-caused error implies ctx.Err() != nil.
//
// This mirrors the executor's rule that only platform faults error the span,
// with one deliberate divergence: the executor reddens its span when its lease
// keeper loses the lease (internal/executor/executor.go), whereas for the worker
// a lost lease *is* the heartbeat cancelling the run — its designed teardown
// path — so reddening it would light up a trace on routine lease handoff.
func reclaimFault(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// progress records when the run last reported that it moved, so the heartbeat
// can tell a long item from a wedged one. Only the run can make that call — a
// tool answered, a mount landed — so progress is reported, never inferred here;
// a timer would report progress a wedged run is not making. It is the worker's
// twin of queue.LeaseKeeper's own tracker, separate because the two lanes renew
// leases through different mechanisms (a database row there, the wire here).
type progress struct {
	// start is the monotonic base last is measured from; storing an instant
	// instead would compare a wall clock against the loop's monotonic one.
	start time.Time
	last  atomic.Int64 // nanoseconds since start of the last report
}

func newProgress() *progress { return &progress{start: time.Now()} }

// report marks the run as having moved. Safe from any goroutine.
func (p *progress) report() { p.last.Store(int64(time.Since(p.start))) }

// stalledFor reports whether the run has said nothing for the whole budget. A
// budget of zero or less never stalls.
//
// Written as elapsed-minus-last rather than last-plus-budget: the left side
// cannot underflow (last is an elapsed time this tracker stored, so never ahead
// of now), while the right side would wrap negative — and stall instantly — for
// a budget near the duration ceiling, which time.ParseDuration accepts.
func (p *progress) stalledFor(budget time.Duration) bool {
	return budget > 0 && time.Since(p.start)-time.Duration(p.last.Load()) > budget
}

// hbExit reports why the heartbeat loop ended, so handleItem can tell a stop the
// control plane deliberately asked for from a lease this worker merely lost.
// Both cancel the run identically, but they are opposite instructions about the
// item afterwards: the first must be finished, the second must be left alone.
type hbExit int

const (
	// hbExitCancelled: the loop's context ended it — handleItem cancelled it once
	// the run finished, or the worker is shutting down. The lease was never lost.
	hbExitCancelled hbExit = iota
	// hbExitStopRequested: the control plane moved the item to stopping/stopped.
	// The item is still exclusively this worker's — Poll never re-offers a
	// stopping item — so finishing the stop is this worker's job (see handleItem).
	// The already-stopped half needs no finishing, and needs no branch of its own
	// either: the stop it provokes is the 409 forceStop already ignores.
	hbExitStopRequested
	// hbExitLeaseLost: ownership is gone or unprovable — a 412 (another worker
	// reclaimed it), any other fatal 4xx, a lease the control plane declined to
	// extend, or the staleness ceiling. The item may be another worker's now, so
	// this worker must not stop it.
	hbExitLeaseLost
	// hbExitStalled: the run reported no progress for Config.StallTimeout, so
	// the heartbeat cancelled it and stopped beating (#383). The lease is then
	// given up rather than taken: it lapses server-side and the control plane
	// re-offers the item, which is the recovery a wedged run would otherwise
	// never reach, because nothing crashed.
	hbExitStalled
)

// ownershipGone reports the exits after which this worker must not touch the
// item again. A lost lease was taken from it; a stalled one it gave up — either
// way another worker may hold the item by now, and stopping it could terminate
// that worker's run.
func (e hbExit) ownershipGone() bool { return e == hbExitLeaseLost || e == hbExitStalled }

// heartbeat keeps the item's lease alive on the wire's optimistic-concurrency
// protocol: the first beat sends NO_HEARTBEAT to claim the lease (starting →
// active), each later beat echoes the server's prior last_heartbeat to extend
// it. It cancels the run (via cancel) and returns when the item stops being this
// worker's to run — the control plane moved it to stopping/stopped, declined to
// extend the lease, or rejected the precondition (412, another worker reclaimed
// it) — or on any other fatal 4xx. Which of those it was is the return value
// (see hbExit): only the control plane's own stop leaves the item this worker's
// to finish. A transient error is retried, but only until the lease's TTL has
// elapsed with no successful beat: past that staleness ceiling the lease has
// lapsed server-side and may be reclaimed, so the run is cancelled rather than
// left executing against a lease this worker no longer holds. While retrying
// transiently, the wait shrinks so the ceiling is checked right at the deadline,
// not up to a full interval late. The first beat fires immediately.
//
// It is also where the run's own liveness is bounded: before each beat it asks
// prog whether the run has reported anything within Config.StallTimeout, and a
// run that has not is cancelled and beaten for no longer (hbExitStalled, #383).
// The check sits before the beat, never after, so a wedged item's lease is never
// bought another interval it cannot use. Detection costs up to one interval on
// top of the budget, since both live on this loop.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, workID string, prog *progress) hbExit {
	last := noHeartbeat
	interval := heartbeatCap
	if w.cfg.HeartbeatInterval > 0 {
		interval = w.cfg.HeartbeatInterval
	}
	// ttl is the lease window (the staleness ceiling); lastSuccess anchors it.
	// Both start at the default before the first response refines them.
	ttl := heartbeatCap
	lastSuccess := time.Now()
	for {
		if prog.stalledFor(w.cfg.StallTimeout) {
			// The run has said nothing for its whole budget: cancel it and beat no
			// more, so the lease lapses and the control plane re-offers the item.
			slog.Warn("worker: run reported no progress within its stall budget, leaving its lease to lapse",
				"work", workID, "budget", w.cfg.StallTimeout)
			cancel()
			return hbExitStalled
		}
		// Bound each call so a hung heartbeat cannot silently let the lease lapse,
		// but never below a second: the derived interval is already clamped to
		// [1s, 30s], so this floor only guards a very short configured interval
		// (tests) from timing out a real HTTP+DB round trip.
		callTimeout := interval
		if callTimeout < time.Second {
			callTimeout = time.Second
		}
		resp, err := w.client.Beta.Environments.Work.Heartbeat(ctx, workID, sdk.BetaEnvironmentWorkHeartbeatParams{
			EnvironmentID:         w.cfg.EnvironmentID,
			ExpectedLastHeartbeat: sdk.String(last),
		}, option.WithRequestTimeout(callTimeout))
		wait := interval
		if err != nil {
			if ctx.Err() != nil {
				return hbExitCancelled
			}
			if isFatalHeartbeat(err) {
				slog.Warn("worker: heartbeat lost the lease", "work", workID, "err", err)
				cancel()
				return hbExitLeaseLost
			}
			if leaseLapsed(time.Since(lastSuccess), ttl) {
				// The lease TTL elapsed with no successful beat: it has lapsed
				// server-side and may be reclaimed, so stop running against it.
				slog.Warn("worker: heartbeat stale beyond lease TTL, releasing", "work", workID, "err", err)
				cancel()
				return hbExitLeaseLost
			}
			slog.Warn("worker: transient heartbeat error, retrying", "work", workID, "err", err)
			// Shrink the wait so the next iteration re-checks the ceiling at the
			// deadline rather than up to a full interval past it.
			if untilDeadline := ttl - time.Since(lastSuccess); untilDeadline < wait {
				wait = untilDeadline
			}
		} else {
			if resp.State == "stopping" || resp.State == "stopped" {
				slog.Info("worker: control plane stopped the item, winding down", "work", workID, "state", string(resp.State))
				cancel()
				return hbExitStopRequested
			}
			if !resp.LeaseExtended {
				slog.Warn("worker: lease not extended, winding down", "work", workID)
				cancel()
				return hbExitLeaseLost
			}
			last = resp.LastHeartbeat
			lastSuccess = time.Now()
			if resp.TTLSeconds > 0 {
				ttl = time.Duration(resp.TTLSeconds) * time.Second
				if ttl < heartbeatFloor {
					ttl = heartbeatFloor
				}
				if w.cfg.HeartbeatInterval <= 0 {
					interval = clampDur(ttl/2, heartbeatFloor, heartbeatCap)
				}
				wait = interval
			}
		}
		if sleep(ctx, wait) != nil {
			return hbExitCancelled
		}
	}
}

// leaseLapsed reports whether the lease has gone stale: the time since the last
// successful heartbeat has exceeded the lease TTL, so the control plane may have
// reclaimed it and the worker must stop running against it.
func leaseLapsed(sinceLastSuccess, ttl time.Duration) bool {
	return sinceLastSuccess > ttl
}

// sessionLive reports whether the session is still a valid target for tool
// execution — running and not archived — and whether it is a coordinator's,
// which is the mode the tool-exec driver switches on (plan 35 decision 13 iv).
// It reads the session over the wire (the worker has no database) and uses the
// SDK's typed fields: a non-archived session serializes archived_at as null,
// which unmarshals to the zero time, so ArchivedAt.IsZero() is exactly the
// null-archived case, and a single-agent session serializes multiagent as null,
// which unmarshals to the zero struct and so an empty roster. Both answers come
// from the one read the liveness gate already makes; nothing else in the worker
// needs the snapshot.
func (w *Worker) sessionLive(ctx context.Context, sessionID string) (live, coordinator bool, err error) {
	sess, err := w.client.Beta.Sessions.Get(ctx, sessionID, sdk.BetaSessionGetParams{})
	if err != nil {
		return false, false, err
	}
	live = sess.Status == sdk.BetaManagedAgentsSessionStatusRunning && sess.ArchivedAt.IsZero()
	return live, len(sess.Agent.Multiagent.Agents) > 0, nil
}

// forceStop stops the work item, ignoring a 409 (already stopping/stopped, which
// the reference also ignores). It runs on a fresh background context so the item
// is still stopped even when the worker is shutting down and ctx is cancelled.
// A 404 is logged rather than ignored: it means this worker hung long enough for
// the control plane to re-offer its item under a fresh identity (#62), so the
// stop reached nothing — which is the point, but is worth an operator's eye. It
// is the one log line that fires in exactly that scenario, and the id it names
// is by then unresolvable, so it carries the session too: that is the key an
// operator can still follow, and the one the item's spans are joined on.
//
// Stop answers a bodiless 204, but the generated method is typed
// *BetaSelfHostedWork, so the SDK's strict decoder fails a successful call with
// "expected destination type of 'string' or '[]byte' …". Rebinding the response
// destination to **http.Response trips the decoder bypass — the same workaround
// the reference's own poller applies, for the same reason (anthropic-sdk-go
// lib/environments/poller.go, stopWork).
func (w *Worker) forceStop(workID, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	var raw *http.Response
	if _, err := w.client.Beta.Environments.Work.Stop(ctx, workID, sdk.BetaEnvironmentWorkStopParams{
		EnvironmentID:                 w.cfg.EnvironmentID,
		BetaSelfHostedWorkStopRequest: sdk.BetaSelfHostedWorkStopRequestParam{Force: sdk.Bool(true)},
	}, option.WithResponseBodyInto(&raw)); err != nil && !isStatus(err, 409) {
		slog.Warn("worker: force-stop failed", "work", workID, "session", sessionID, "err", err)
	}
	if raw != nil && raw.Body != nil {
		_ = raw.Body.Close()
	}
}

// isAuthError reports a bad-credential poll error (401/403) — fatal, since
// retrying with the same environment key will never succeed.
func isAuthError(err error) bool {
	return isStatus(err, 401) || isStatus(err, 403)
}

// isFatalHeartbeat reports a heartbeat error that means the lease is gone: a 412
// precondition failure (another worker reclaimed it) or any other client error
// except the transient 408/429. A 5xx or network error is transient.
func isFatalHeartbeat(err error) bool {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.StatusCode
	return code >= 400 && code < 500 && code != 408 && code != 429
}

func isStatus(err error, code int) bool {
	var apiErr *sdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func clampDur(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

const (
	// backoffBase/Cap bound the poll-error backoff: 1s doubling to a 60s cap,
	// matching the reference `ant beta:worker` retry schedule.
	backoffBase = 1 * time.Second
	backoffCap  = 60 * time.Second
)

// backoff returns a jittered exponential backoff for the given consecutive
// poll-failure count (1-based): base 1s doubling to a 60s cap, then jittered
// down (see jitter) so a fleet recovering from a shared control-plane outage
// does not re-poll in lockstep.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase
	for i := 1; i < attempt && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return jitter(d)
}

// jitter returns a random duration in [d/2, d]: half of d fixed, half random.
// Applied to poll backoff and empty-poll spacing, it desynchronizes a fleet of
// workers' timers (avoiding a thundering herd) while never exceeding d.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(mrand.Int64N(int64(d)/2+1))
}

// defaultWorkerID mints a stable-per-process worker id, "<hostname>-<random>",
// as the reference does, so the control plane's poll metrics can distinguish
// workers.
func defaultWorkerID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	suffix := hex.EncodeToString(b[:])
	if host, err := os.Hostname(); err == nil && host != "" {
		return host + "-" + suffix
	}
	return "managed-agent-worker-" + suffix
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
