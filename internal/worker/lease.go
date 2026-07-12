package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// noHeartbeat is the wire sentinel a worker's first heartbeat sends as
// expected_last_heartbeat to claim an unclaimed lease; every later heartbeat
// echoes the server's prior last_heartbeat value.
const noHeartbeat = "NO_HEARTBEAT"

// pollBlockMs is the long-poll window the worker requests. The reference sends
// 999 (the server's cap); our control plane's poll returns immediately (it does
// not yet honour block_ms), so EmptyPollSleep paces empty polls client-side.
// Sending it keeps the request wire-identical to the reference.
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
	// EmptyPollSleep is the wait between empty polls (default 1s). Our poll is
	// non-blocking, so this — not block_ms — spaces reconnections.
	EmptyPollSleep time.Duration
	// HeartbeatInterval, when > 0, fixes the heartbeat cadence; otherwise it is
	// derived from each heartbeat response's ttl (ttl/2, clamped to
	// [heartbeatFloor, heartbeatCap]) as the reference does. Tests set a small
	// value; production leaves it 0.
	HeartbeatInterval time.Duration
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
		work, err := w.pollAck(ctx)
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
			// Our poll is non-blocking, so space empty polls client-side; the
			// jitter spreads a fleet's reconnections rather than synchronizing them.
			if sleep(ctx, jitter(w.cfg.EmptyPollSleep)) != nil {
				return nil
			}
			continue
		}
		w.handleItem(ctx, work)
	}
}

// pollAck polls for the oldest queued item and acknowledges it (queued →
// starting), returning the acked item or nil when the queue is empty. Both a
// poll error and an ack error are returned to Run to back off on.
//
// An ack failure must NOT force-stop the item. A transient ack error (a
// control-plane blip, or a lost response for an ack that actually applied)
// leaves the item queued server-side; force-stopping it moves it to the
// terminal stopped state that no path reclaims (Poll reclaims only queued
// items), so a single hiccup would permanently strand the session's outstanding
// tool work. Leaving it queued lets Poll re-offer it once the lease window
// lapses, and Run's backoff keeps a genuinely un-ackable item from hot-looping.
func (w *Worker) pollAck(ctx context.Context) (*sdk.BetaSelfHostedWork, error) {
	work, err := w.client.Beta.Environments.Work.Poll(ctx, w.cfg.EnvironmentID, sdk.BetaEnvironmentWorkPollParams{
		BlockMs:           sdk.Int(pollBlockMs),
		AnthropicWorkerID: sdk.String(w.cfg.WorkerID),
	})
	if err != nil {
		return nil, err
	}
	if work == nil || work.ID == "" {
		return nil, nil
	}
	if _, err := w.client.Beta.Environments.Work.Ack(ctx, work.ID, sdk.BetaEnvironmentWorkAckParams{
		EnvironmentID: w.cfg.EnvironmentID,
	}); err != nil {
		return nil, fmt.Errorf("ack %s: %w", work.ID, err)
	}
	return work, nil
}

// itemOutcome is what runItem decided to do with a work item.
type itemOutcome int

