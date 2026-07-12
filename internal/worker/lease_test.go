package worker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// enqueueWork enqueues a tool_exec work item for the harness session — the item
// a worker polls, acks, and runs. Mirrors the brain suspending a turn on a
// built-in tool (which enqueues one tool_exec item).
func (h *harness) enqueueWork(t *testing.T) {
	t.Helper()
	if _, err := queue.New(h.pool).Enqueue(context.Background(), h.pool, h.envID, h.sid, queue.ToolExec); err != nil {
		t.Fatalf("enqueue tool_exec: %v", err)
	}
}

// waitForState polls the work item's state until it reaches want, up to ~3s.
func waitForState(t *testing.T, h *harness, want string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if h.workState(t) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("work item never reached state %q (last %q)", want, h.workState(t))
}

// workState returns the tool_exec work item's current state.
func (h *harness) workState(t *testing.T) string {
	t.Helper()
	var state string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`,
		h.sid.String()).Scan(&state); err != nil {
		t.Fatalf("read work state: %v", err)
	}
	return state
}

// newWorker builds a worker over the harness client + provider, tuned for fast
// tests (short empty-poll sleep and heartbeat interval), and wires onItemDone to
// signal each fully-handled item on a channel the test waits on.
func (h *harness) newWorker(cfg Config) (*Worker, <-chan string) {
	cfg.EnvironmentID = h.envID.String()
	cfg.WorkerID = "worker-test"
	if cfg.EmptyPollSleep == 0 {
		cfg.EmptyPollSleep = 5 * time.Millisecond
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 20 * time.Millisecond
	}
	done := make(chan string, 8)
	w := NewWorker(h.client, h.prov, cfg)
	w.onItemDone = func(id string) { done <- id }
	return w, done
}

// runWorker runs w.Run in the background, returning a cancel func and a channel
// that receives Run's return value.
func runWorker(w *Worker) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx) }()
	return cancel, errc
}

// waitDone blocks until the worker signals a handled item or a timeout fires.
func waitDone(t *testing.T, done <-chan string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not finish an item in time")
	}
}

// waitExit cancels the worker and asserts Run returns nil promptly.
func waitExit(t *testing.T, cancel context.CancelFunc, errc <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("worker Run returned %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker Run did not return after cancel")
	}
}

// TestWorkerPollsRunsAndStops is the full lease loop end to end: the worker polls
// the queued tool_exec item, acks and heartbeats it, runs the session's tool in
// its sandbox, posts a user.tool_result (which resumes the brain — a model_turn
// is enqueued), and force-stops the work item. Then it idles until cancelled.
func TestWorkerPollsRunsAndStops(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hello"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("sandbox file = %q, want the tool to have run", sb.files["/workspace/out.txt"])
	}
	if got := h.liveModelTurns(t); got != 1 {
		t.Errorf("model_turn = %d, want 1 (the completed set resumes the brain)", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerToolSpanParentsOnEnqueueTrace pins cross-process tracing end to end:
// an item enqueued under the brain turn's span is polled, and the worker's
// tool-exec span parents on that turn — same trace, so a session's model turns
// and its BYOC tool runs live in one OTel trace across the process boundary.
func TestWorkerToolSpanParentsOnEnqueueTrace(t *testing.T) {
	// Record spans through the global provider both the control plane and the
	// worker resolve, so the worker's tool-exec span is captured with its parent.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hello"))

	// Enqueue the tool_exec item under a known span, as the brain does mid-turn:
	// the control plane captures its trace context and hands it back on poll.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	turnCtx := trace.ContextWithSpanContext(context.Background(), sc)
	if _, err := queue.New(h.pool).Enqueue(turnCtx, h.pool, h.envID, h.sid, queue.ToolExec); err != nil {
		t.Fatalf("enqueue under span: %v", err)
	}

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)
	waitExit(t, cancel, errc)

	var toolSpan sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "tool_exec" {
			toolSpan = s
			break
		}
	}
	if toolSpan == nil {
		t.Fatal("no tool_exec span recorded")
	}
	if got := toolSpan.SpanContext().TraceID(); got != sc.TraceID() {
		t.Errorf("tool_exec trace id = %s, want the enqueue trace %s", got, sc.TraceID())
	}
	if got := toolSpan.Parent().SpanID(); got != sc.SpanID() {
		t.Errorf("tool_exec parent span id = %s, want the enqueue span %s", got, sc.SpanID())
	}
}

// TestWorkerLivenessGateDrainsStaleSession: a session archived before the worker
// picks up its item must not have its tools run — the worker's liveness gate
// drains the item (force-stop) without provisioning a sandbox or posting a
// result, so a dead session's tools never fire on customer compute.
func TestWorkerLivenessGateDrainsStaleSession(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET archived_at = now() WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (stale session runs nothing)", h.prov.provisions)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (nothing ran)", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (drained)", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerHeartbeatClaimsLease: before the worker's tool runs, its first
// heartbeat must claim the lease (queued → active), so a second worker cannot
// reclaim the session mid-run. A tool held open lets the test observe the item
// active while the run is in flight.
func TestWorkerHeartbeatClaimsLease(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	<-sb.entered // the tool is now running, held open by the gate
	// The first heartbeat claims the lease (queued → starting → active) shortly
	// after the run begins; the item being briefly starting is still safe (Poll
	// only reclaims queued items), so wait for the claim rather than racing it.
	waitForState(t, h, "active")
	close(sb.gate) // release the tool

	waitDone(t, done)
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerBackendFaultLeavesItemForReclaim: when a tool backend-faults mid-run,
// the worker must NOT force-stop the item. A fault can leave tools unanswered, and
// terminally stopping the item would wedge a still-live session with no way to
// resume it. The item is left in a live state (starting/active) so a later reclaim
// (or lease expiry) can re-run it — mirroring the platform executor, which
// completes its item only when the run finished without a fault. Poll returns only
// queued items, so the same worker does not re-pick and hot-loop on it.
func TestWorkerBackendFaultLeavesItemForReclaim(t *testing.T) {
	sb := &fakeSandbox{failPath: "out.txt"}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if got := h.workState(t); got == "stopped" {
		t.Errorf("work item state = %q, want it left live for reclaim (a fault must not force-stop)", got)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the tool faulted before answering)", got)
	}
	if got := h.liveModelTurns(t); got != 0 {
		t.Errorf("model_turn = %d, want 0 (a faulted set does not resume)", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerAckFailureLeavesItemQueued: a failing ack must NOT force-stop the
// item. A transient ack error leaves the item queued server-side, so Poll
// re-offers it once its reservation lapses; force-stopping it instead would move
// it to the terminal stopped state that nothing reclaims, wedging the session
// after one control-plane blip. This pins the regression the reviewers flagged.
func TestWorkerAckFailureLeavesItemQueued(t *testing.T) {
	var acks atomic.Int32
	failAck := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/ack") {
				acks.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	h := newHarnessWrapped(t, &fakeSandbox{}, failAck)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	// SDK retries disabled so the 503 surfaces to pollAck directly, not absorbed.
	noRetry := sdk.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(h.serverURL),
		option.WithAuthToken(workerKey),
		option.WithMaxRetries(0),
	)
	w := NewWorker(noRetry, h.prov, Config{EnvironmentID: h.envID.String(), EmptyPollSleep: 5 * time.Millisecond})
	cancel, errc := runWorker(w)

	// Wait until the worker has polled and attempted (and failed) the ack.
	for i := 0; i < 300 && acks.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if acks.Load() == 0 {
		t.Fatal("worker never attempted an ack")
	}
	// The item must be left queued (recoverable), never force-stopped, and no
	// sandbox provisioned — the worker never got past the ack.
	if got := h.workState(t); got != "queued" {
		t.Errorf("work item state = %q, want queued (a failed ack must not force-stop it)", got)
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (never ran)", h.prov.provisions)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerReclaimsStrandedItem is the C3 headline: a dead worker's item, left
// acked+heartbeating (active) with a lapsed lease, is reclaimed by a fresh worker
// on the next poll — it re-acks, re-claims, runs the session's still-unanswered
// tool, posts the result, and force-stops the item. This is what makes C2b's
// leave-live-for-reclaim actually recover.
func TestWorkerReclaimsStrandedItem(t *testing.T) {
	ctx := context.Background()
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	// Simulate a worker that acked and claimed the item, then died: drive the
	// queue to active, then expire its lease.
	q := queue.New(h.pool)
	dead, err := q.Poll(ctx, h.envID, time.Minute)
	if err != nil || dead == nil {
		t.Fatalf("seed poll: %+v %v", dead, err)
	}
	if _, err := q.Ack(ctx, h.envID, dead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, h.envID, dead.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, dead.ID.String()); err != nil {
		t.Fatal(err)
	}

	// A fresh worker reclaims the stranded item and finishes it.
	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (reclaimed and finished)", got)
	}
	if sb.files["/workspace/out.txt"] != "hi" {
		t.Errorf("sandbox file = %q, want the reclaimed tool to have run", sb.files["/workspace/out.txt"])
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (reclaimed run completed)", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerEmptyQueueIdlesUntilCancel: with no work, the worker polls, finds
// nothing, and idles — returning nil promptly once cancelled, having provisioned
// nothing.
func TestWorkerEmptyQueueIdlesUntilCancel(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	w, _ := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	time.Sleep(50 * time.Millisecond) // let it spin the empty-poll loop a few times
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (empty queue)", h.prov.provisions)
	}
	waitExit(t, cancel, errc)
}

// workID returns the tool_exec work item's id.
func (h *harness) workID(t *testing.T) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`,
		h.sid.String()).Scan(&id); err != nil {
		t.Fatalf("read work id: %v", err)
	}
	return id
}

// TestWorkerControlPlaneStopWindsDown: while the worker runs a session's tool,
// the control plane moving the work item to stopping (a graceful stop) must make
// the worker's heartbeat wind the run down — cancelling the in-flight tool rather
// than finishing it. No result is posted for the cancelled tool.
func TestWorkerControlPlaneStopWindsDown(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	<-sb.entered // the tool is held open, mid-run
	// The control plane asks the item to stop. The next heartbeat sees the
	// stopping state and cancels the run; the held tool unblocks via ctx and
	// never completes, so no result is posted.
	if _, err := queue.New(h.pool).Stop(context.Background(), h.envID, domain.ID(h.workID(t)), false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}

	waitDone(t, done)
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was wound down)", got)
	}
	close(sb.gate) // release, though the tool already returned via cancellation
	waitExit(t, cancel, errc)
}

// TestWorkerBadKeyPollIsFatal: a worker whose environment key the control plane
// rejects fails fast — Run returns the auth error rather than spinning forever.
func TestWorkerBadKeyPollIsFatal(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	w := NewWorker(NewClient(h.serverURL, "ek-not-a-real-key"), h.prov,
		Config{EnvironmentID: h.envID.String(), EmptyPollSleep: 5 * time.Millisecond}.withDefaults())

	errc := make(chan error, 1)
	go func() { errc <- w.Run(context.Background()) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Run returned nil for a rejected key, want the auth error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on a fatal auth error (it should not spin)")
	}
}