const (
	// outcomeReclaim: an uncertain result (liveness unknown, tools faulted with
	// work unanswered, or the run was cancelled) — leave the item live so a
	// future reclaim can re-run it. NOTE: reclaim of an already-acked self_hosted
	// item is the C3 dead-worker-reclaim protocol (not yet built); until then a
	// left-live item stays stranded and the session waits. Leaving it live is
	// still correct — it is the state C3 reclaims; force-stopping would make it
	// terminal and unrecoverable.
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
//   - drain: the session is dead, so stopping its item disrupts nothing live.
//   - complete: every tool was answered — force-stop to clear the item (which
//     otherwise lingers active and blocks the session's next tool turn), UNLESS
//     the heartbeat observed losing the lease, in which case another worker may
//     now own the item and stopping it could terminate that worker's run. The
//     tightest ownership race (a worker whose delayed ack let a second worker
//     reclaim, then completes before its own claim-beat 412s) is only fully
//     closed by server-side lease generation on stop — the C3 reclaim protocol.
//   - reclaim: leave the item live (mirrors the executor completing only when
//     faultErr is nil).
func (w *Worker) handleItem(ctx context.Context, work *sdk.BetaSelfHostedWork) {
	sessCtx, cancel := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	var hb hbResult
	go func() {
		defer close(hbDone)
		hb = w.heartbeat(sessCtx, cancel, work.ID)
	}()

	outcome := w.runItem(sessCtx, work)

	cancel()
	<-hbDone

	switch outcome {
	case outcomeDrain:
		w.forceStop(work.ID)
	case outcomeComplete:
		if hb.lostLease {
			slog.Warn("worker: completed work but observed losing the lease, not stopping", "work", work.ID)
		} else {
			w.forceStop(work.ID)
		}
	case outcomeReclaim:
		// leave the item live for reclaim
	}
	if w.onItemDone != nil {
		w.onItemDone(work.ID)
	}
}

// runItem does the item's work and reports what to do with it (see itemOutcome).
func (w *Worker) runItem(ctx context.Context, work *sdk.BetaSelfHostedWork) itemOutcome {
	sessionID := work.Data.ID
	live, err := w.sessionLive(ctx, sessionID)
	if err != nil {
		// Could not determine liveness (a transient control-plane error, say):
		// leave the item for reclaim rather than discarding a possibly-live
		// session's work terminally.
		slog.Error("worker: session liveness check failed, leaving item for reclaim",
			"session", sessionID, "work", work.ID, "err", err)
		return outcomeReclaim
	}
	if !live {
		// A session that is not running or has been archived is stale: run
		// nothing and drain the item (force-stop), so a dead session's tools
		// never fire on customer compute and the item does not reclaim-loop. This
		// is the worker's equivalent of the executor's sessionForRun drain — the
		// executor completes the item under the DB lock; the worker force-stops.
		slog.Info("worker: session not live, draining work item", "session", sessionID, "work", work.ID)
		return outcomeDrain
	}
	if err := RunSessionTools(ctx, w.client, w.provider, sessionID, ToolExecConfig{
		Image:      w.cfg.Image,
		Workdir:    w.cfg.Workdir,
		Networking: w.cfg.Networking,
	}); err != nil {
		// A tool backend-faulted (or the heartbeat cancelled the run): some tools
		// may be unanswered. Leave the item live for reclaim, matching the
		// executor's partial-fault semantics — do not force-stop it terminally.
		slog.Error("worker: session tools did not complete, leaving item for reclaim",
			"session", sessionID, "work", work.ID, "err", err)
		return outcomeReclaim
	}
	return outcomeComplete
}

// hbResult reports how the heartbeat loop ended, so handleItem can decide
// whether it still owns the item. lostLease is true if the loop gave the lease
// up (the control plane stopped it, declined to extend, a fatal precondition, or
// the staleness ceiling) rather than exiting because handleItem cancelled it
// after the run finished.
type hbResult struct {
	lostLease bool
}

// heartbeat keeps the item's lease alive on the wire's optimistic-concurrency
// protocol: the first beat sends NO_HEARTBEAT to claim the lease (starting →
// active), each later beat echoes the server's prior last_heartbeat to extend
// it. It cancels the run (via cancel) and returns when the lease is lost — the
// control plane moved the item to stopping/stopped, declined to extend, or
// rejected the precondition (412, another worker reclaimed it) — or on any other
// fatal 4xx. A transient error is retried, but only until the lease's TTL has
// elapsed with no successful beat: past that staleness ceiling the lease has
// lapsed server-side and may be reclaimed, so the run is cancelled rather than
// left executing against a lease this worker no longer holds. While retrying
// transiently, the wait shrinks so the ceiling is checked right at the deadline,
// not up to a full interval late. The first beat fires immediately.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, workID string) hbResult {
	last := noHeartbeat
	interval := heartbeatCap
	if w.cfg.HeartbeatInterval > 0 {
		interval = w.cfg.HeartbeatInterval
	}
	// ttl is the lease window (the staleness ceiling); lastSuccess anchors it.
	// Both start at the default before the first response refines them.
	ttl := heartbeatCap
	lastSuccess := time.Now()
	var res hbResult
	for {
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
				return res
			}
			if isFatalHeartbeat(err) {
				slog.Warn("worker: heartbeat lost the lease", "work", workID, "err", err)
				res.lostLease = true
				cancel()
				return res
			}
			if leaseLapsed(time.Since(lastSuccess), ttl) {
				// The lease TTL elapsed with no successful beat: it has lapsed
				// server-side and may be reclaimed, so stop running against it.
				slog.Warn("worker: heartbeat stale beyond lease TTL, releasing", "work", workID, "err", err)
				res.lostLease = true
				cancel()
				return res
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
				res.lostLease = true
				cancel()
				return res
			}
			if !resp.LeaseExtended {
				slog.Warn("worker: lease not extended, winding down", "work", workID)
				res.lostLease = true
				cancel()
				return res
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
			return res
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
// execution — running and not archived. It reads the session over the wire (the
// worker has no database) and uses the SDK's typed fields: a non-archived
// session serializes archived_at as null, which unmarshals to the zero time, so
// ArchivedAt.IsZero() is exactly the null-archived case.
func (w *Worker) sessionLive(ctx context.Context, sessionID string) (bool, error) {
	sess, err := w.client.Beta.Sessions.Get(ctx, sessionID, sdk.BetaSessionGetParams{})
	if err != nil {
		return false, err
	}
	return sess.Status == sdk.BetaManagedAgentsSessionStatusRunning && sess.ArchivedAt.IsZero(), nil
}

// forceStop stops the work item, ignoring a 409 (already stopping/stopped, which
// the reference also ignores). It runs on a fresh background context so the item
// is still stopped even when the worker is shutting down and ctx is cancelled.
func (w *Worker) forceStop(workID string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if _, err := w.client.Beta.Environments.Work.Stop(ctx, workID, sdk.BetaEnvironmentWorkStopParams{
		EnvironmentID:                 w.cfg.EnvironmentID,
		BetaSelfHostedWorkStopRequest: sdk.BetaSelfHostedWorkStopRequestParam{Force: sdk.Bool(true)},
	}); err != nil && !isStatus(err, 409) {
		slog.Warn("worker: force-stop failed", "work", workID, "err", err)
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