// TestErrorClassification pins the wire-status policy the loop branches on.
func TestErrorClassification(t *testing.T) {
	auth := &sdk.Error{StatusCode: 401}
	perm := &sdk.Error{StatusCode: 403}
	precond := &sdk.Error{StatusCode: 412}
	conflict := &sdk.Error{StatusCode: 409}
	rateLimit := &sdk.Error{StatusCode: 429}
	server := &sdk.Error{StatusCode: 503}

	if !isAuthError(auth) || !isAuthError(perm) {
		t.Error("401/403 should be auth errors (fatal poll)")
	}
	if isAuthError(conflict) || isAuthError(errors.New("network")) {
		t.Error("409 and non-API errors are not auth errors")
	}
	if !isFatalHeartbeat(precond) || !isFatalHeartbeat(conflict) {
		t.Error("412/409 should be fatal heartbeat errors (lease lost)")
	}
	if isFatalHeartbeat(rateLimit) || isFatalHeartbeat(server) || isFatalHeartbeat(errors.New("timeout")) {
		t.Error("429/5xx/network errors are transient, not fatal heartbeat errors")
	}
	if !isStatus(conflict, 409) || isStatus(conflict, 412) {
		t.Error("isStatus must match the exact code")
	}
}

// TestClampDurAndWorkerID covers the small pure helpers.
func TestClampDurAndWorkerID(t *testing.T) {
	if got := clampDur(5*time.Second, time.Second, 30*time.Second); got != 5*time.Second {
		t.Errorf("clampDur in range = %v", got)
	}
	if got := clampDur(time.Millisecond, time.Second, 30*time.Second); got != time.Second {
		t.Errorf("clampDur below floor = %v, want 1s", got)
	}
	if got := clampDur(time.Hour, time.Second, 30*time.Second); got != 30*time.Second {
		t.Errorf("clampDur above cap = %v, want 30s", got)
	}
	if defaultWorkerID() == "" {
		t.Error("defaultWorkerID returned empty")
	}
}

// TestBackoffAndJitter pins the poll-error backoff schedule and the jitter
// window: backoff escalates 1s→60s and never exceeds the cap; jitter always
// falls in [d/2, d] so a fleet desynchronizes without ever waiting longer than
// the nominal interval.
func TestBackoffAndJitter(t *testing.T) {
	// jitter stays within [d/2, d] across a spread of inputs.
	for _, d := range []time.Duration{time.Second, 5 * time.Second, backoffCap} {
		for i := 0; i < 200; i++ {
			j := jitter(d)
			if j < d/2 || j > d {
				t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, j, d/2, d)
			}
		}
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}

	// backoff escalates and is bounded by the cap; attempt<1 floors to attempt 1.
	if got := backoff(0); got < backoffBase/2 || got > backoffBase {
		t.Errorf("backoff(0) = %v, want ~1s (floored to attempt 1)", got)
	}
	for _, attempt := range []int{1, 2, 3, 4, 10, 100} {
		got := backoff(attempt)
		if got > backoffCap {
			t.Errorf("backoff(%d) = %v, exceeds cap %v", attempt, got, backoffCap)
		}
		if got < backoffBase/2 {
			t.Errorf("backoff(%d) = %v, below floor %v", attempt, got, backoffBase/2)
		}
	}
	// At a high attempt the backoff sits in the capped band [cap/2, cap].
	if got := backoff(50); got < backoffCap/2 {
		t.Errorf("backoff(50) = %v, want the capped band [%v, %v]", got, backoffCap/2, backoffCap)
	}
}

// TestLeaseLapsed pins the heartbeat staleness comparison so a regression that
// inverts it (e.g. >= vs >, or swapped operands) is caught — the ceiling is what
// stops the worker running against a lease it may have lost.
func TestLeaseLapsed(t *testing.T) {
	ttl := 30 * time.Second
	if leaseLapsed(ttl-time.Millisecond, ttl) {
		t.Error("just under TTL must not be lapsed")
	}
	if leaseLapsed(ttl, ttl) {
		t.Error("exactly at TTL is not yet lapsed (strict >)")
	}
	if !leaseLapsed(ttl+time.Millisecond, ttl) {
		t.Error("past TTL must be lapsed")
	}
}

// TestWorkerTransientHeartbeatRecovers: transient heartbeat failures (a 503 for
// the first two beats) must not abandon the run — the worker retries, recovers,
// claims the lease, and still finishes the item. This exercises the heartbeat
// transient-retry path that unit tests of the pure helpers cannot reach. The
// worker's client has SDK retries disabled so each 503 surfaces to the worker's
// own retry loop deterministically (rather than being absorbed by the SDK), and
// the tool is held open until the lease is claimed so completion is owned.
func TestWorkerTransientHeartbeatRecovers(t *testing.T) {
	var beats atomic.Int32
	failFirst := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/heartbeat") && beats.Add(1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWrapped(t, sb, failFirst)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	noRetry := sdk.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(h.serverURL),
		option.WithAuthToken(workerKey),
		option.WithMaxRetries(0),
	)
	w := NewWorker(noRetry, h.prov, Config{
		EnvironmentID:     h.envID.String(),
		EmptyPollSleep:    5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
	})
	done := make(chan string, 4)
	w.onItemDone = func(id string) { done <- id }
	cancel, errc := runWorker(w)

	<-sb.entered                 // the tool is running, held open by the gate
	waitForState(t, h, "active") // two beats 503'd, then the third claimed the lease
	close(sb.gate)               // release the tool

	waitDone(t, done)
	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (recovered and finished)", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (owned and completed)", got)
	}
	if beats.Load() < 3 {
		t.Errorf("heartbeat attempts = %d, want the two failures plus a recovery", beats.Load())
	}
	waitExit(t, cancel, errc)
}

// TestWorkerProductionHeartbeatCadence exercises the shipped configuration —
// HeartbeatInterval unset, so the cadence is derived from the server's TTL — and
// confirms an item still processes end to end under it (the other lease tests
// pin a fixed test interval).
func TestWorkerProductionHeartbeatCadence(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	// Build directly (not via newWorker) so HeartbeatInterval stays 0 — the
	// derive-from-TTL path — while keeping the empty-poll fast for the test.
	w := NewWorker(h.client, h.prov,
		Config{EnvironmentID: h.envID.String(), EmptyPollSleep: 5 * time.Millisecond}.withDefaults())
	done := make(chan string, 4)
	w.onItemDone = func(id string) { done <- id }
	cancel, errc := runWorker(w)

	waitDone(t, done)
	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1", got)
	}
	waitExit(t, cancel, errc)
}

// TestSessionLiveFetchError: a session that cannot be fetched surfaces as an
// error from the liveness check (not a false "live"), so the caller drains
// rather than provisioning for a session it cannot see.
func TestSessionLiveFetchError(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	w := NewWorker(h.client, h.prov, Config{EnvironmentID: h.envID.String()}.withDefaults())
	live, err := w.sessionLive(context.Background(), "sesn_does_not_exist_00000000")
	if err == nil {
		t.Fatal("sessionLive returned nil error for a missing session")
	}
	if live {
		t.Error("sessionLive reported a missing session as live")
	}
}

// TestWorkerDefaultsFillWorkerID: an unset worker id is auto-generated, and an
// unset empty-poll sleep gets a sane default — the loop never runs with a zero
// cadence.
func TestWorkerDefaultsFillWorkerID(t *testing.T) {
	cfg := Config{EnvironmentID: "env_x"}.withDefaults()
	if cfg.WorkerID == "" {
		t.Error("WorkerID was not auto-generated")
	}
	if cfg.EmptyPollSleep <= 0 {
		t.Errorf("EmptyPollSleep = %v, want a positive default", cfg.EmptyPollSleep)
	}
}
