package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
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

// firstItem is the clause the item readers below share: the session's first
// tool_exec item. A stop that lands re-arms the session's unanswered calls as a
// newer row (the work API's stopWork), and a re-hand-out rotates the row's id
// but keeps its created_at, so this reads the item a test staged whatever
// followed it.
const firstItem = `FROM work_items WHERE session_id = $1 AND kind = 'tool_exec' ORDER BY created_at, id LIMIT 1`

// workState returns the tool_exec work item's current state.
func (h *harness) workState(t *testing.T) string {
	t.Helper()
	var state string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state `+firstItem,
		h.sid.String()).Scan(&state); err != nil {
		t.Fatalf("read work state: %v", err)
	}
	return state
}

// lastHeartbeat returns the tool_exec work item's current last_heartbeat, which
// must already be set (the caller waits for the item to reach active first).
func (h *harness) lastHeartbeat(t *testing.T) time.Time {
	t.Helper()
	var ts *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT last_heartbeat `+firstItem,
		h.sid.String()).Scan(&ts); err != nil {
		t.Fatalf("read last_heartbeat: %v", err)
	}
	if ts == nil {
		t.Fatal("last_heartbeat is still null")
	}
	return *ts
}

// waitHeartbeatAfter blocks until the item's last_heartbeat advances past mark —
// the worker's next beat landing on an item that is still active and still its
// own. An item the control plane has moved to stopping/stopped never advances it
// (the echo branch re-stamps only while active), so this is a positive proof of
// liveness, not a sleep.
func (h *harness) waitHeartbeatAfter(t *testing.T, mark time.Time) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if h.lastHeartbeat(t).After(mark) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the item's heartbeat never advanced past the stale stop")
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

// noRetryClient is the harness's worker client with the SDK's own retries off,
// so each answer a test stages reaches the worker's loop as it is rather than
// being absorbed by a retry. opts ride on top (a test's middleware, say).
func (h *harness) noRetryClient(opts ...option.RequestOption) sdk.Client {
	return sdk.NewClient(append([]option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(h.serverURL),
		option.WithAuthToken(h.key),
		option.WithMaxRetries(0),
	}, opts...)...)
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

// TestWorkerForceStopAcceptsTheWorkObject pins the worker half of the wire's
// Stop against the current control plane, which answers 200 with the work
// object, as the recorded reference service does (#804). The worker binds the
// response to **http.Response rather than decoding it, since the body is of no
// use to it, so what it owes the body is a read to EOF before the close: Go's
// transport returns a connection to the pool only once its body has been read
// to the end, and one closed unread costs the worker's next request a new
// connection. A probe around each 200 stop body the worker receives records
// whether it was, and that the probe saw one proves the test's premise. A
// mishandled 200 could also cry wolf on a clean finish, so the absence of a
// false warning is asserted too. Work metadata has no size cap of its own, so
// the object can run past any bound a drain might set; the large case is one
// well past 64 KiB, and it is read to the end all the same.
func TestWorkerForceStopAcceptsTheWorkObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		pad  int // bytes of metadata on the item
	}{{"a small object", 0}, {"an object past 64 KiB", 100_000}} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			h.suspend(t, writeUse("out.txt", "hello"))
			h.enqueueWork(t)
			if tc.pad > 0 {
				if _, err := h.pool.Exec(context.Background(),
					`UPDATE work_items SET metadata = jsonb_build_object('pad', repeat('x', $2))
					  WHERE session_id = $1 AND kind = 'tool_exec'`, h.sid.String(), tc.pad); err != nil {
					t.Fatalf("pad metadata: %v", err)
				}
			}
			var mu sync.Mutex
			var bodies []*drainProbe
			h.client = sdk.NewClient(
				option.WithoutEnvironmentDefaults(),
				option.WithBaseURL(h.serverURL),
				option.WithAuthToken(h.key),
				option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
					res, err := next(req)
					if err == nil && strings.HasSuffix(req.URL.Path, "/stop") && res.StatusCode == http.StatusOK &&
						strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
						p := &drainProbe{ReadCloser: res.Body}
						res.Body = p
						mu.Lock()
						bodies = append(bodies, p)
						mu.Unlock()
					}
					return res, err
				}),
			)

			warnings := captureWarnings(t)

			w, done := h.newWorker(Config{})
			cancel, errc := runWorker(w)
			waitDone(t, done)
			waitExit(t, cancel, errc)

			if got := h.workState(t); got != "stopped" {
				t.Fatalf("work item state = %q, want stopped", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) == 0 {
				t.Fatal("no stop was answered 200 with a JSON body; the test proves nothing")
			}
			for i, p := range bodies {
				if !p.closed.Load() || !p.drained.Load() {
					t.Errorf("stop body %d: closed %v, read to EOF before the close %v; want both, or the connection is dropped",
						i, p.closed.Load(), p.drained.Load())
				}
			}
			if out := warnings(); strings.Contains(out, "force-stop failed") {
				t.Errorf("a successful 200 stop logged a failure:\n%s", out)
			}
		})
	}
}

// drainProbe wraps a response body and records whether it was read to EOF
// before it was first closed.
type drainProbe struct {
	io.ReadCloser
	eof, drained, closed atomic.Bool
}

func (p *drainProbe) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if err == io.EOF {
		p.eof.Store(true)
	}
	return n, err
}

func (p *drainProbe) Close() error {
	if !p.closed.Swap(true) {
		p.drained.Store(p.eof.Load())
	}
	return p.ReadCloser.Close()
}

// TestWorkerForceStopAcceptsNoContent pins the worker against an older
// control plane, which answered a successful Stop with a bodiless 204 and no
// Content-Type (#27, reversed by #804). The generated SDK method is typed
// *BetaSelfHostedWork, so without the response-body bypass the strict Go
// decoder fails a call that in fact succeeded — the item still reaches stopped,
// and the only visible damage is the worker crying wolf on every clean finish.
// Asserting the state alone therefore cannot catch a missing bypass; this
// asserts the absence of the false warning. The real control plane runs the
// stop and olderServer answers it as the old one did, so the item's state is
// the real transition's.
func TestWorkerForceStopAcceptsNoContent(t *testing.T) {
	older, noContent, _ := olderServer()
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, older)
	h.suspend(t, writeUse("out.txt", "hello"))
	h.enqueueWork(t)

	warnings := captureWarnings(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)
	waitExit(t, cancel, errc)

	if got := h.workState(t); got != "stopped" {
		t.Fatalf("work item state = %q, want stopped", got)
	}
	if noContent.Load() == 0 {
		t.Fatal("no stop was answered 204; the test proves nothing")
	}
	if out := warnings(); strings.Contains(out, "force-stop failed") {
		t.Errorf("a successful 204 stop logged a failure; the SDK decoder bypass is missing:\n%s", out)
	}
}

// TestWorkerForceStopIgnoresAnOlderServersConflict pins the other half of an
// older control plane's Stop: it refused a stop that moved nothing with 409
// invalid_request_error, where this one and the recorded reference answer 200
// with the item unchanged (#804). The reference's poller ignores that 409
// (checked against anthropic-sdk-go v1.70.1 — poller.go
// WorkPoller.discardUnprocessable), and so does forceStop: a warning for it
// would cry wolf on every finish whose item something else had already
// stopped. That is the case staged here. The control plane force-stops the
// item while the worker runs it, the worker's next heartbeat learns of it and
// winds the run down, and the stop the worker then sends is the repeat that
// olderServer refuses. The SDK retries a 409 twice with backoff, as it would
// against a real older server, so this test takes about a second and a half.
func TestWorkerForceStopIgnoresAnOlderServersConflict(t *testing.T) {
	older, _, conflicts := olderServer()
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWrapped(t, sb, older)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	warnings := captureWarnings(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	<-sb.entered // the tool is held open, mid-run
	// The claim beat first: a force stop leaves a still-starting item stopped,
	// and a claim on stopped work is refused (an inference docs/DIVERGENCES.md
	// registers), which the worker reads as a lost lease, sending no stop at all.
	waitForState(t, h, "active")
	if _, _, err := queue.New(h.pool).Stop(context.Background(), h.envID, domain.ID(h.workID(t)), true); err != nil {
		t.Fatalf("force stop: %v", err)
	}

	waitDone(t, done)
	close(sb.gate) // release, though the tool already returned via cancellation
	waitExit(t, cancel, errc)

	if conflicts.Load() == 0 {
		t.Fatal("no stop was answered 409; the test proves nothing")
	}
	if out := warnings(); strings.Contains(out, "force-stop failed") {
		t.Errorf("an older server's 409 stop logged a failure; forceStop no longer ignores it:\n%s", out)
	}
}

// olderServer wraps the control plane in the Stop answers an older one gave: a
// bodiless 204 with no Content-Type for a stop that moved the item (#27), and
// 409 invalid_request_error for one that moved nothing. The real stop runs
// first, so the item's state is the real transition's. Whether it moved the
// item is read off the item itself: a GET just before the stop renders exactly
// what a stop that moves nothing answers. Every other request, and a stop the
// control plane refused, passes through unchanged. The counts let a test prove
// its own premise.
func olderServer() (wrap func(http.Handler) http.Handler, noContent, conflicts *atomic.Int32) {
	noContent, conflicts = new(atomic.Int32), new(atomic.Int32)
	wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/stop") {
				next.ServeHTTP(w, r)
				return
			}
			get := httptest.NewRequest(http.MethodGet, strings.TrimSuffix(r.URL.Path, "/stop"), nil)
			get.Header = r.Header.Clone()
			before := httptest.NewRecorder()
			next.ServeHTTP(before, get)
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			switch {
			case rec.Code != http.StatusOK:
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
			case bytes.Equal(rec.Body.Bytes(), before.Body.Bytes()):
				conflicts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"work item is already stopping or stopped"}}`)
			default:
				noContent.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}
		})
	}
	return wrap, noContent, conflicts
}

// TestSDKTypedStopDecodesTheWorkObject drives the generated Stop with no
// bypass at all, as a caller of the typed SDK method would: the 200 decodes
// into the BetaSelfHostedWork it is typed as, and a repeat stop of the stopped
// item is the same object again rather than an error (#804).
func TestSDKTypedStopDecodesTheWorkObject(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.enqueueWork(t)
	ctx := context.Background()
	id := h.workID(t)
	params := sdk.BetaEnvironmentWorkStopParams{
		EnvironmentID:                 h.envID.String(),
		BetaSelfHostedWorkStopRequest: sdk.BetaSelfHostedWorkStopRequestParam{Force: sdk.Bool(true)},
	}

	first, err := h.client.Beta.Environments.Work.Stop(ctx, id, params)
	if err != nil {
		t.Fatalf("typed stop: %v", err)
	}
	if first.ID != id || first.State != sdk.BetaSelfHostedWorkStateStopped || first.StoppedAt == "" ||
		first.Data.ID != h.sid.String() {
		t.Errorf("typed stop = %+v, want work %s stopped with stopped_at, for session %s", first, id, h.sid)
	}
	again, err := h.client.Beta.Environments.Work.Stop(ctx, id, sdk.BetaEnvironmentWorkStopParams{EnvironmentID: h.envID.String()})
	if err != nil {
		t.Fatalf("typed repeat stop: %v", err)
	}
	if again.State != first.State || again.StoppedAt != first.StoppedAt || again.StopRequestedAt != first.StopRequestedAt {
		t.Errorf("typed repeat stop = %+v, want the unchanged %+v", again, first)
	}
}

// TestSDKTypedHeartbeatDecodesAStoppingClaim drives the generated Heartbeat
// with no bypass, as the reference worker's heartbeat loop does, for a claim
// on work a graceful stop reached first. This control plane's answer decodes
// into BetaSelfHostedWorkHeartbeatResponse as the recorded one does (2026-09-19
// custom-mixed-tools #44, served verbatim here by a stub): state stopping, the
// lease not extended, and last_heartbeat present as the empty string. The SDK
// types that field a string, so a null would decode to the same "" — only the
// raw field tells them apart. ttl_seconds is the one field that differs: the
// recording's 120 against the TTL this platform echoes, a difference
// docs/DIVERGENCES.md registers.
func TestSDKTypedHeartbeatDecodesAStoppingClaim(t *testing.T) {
	const recorded = `{"type":"work_heartbeat","lease_extended":false,"state":"stopping","last_heartbeat":"","ttl_seconds":120}`
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, recorded)
	}))
	t.Cleanup(stub.Close)

	h := newHarness(t, &fakeSandbox{})
	h.enqueueWork(t)
	ctx := context.Background()
	q := queue.New(h.pool)
	w, err := q.Poll(ctx, h.envID, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}
	if _, err := q.Ack(ctx, h.envID, w.ID); err != nil {
		t.Fatal(err)
	}
	id := w.ID.String()
	stopped, err := h.client.Beta.Environments.Work.Stop(ctx, id, sdk.BetaEnvironmentWorkStopParams{EnvironmentID: h.envID.String()})
	if err != nil || stopped.State != sdk.BetaSelfHostedWorkStateStopping {
		t.Fatalf("typed graceful stop of starting work = %+v %v, want stopping", stopped, err)
	}

	params := sdk.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID:         h.envID.String(),
		ExpectedLastHeartbeat: sdk.String(noHeartbeat),
	}
	ours, err := h.client.Beta.Environments.Work.Heartbeat(ctx, id, params)
	if err != nil {
		t.Fatalf("typed claim on never-claimed stopping work: %v", err)
	}
	recording := NewClient(stub.URL, "ek-stub")
	theirs, err := recording.Beta.Environments.Work.Heartbeat(ctx, id, params)
	if err != nil {
		t.Fatalf("typed decode of the recorded answer: %v", err)
	}
	for _, got := range []struct {
		name string
		resp *sdk.BetaSelfHostedWorkHeartbeatResponse
		ttl  int64
	}{{"this control plane's", ours, 30}, {"the recorded", theirs, 120}} {
		r := got.resp
		if r.State != sdk.BetaSelfHostedWorkHeartbeatResponseStateStopping || r.LeaseExtended ||
			r.LastHeartbeat != "" || r.JSON.LastHeartbeat.Raw() != `""` || r.TTLSeconds != got.ttl {
			t.Errorf("%s claim decodes to %+v (last_heartbeat raw %q), want stopping, not extended, last_heartbeat \"\", ttl %d",
				got.name, r, r.JSON.LastHeartbeat.Raw(), got.ttl)
		}
	}
}

// captureWarnings routes WARN-and-above logging into a buffer for one test and
// returns a locked accessor for it.
//
// The stdlib-log save/restore is not optional, for the reason internal/executor's
// captureLogs documents: slog.SetDefault reroutes the standard log package into
// whatever handler it installs, and restoring only slog.Default() does not undo
// that — on the way back the previous handler IS a *defaultHandler, so
// SetDefault's type check skips the log.SetOutput call and log keeps pointing at
// this finished test's handler. Every later log.Print in this package's test
// binary would vanish into it, taking the httptest control plane's diagnostics
// (superfluous WriteHeader, handler panics) with them.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

// lockedBuffer guards the assertion's read against writes from any logger still
// live when it runs — the in-process control plane keeps logging on its own
// goroutines. slog's handler serializes its own writes, but not against us.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
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
	if got := toolSpan.Status().Code; got != codes.Unset {
		t.Errorf("clean run's span status = %v, want unset", got)
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
	w := NewWorker(h.noRetryClient(), h.prov, Config{EnvironmentID: h.envID.String(), EmptyPollSleep: 5 * time.Millisecond})
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

// TestWorkerAck404DropsTheItemWithoutFaulting: a 404 ack is the hand-off the
// fresh-identity-per-re-hand-out rotation creates (#62) — this worker's ack was
// so late that the control plane re-offered its item under a new id, which
// another worker now owns. It must be dropped as an empty poll, not reported as
// a fault: routing it through Run's error path would log a control-plane failure
// at Error and escalate the poll backoff for routine hand-off. The worker keeps
// polling, provisions nothing, and never force-stops.
func TestWorkerAck404DropsTheItemWithoutFaulting(t *testing.T) {
	var acks atomic.Int32
	gone := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/ack") {
				acks.Add(1)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	h := newHarnessWrapped(t, &fakeSandbox{}, gone)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	warnings := captureWarnings(t)
	w, _ := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	// One hand-out is all the queue gives: the poll reserves the item for the
	// wire's default reclaim window, so the ack is attempted once and every later
	// poll finds the queue empty.
	for i := 0; i < 300 && acks.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if acks.Load() == 0 {
		t.Fatal("worker never attempted an ack")
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (the item was never this worker's)", h.prov.provisions)
	}
	if got := h.workState(t); got != "queued" {
		t.Errorf("work item state = %q, want queued (a dropped item must not be force-stopped)", got)
	}
	waitExit(t, cancel, errc)
	// The discriminator: routed through Run's error path this would be
	// `ERROR worker: poll failed, backing off`, indistinguishable from a down
	// control plane and escalating the retry schedule with it.
	if got := warnings(); strings.Contains(got, "poll failed") {
		t.Errorf("a 404 ack was reported as a poll failure:\n%s", got)
	}
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

// TestStaleWorkerStopCannotStrandTheReplacement is #62's acceptance, over the
// real wire: a hung-then-revived worker force-stops its item under the identity
// it was handed, while a replacement worker is mid-tool on the same item under
// the fresh identity the reclaim minted. Three things are asserted in the order
// the race would break them — the reclaim did not re-offer the stale id, the
// stale stop 404s, and the replacement's next heartbeat still lands on a live
// item — and only then is the tool released, so the run finishing, its result
// posted and the session resuming are consequences the test has earned rather
// than a happy path that would hold either way.
func TestStaleWorkerStopCannotStrandTheReplacement(t *testing.T) {
	ctx := context.Background()
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	// A worker polls, acks and claims the lease — then hangs, and its lease lapses.
	q := queue.New(h.pool)
	stale, err := q.Poll(ctx, h.envID, time.Minute)
	if err != nil || stale == nil {
		t.Fatalf("seed poll: %+v %v", stale, err)
	}
	if _, err := q.Ack(ctx, h.envID, stale.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, h.envID, stale.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, stale.ID.String()); err != nil {
		t.Fatal(err)
	}

	// The replacement reclaims the item and is held mid-tool, holding the lease.
	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	defer cancel() // so a failure below still unblocks the held tool via ctx
	select {
	case <-sb.entered:
	case <-time.After(15 * time.Second):
		// Bounded, so a regression anywhere before the tool (poll, ack, liveness,
		// provisioning) fails here instead of hanging to the package timeout.
		t.Fatal("the replacement never reached its tool")
	}
	if got := h.workID(t); got == stale.ID.String() {
		t.Fatalf("reclaim re-offered the stale work id %s", got)
	}
	// Let the replacement's claim beat land, so the item is active and there is a
	// real heartbeat for the wait below to advance past.
	waitForState(t, h, "active")

	// The stale worker revives and force-stops under its old identity.
	var raw *http.Response
	_, err = h.client.Beta.Environments.Work.Stop(ctx, stale.ID.String(), sdk.BetaEnvironmentWorkStopParams{
		EnvironmentID:                 h.envID.String(),
		BetaSelfHostedWorkStopRequest: sdk.BetaSelfHostedWorkStopRequestParam{Force: sdk.Bool(true)},
	}, option.WithResponseBodyInto(&raw))
	if raw != nil && raw.Body != nil {
		_ = raw.Body.Close()
	}
	if !isStatus(err, 404) {
		t.Fatalf("stale force-stop = %v, want a 404 (its identity is gone)", err)
	}

	// The strand itself, observed rather than inferred: the replacement's NEXT
	// heartbeat still lands on a live item. Under a reused id the stale stop had
	// moved that very item to stopped, where the echo branch no longer re-stamps
	// last_heartbeat (internal/queue/lifecycle.go) and the loop winds the run
	// down instead — so this wait is what the race destroys. Released only after,
	// because letting the tool finish first lets it win that race either way.
	//
	// The mark is read AFTER the stop, never before: a beat landing in the gap
	// would otherwise satisfy the wait on its own and hollow the counterfactual out.
	h.waitHeartbeatAfter(t, h.lastHeartbeat(t))

	close(sb.gate) // release the held tool: the replacement finishes its run
	waitDone(t, done)

	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (the replacement's run was not stopped)", got)
	}
	if sb.files["/workspace/out.txt"] != "hi" {
		t.Errorf("sandbox file = %q, want the replacement's tool to have run", sb.files["/workspace/out.txt"])
	}
	if got := h.liveModelTurns(t); got != 1 {
		t.Errorf("model_turn = %d, want 1 (the completed set resumes the session)", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (the replacement completed it)", got)
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
		`SELECT id `+firstItem,
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
	// Wait for the claim beat before stopping, so the stop reaches a worker that
	// holds the lease and is told by an echo beat — the channel this test is
	// about. A stop landing on a still-starting item is told by the claim
	// instead, TestWorkerStopBeforeTheClaimWindsDown's case. Tool entry costs
	// several round trips against the claim's one, so the beat wins in
	// practice — this makes it certain.
	waitForState(t, h, "active")
	// The control plane asks the item to stop. The next heartbeat sees the
	// stopping state and cancels the run; the held tool unblocks via ctx and
	// never completes, so no result is posted.
	if _, _, err := queue.New(h.pool).Stop(context.Background(), h.envID, domain.ID(h.workID(t)), false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}

	waitDone(t, done)
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was wound down)", got)
	}
	// Winding the run down is only half the contract. The graceful stop's second
	// phase — stopping → stopped — belongs to the worker that was asked to stop:
	// Poll never re-offers a stopping item, so nothing else would ever finish the
	// transition and the item would sit non-terminal with a null stopped_at (#25).
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (the worker finishes the stop it was asked for)", got)
	}
	stoppedAt, lease := h.stopFinality(t)
	if stoppedAt == nil {
		t.Error("stopped_at is null, want the finished stop stamped")
	}
	if lease != nil {
		t.Error("lease_expires_at survived the stop, want it cleared")
	}
	close(sb.gate) // release, though the tool already returned via cancellation
	waitExit(t, cancel, errc)
}

// TestWorkerStopBeforeTheClaimWindsDown runs the recorded wind-down of an item
// stopped before its worker's first beat (2026-09-19 custom-mixed-tools #22,
// #23, #44 and #47) with this worker: a graceful stop lands between the ack
// and the claim. The claim answers 200 stopping, not the 412 of a lost lease,
// so the worker takes it for the control plane's stop: it cancels the run on
// that answer, posts no tool result, and force-stops the item itself, the
// reference worker's own sequence (checked against anthropic-sdk-go v1.70.1 —
// worker.go runHeartbeat). The run starts beside the claim, as the reference
// worker's does, so a tool may be entered before the answer; the held tool
// makes a cancelled run unable to finish one. Exactly one re-arm follows, from
// that force stop. The poll hands out one item only, so the re-armed one stays
// queued to be counted rather than run.
func TestWorkerStopBeforeTheClaimWindsDown(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	var (
		h         *harness // set before the worker sends its first request
		handedOut atomic.Bool
		mu        sync.Mutex
		graceful  map[string]any // the graceful stop's answer
		liveAfter = -1           // live tool_exec items right after it
		claims    []string       // each heartbeat's status and body
		forced    []string       // each stop the worker sent: body, status, answered state
	)
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			switch path := r.URL.Path; {
			case strings.HasSuffix(path, "/work/poll") && handedOut.Load():
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "null")
				return
			case strings.HasSuffix(path, "/work/poll"):
				next.ServeHTTP(rec, r)
				handedOut.Store(strings.TrimSpace(rec.Body.String()) != "null")
			case strings.HasSuffix(path, "/ack"):
				next.ServeHTTP(rec, r)
				stop := httptest.NewRequest(http.MethodPost, strings.TrimSuffix(path, "/ack")+"/stop", strings.NewReader("{}"))
				stop.Header = r.Header.Clone()
				stopRec := httptest.NewRecorder()
				next.ServeHTTP(stopRec, stop)
				var n int
				_ = h.pool.QueryRow(r.Context(),
					`SELECT count(*) FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'
					    AND state IN ('queued', 'starting', 'active')`, h.sid.String()).Scan(&n)
				mu.Lock()
				_ = json.Unmarshal(stopRec.Body.Bytes(), &graceful)
				liveAfter = n
				mu.Unlock()
			case strings.HasSuffix(path, "/heartbeat"):
				next.ServeHTTP(rec, r)
				mu.Lock()
				claims = append(claims, fmt.Sprintf("%d %s", rec.Code, rec.Body.String()))
				mu.Unlock()
			case strings.HasSuffix(path, "/stop"):
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				next.ServeHTTP(rec, r)
				var answer struct{ State string }
				_ = json.Unmarshal(rec.Body.Bytes(), &answer)
				mu.Lock()
				forced = append(forced, fmt.Sprintf("%s %d %s", body, rec.Code, answer.State))
				mu.Unlock()
			default:
				next.ServeHTTP(w, r)
				return
			}
			relay(w, rec)
		})
	}
	h = newHarnessWrapped(t, sb, wrap)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)
	workID := h.workID(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	defer cancel() // so a failure below still unblocks a held tool via ctx
	waitDone(t, done)
	close(sb.gate)
	waitExit(t, cancel, errc)

	mu.Lock()
	defer mu.Unlock()
	if graceful["state"] != "stopping" || graceful["stop_requested_at"] == nil || graceful["stopped_at"] != nil {
		t.Errorf("graceful stop between the ack and the claim = %v, want stopping with stop_requested_at and no stopped_at", graceful)
	}
	if liveAfter != 0 {
		t.Errorf("live tool_exec items after the graceful stop = %d, want 0 (nothing re-armed yet)", liveAfter)
	}
	if len(claims) != 1 || !strings.HasPrefix(claims[0], "200 ") ||
		!strings.Contains(claims[0], `"state":"stopping"`) || !strings.Contains(claims[0], `"lease_extended":false`) ||
		!strings.Contains(claims[0], `"last_heartbeat":""`) {
		t.Errorf("heartbeats = %q, want the one claim answered 200 stopping, not extended, last_heartbeat \"\"", claims)
	}
	if len(forced) != 1 || forced[0] != `{"force":true} 200 stopped` {
		t.Errorf("stops the worker sent = %q, want its one force stop, answered 200 stopped", forced)
	}

	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (the worker finished the stop it was told of)", got)
	}
	var requested time.Time
	var beat *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT stop_requested_at, last_heartbeat FROM work_items WHERE id = $1`, workID).Scan(&requested, &beat); err != nil {
		t.Fatal(err)
	}
	if want, _ := json.Marshal(requested.UTC()); graceful["stop_requested_at"] != strings.Trim(string(want), `"`) || beat != nil {
		t.Errorf("stop_requested_at %s, last_heartbeat %v; want the graceful stop's %v kept, and no beat recorded",
			want, beat, graceful["stop_requested_at"])
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was cancelled on the claim's answer)", got)
	}
	if _, ran := sb.files["/workspace/out.txt"]; ran {
		t.Error("the tool finished its write; the run must be cancelled on the claim's answer")
	}
	var others, queued int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE state = 'queued') FROM work_items
		  WHERE session_id = $1 AND kind = 'tool_exec' AND id <> $2`, h.sid.String(), workID).Scan(&others, &queued); err != nil {
		t.Fatal(err)
	}
	if others != 1 || queued != 1 {
		t.Errorf("tool_exec items besides the stopped one = %d (%d queued), want exactly the one re-armed item", others, queued)
	}
}

// stopFinality reads the two row fields a finished stop settles and the wire's
// work object either renders (stopped_at) or does not carry at all
// (lease_expires_at) — both null-until-set, hence the pointers.
func (h *harness) stopFinality(t *testing.T) (stoppedAt, leaseExpiresAt *time.Time) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(),
		`SELECT stopped_at, lease_expires_at `+firstItem,
		h.sid.String()).Scan(&stoppedAt, &leaseExpiresAt); err != nil {
		t.Fatalf("read stop finality: %v", err)
	}
	return stoppedAt, leaseExpiresAt
}

// TestWorkerLeaseLossDoesNotStopTheItem is the counterweight to the wind-down
// above, and the reason the worker cannot simply stop every item it lets go of.
// A heartbeat rejected with 412 means the row moved out from under this worker —
// the reclaim race — so another worker may now hold the item and stopping it
// would terminate that worker's run. Only a stop the control plane actually
// asked for is finished by the worker; a lease merely lost leaves the item live
// for reclaim, and no stop is sent for it at all.
func TestWorkerLeaseLossDoesNotStopTheItem(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	o := &beatOverride{} // never armed: it only counts the stops
	h := newHarnessWrapped(t, sb, o.wrap)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	<-sb.entered // the tool is held open, mid-run
	waitForState(t, h, "active")
	// Move last_heartbeat out from under the worker, which is exactly what a
	// reclaim does: its next beat echoes a value the row no longer carries, and
	// the control plane answers the mismatch with a 412.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE work_items SET last_heartbeat = now() + interval '1 second'
		 WHERE session_id = $1 AND kind = 'tool_exec'`, h.sid.String()); err != nil {
		t.Fatalf("displace the heartbeat: %v", err)
	}

	waitDone(t, done)
	if got := h.workState(t); got != "active" {
		t.Errorf("work item state = %q, want it left active for reclaim (a lost lease must not stop it)", got)
	}
	if got := o.stops.Load(); got != 0 {
		t.Errorf("stops sent = %d, want none for a lost lease", got)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was cancelled)", got)
	}
	close(sb.gate) // release, though the tool already returned via cancellation
	waitExit(t, cancel, errc)
}

// relay writes a recorded answer through to the worker.
func relay(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// beatOverride stands between the worker and the control plane for the tests of
// how a heartbeat ends. It answers a heartbeat itself, with beatStatus and
// beatBody, while armed and, when first is set, for the first that many beats.
// Other beats reach the control plane, and when ttl is set their ttl_seconds is
// rewritten to it, so the worker's staleness ceiling is short while the lease
// the control plane holds is not. Every beat and stop the worker sends is
// counted, and every poll once a stop has been sent. A stop that lands re-arms
// the session's unanswered calls as a new item (the work API's stopWork), which
// the worker would poll at once and run under the same override, so from then
// on polls are answered empty, unless repoll is set, and a test sees one item's
// stops alone.
type beatOverride struct {
	ttl        int
	first      int32
	beatStatus int
	beatBody   string
	repoll     bool

	armed          atomic.Bool
	beats          atomic.Int32
	stops          atomic.Int32
	landed         atomic.Bool
	pollsAfterStop atomic.Int32
}

func (o *beatOverride) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case strings.HasSuffix(path, "/heartbeat"):
			if n := o.beats.Add(1); o.armed.Load() || n <= o.first {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(o.beatStatus)
				_, _ = io.WriteString(w, o.beatBody)
				return
			}
			if o.ttl == 0 {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			var beat map[string]any
			if rec.Code == http.StatusOK && json.Unmarshal(body, &beat) == nil {
				beat["ttl_seconds"] = o.ttl
				body, _ = json.Marshal(beat)
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body)
		case strings.HasSuffix(path, "/stop"):
			o.stops.Add(1)
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			if rec.Code == http.StatusOK {
				o.landed.Store(true)
			}
			relay(w, rec)
		case strings.HasSuffix(path, "/work/poll"):
			if o.stops.Load() > 0 {
				o.pollsAfterStop.Add(1)
			}
			if o.landed.Load() && !o.repoll {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "null")
				return
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// heldRun stages one tool on a harness behind wrap and starts a worker on it,
// its client retrying nothing so each answer a test stages reaches the
// heartbeat as it is. It returns once the claim has reached the control plane
// and the tool is entered and held, so the run ends only as the caller then
// provokes: by arming an override, say, or by releasing the tool. The caller
// waits for the item with waitDone.
func heldRun(t *testing.T, wrap func(http.Handler) http.Handler) (h *harness, sb *fakeSandbox, done <-chan string, cancel context.CancelFunc, errc <-chan error) {
	t.Helper()
	sb = &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h = newHarnessWrapped(t, sb, wrap)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)
	h.client = h.noRetryClient()
	var w *Worker
	w, done = h.newWorker(Config{})
	cancel, errc = runWorker(w)
	t.Cleanup(cancel) // so a failure below still unblocks the held tool via ctx
	select {
	case <-sb.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the held tool was never entered")
	}
	waitForState(t, h, "active")
	return h, sb, done, cancel, errc
}

// TestWorkerLeaseNotExtendedStopsTheItem: a beat answered 200 with the lease not
// extended beside a live state cancels the run and force-stops the item. The
// reference worker reads a declined lease as the control plane's stop, not a
// lost lease, and force-stops the item on its way out (checked against
// anthropic-sdk-go v1.70.1 — worker.go runHeartbeat) (#813). This control plane
// declines a lease only beside stopping or stopped, so the answer is staged.
func TestWorkerLeaseNotExtendedStopsTheItem(t *testing.T) {
	o := &beatOverride{beatStatus: http.StatusOK,
		beatBody: `{"type":"work_heartbeat","lease_extended":false,"state":"active","last_heartbeat":"2026-10-01T00:00:00Z","ttl_seconds":30}`}
	h, sb, done, cancel, errc := heldRun(t, o.wrap)
	o.armed.Store(true)

	waitDone(t, done) // the held tool returns only through cancellation
	if got := o.stops.Load(); got != 1 {
		t.Errorf("stops sent = %d, want the one force stop", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped", got)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was cancelled)", got)
	}
	close(sb.gate)
	waitExit(t, cancel, errc)
}

// TestWorkerRejectedHeartbeatStopsTheItem: a beat refused with a 4xx that
// retrying will not fix, other than 412, cancels the run and force-stops the
// item. The reference worker ends its heartbeat on such a refusal with a reason
// that is not a lost lease, so it force-stops the item on its way out (checked
// against anthropic-sdk-go v1.70.1 — worker.go runHeartbeat) (#813). Only a 412
// and a lapsed lease release the item unstopped. The refusal staged here comes
// after a claim that landed, so the worker polls on without the backoff an item
// refused from its claim earns (TestWorkerBacksOffAfterItemsRefusedFromTheirClaim).
func TestWorkerRejectedHeartbeatStopsTheItem(t *testing.T) {
	for _, tc := range []struct {
		status  int
		errType string
	}{
		{http.StatusNotFound, "not_found_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusUnauthorized, "authentication_error"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			warnings := captureWarnings(t)
			o := &beatOverride{beatStatus: tc.status,
				beatBody: `{"type":"error","error":{"type":"` + tc.errType + `","message":"refused"}}`}
			h, sb, done, cancel, errc := heldRun(t, o.wrap)
			o.armed.Store(true)

			waitDone(t, done)
			if got := o.stops.Load(); got != 1 {
				t.Errorf("stops sent = %d, want the one force stop", got)
			}
			if got := h.workState(t); got != "stopped" {
				t.Errorf("work item state = %q, want stopped", got)
			}
			if got := len(h.results(t)); got != 0 {
				t.Errorf("user.tool_result = %d, want 0 (the run was cancelled)", got)
			}
			close(sb.gate)
			waitExit(t, cancel, errc)
			// Run has returned, so a backoff it began is logged by now.
			if out := warnings(); strings.Contains(out, "before its lease was extended") {
				t.Errorf("an item whose claim landed was followed by a backoff:\n%s", out)
			}
		})
	}
}

// TestWorkerRevokedKeyEndsTheRunThenTheWorker: a revoked environment key gets
// the worker's beat, its stop and its next poll each refused 401. The refused
// beat cancels the run and is followed by the force stop the reference sends
// after one (checked against anthropic-sdk-go v1.70.1 — worker.go
// EnvironmentWorker.handleItem). Refused in turn, that stop is only logged and
// leaves the item active; the poll after it ends the worker with the auth
// error, as any poll refused 401 or 403 does.
func TestWorkerRevokedKeyEndsTheRunThenTheWorker(t *testing.T) {
	warnings := captureWarnings(t)
	o := &beatOverride{} // never armed: it only counts the stops
	h, sb, done, _, errc := heldRun(t, o.wrap)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environment_keys SET revoked_at = now() WHERE environment_id = $1`, h.envID.String()); err != nil {
		t.Fatalf("revoke the key: %v", err)
	}

	waitDone(t, done)
	select {
	case err := <-errc:
		if !isAuthError(err) {
			t.Errorf("worker Run returned %v, want its poll's 401", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker Run did not return once its poll was refused")
	}
	if got := o.stops.Load(); got != 1 {
		t.Errorf("stops sent = %d, want the one force stop", got)
	}
	if out := warnings(); !strings.Contains(out, "heartbeat rejected") ||
		!strings.Contains(out, "force-stop failed") || !strings.Contains(out, "401") {
		t.Errorf("want the refused beat and the refused stop logged:\n%s", out)
	}
	if got := h.workState(t); got != "active" {
		t.Errorf("work item state = %q, want active (the stop never reached it)", got)
	}
	close(sb.gate)
}

// TestWorkerRejectedBeatOnAReissuedIDLeavesTheReplacementAlone drives the path
// #813 opened beside #62's rotation. This worker's lease lapses while its run is
// held, and a replacement re-polls the item, which mints it a fresh id, and
// claims it; only then does this worker's next beat reach the control plane. It
// 404s on the retired id, so the worker cancels its run and sends the force
// stop the reference sends after a rejected beat (checked against
// anthropic-sdk-go v1.70.1 — worker.go EnvironmentWorker.handleItem), under the
// id it was handed. That stop 404s too and is only logged, the worker polls
// on, and the replacement's item stays active, its next beat still extending
// the lease.
func TestWorkerRejectedBeatOnAReissuedIDLeavesTheReplacementAlone(t *testing.T) {
	ctx := context.Background()
	warnings := captureWarnings(t)
	var holding atomic.Bool
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/heartbeat") && holding.Load() {
				select {
				case parked <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done(): // the beat timed out; the worker retries it
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	o := &beatOverride{} // never armed: it counts the stops and the polls after them
	h, sb, done, cancel, errc := heldRun(t, func(next http.Handler) http.Handler { return o.wrap(hold(next)) })
	stale := h.workID(t)

	// Park this worker's next beat, so nothing renews its lease while the lease
	// lapses and the replacement takes the item.
	holding.Store(true)
	select {
	case <-parked:
	case <-time.After(15 * time.Second):
		t.Fatal("the worker's next beat never arrived")
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, stale); err != nil {
		t.Fatal(err)
	}
	q := queue.New(h.pool)
	repl, err := q.Poll(ctx, h.envID, time.Minute)
	if err != nil || repl == nil {
		t.Fatalf("replacement poll: %+v %v", repl, err)
	}
	if repl.ID.String() == stale {
		t.Fatalf("the reclaim re-offered the retired work id %s", stale)
	}
	if _, err := q.Ack(ctx, h.envID, repl.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := q.Heartbeat(ctx, h.envID, repl.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatal(err)
	}
	holding.Store(false)
	close(release)

	waitDone(t, done)
	for i := 0; i < 300 && o.pollsAfterStop.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if o.pollsAfterStop.Load() == 0 {
		t.Error("the worker sent no poll after its stop, want it to poll on")
	}
	if got := o.stops.Load(); got != 1 {
		t.Errorf("stops sent = %d, want the one force stop", got)
	}
	if out := warnings(); !strings.Contains(out, "heartbeat rejected") ||
		!strings.Contains(out, "force-stop failed") || !strings.Contains(out, "404") {
		t.Errorf("want the beat and the stop on the retired id logged as 404s:\n%s", out)
	}
	var state string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, repl.ID.String()).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Errorf("the replacement's item is %q, want active", state)
	}
	beat, err := q.Heartbeat(ctx, h.envID, repl.ID, claim.LastHeartbeat.UTC().Format(time.RFC3339Nano), 30)
	if err != nil || !beat.LeaseExtended {
		t.Errorf("the replacement's next beat = %+v, %v; want its lease extended", beat, err)
	}
	close(sb.gate)
	waitExit(t, cancel, errc)
}

// TestWorkerLeaseLostAsTheRunFinishesIsNotStopped: a 412 read as the run
// finishes still counts as a lost lease, so the completed item is not stopped.
// The reference checks a beat's 412 before its context for this race (checked
// against anthropic-sdk-go v1.70.1 — worker.go runHeartbeat). It is made
// certain at the one point it can happen: the SDK hands a status back only if
// the caller's context was live when the response arrived, so the claim is
// answered 412 at once, but its body is held until the run's finish cancels
// the beat.
func TestWorkerLeaseLostAsTheRunFinishesIsNotStopped(t *testing.T) {
	o := &beatOverride{} // never armed: it only counts the stops
	h := newHarnessWrapped(t, &fakeSandbox{}, o.wrap)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)
	released := make(chan error, 1)
	h.client = h.noRetryClient(option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
			return next(r)
		}
		return &http.Response{
			StatusCode: http.StatusPreconditionFailed,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: &heldBody{ctx: r.Context(), released: released,
				body: `{"type":"error","error":{"type":"invalid_request_error","message":"lease lost"}}`},
			Request: r,
		}, nil
	}))
	// A long interval, so the claim's request timeout cannot end the hold first.
	w, done := h.newWorker(Config{HeartbeatInterval: time.Minute})
	cancel, errc := runWorker(w)

	waitDone(t, done)
	if err := <-released; !errors.Is(err, context.Canceled) {
		t.Fatalf("the claim's body was released by %v, want the run's finish", err)
	}
	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (the run finished)", got)
	}
	if got := o.stops.Load(); got != 0 {
		t.Errorf("stops sent = %d, want none for a lost lease", got)
	}
	if got := h.workState(t); got != "starting" {
		t.Errorf("work item state = %q, want it left starting", got)
	}
	waitExit(t, cancel, errc)
}

// heldBody yields nothing until ctx ends, then body, and reports on released
// what ended ctx.
type heldBody struct {
	ctx      context.Context
	released chan<- error
	body     string
	r        io.Reader
}

func (b *heldBody) Read(p []byte) (int, error) {
	if b.r == nil {
		<-b.ctx.Done()
		b.released <- b.ctx.Err()
		b.r = strings.NewReader(b.body)
	}
	return b.r.Read(p)
}

func (b *heldBody) Close() error { return nil }

// TestWorkerBacksOffAfterItemsRefusedFromTheirClaim: an item the heartbeat stops
// before any beat extended its lease is followed by a backoff before the next
// poll. The stop re-arms the session's unanswered call as a new item, so
// without one a control plane that refuses every beat spins the worker at the
// speed of a round trip (#813). Every beat is refused here, the claim included,
// and the tool never answers, so each stop re-arms it; the second item ends at
// least the first backoff's floor after the first.
func TestWorkerBacksOffAfterItemsRefusedFromTheirClaim(t *testing.T) {
	o := &beatOverride{beatStatus: http.StatusNotFound, repoll: true,
		beatBody: `{"type":"error","error":{"type":"not_found_error","message":"refused"}}`}
	o.armed.Store(true)
	sb := &fakeSandbox{gate: make(chan struct{})} // never released: no run answers its tool
	h := newHarnessWrapped(t, sb, o.wrap)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)
	h.client = h.noRetryClient()
	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	defer cancel()

	waitDone(t, done)
	first := time.Now()
	waitDone(t, done)
	// The floor is backoffBase/2, jitter's; the margin absorbs this goroutine
	// reading the first item late.
	if gap := time.Since(first); gap < backoffBase/2-100*time.Millisecond {
		t.Errorf("the second refused item ended %v after the first, want a backoff of at least %v between them", gap, backoffBase/2)
	}
	if got := o.stops.Load(); got != 2 {
		t.Errorf("stops sent = %d, want one per item", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerStaleLeaseDoesNotStopTheItem: transient beat failures that outlast
// the lease's TTL release the item without a stop. The lease has lapsed by then,
// so another worker may hold the item, and the reference releases it unstopped
// too (checked against anthropic-sdk-go v1.70.1 — worker.go runHeartbeat).
// ttl_seconds is rewritten to 1 so the ceiling falls within the test, while the
// control plane's own lease stays long enough that no poll reclaims the item.
func TestWorkerStaleLeaseDoesNotStopTheItem(t *testing.T) {
	warnings := captureWarnings(t)
	o := &beatOverride{ttl: 1, beatStatus: http.StatusServiceUnavailable,
		beatBody: `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`}
	h, sb, done, cancel, errc := heldRun(t, o.wrap)
	o.armed.Store(true)

	waitDone(t, done)
	if out := warnings(); !strings.Contains(out, "heartbeat stale beyond lease TTL") {
		t.Fatalf("the run did not end on the staleness ceiling:\n%s", out)
	}
	if got := o.stops.Load(); got != 0 {
		t.Errorf("stops sent = %d, want none for a lapsed lease", got)
	}
	if got := h.workState(t); got != "active" {
		t.Errorf("work item state = %q, want it left active for reclaim", got)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the run was cancelled)", got)
	}
	close(sb.gate)
	waitExit(t, cancel, errc)
}

// TestWorkerStalledRunReleasesTheItem is the wedge #383 is about, in the BYOC
// lane: a sandbox call that never returns leaves the run blocked while the
// heartbeat happily beats on, so the item is neither progressing nor
// reclaimable — the documented recovery (the lease lapses, another worker takes
// it) never fires, because nothing crashed. A run that reports no progress for
// its whole stall budget is cancelled, and the beating stops with it: the item
// is left live, its lease lapses, and the control plane can re-offer it.
func TestWorkerStalledRunReleasesTheItem(t *testing.T) {
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	// A second of budget, not a few hundred milliseconds: the clock starts before
	// the liveness read and the provision, so a tight budget could cancel the run
	// before the gated tool is ever entered — and then the receive below would
	// block until go test's package alarm, which is the very failure this change
	// exists to remove (#318).
	w, done := h.newWorker(Config{StallTimeout: time.Second})
	cancel, errc := runWorker(w)

	// The tool is held open mid-run and never returns.
	select {
	case <-sb.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated tool was never entered")
	}
	waitForState(t, h, "active")
	// Without the guard the run is held by the gate forever and this never fires.
	waitDone(t, done)

	if got := h.workState(t); got != "active" {
		t.Errorf("work item state = %q, want it left active for reclaim (a stalled run must not stop the item)", got)
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the wedged tool never answered)", got)
	}
	// The lease is given up rather than held: the beats stop, so the control
	// plane's TTL (30s, far outside this window) lapses and the item is free.
	mark := h.lastHeartbeat(t)
	time.Sleep(200 * time.Millisecond) // ~10 beats at the test cadence
	if got := h.lastHeartbeat(t); got.After(mark) {
		t.Errorf("the heartbeat kept beating after the stall (%v -> %v); a stalled item must be released", mark, got)
	}
	close(sb.gate)
	waitExit(t, cancel, errc)
}

// TestWorkerLongButMovingRunKeepsItsItem is the other half of the guard, and the
// one that decides whether it can ship: a run that is long overall but keeps
// finishing steps must never be killed for being long. The budget is a second
// against a run of thirty 50ms tools — one and a half times the budget in total,
// a twentieth of it per step, so a result post that takes twenty times its usual
// round trip still lands inside it and this test does not become the flake class
// it is defending against — and only the per-tool progress reports keep the run
// alive: it finishes normally, every tool answered, the item stopped.
func TestWorkerLongButMovingRunKeepsItsItem(t *testing.T) {
	const tools = 30
	sb := &fakeSandbox{delay: 50 * time.Millisecond}
	h := newHarness(t, sb)
	uses := make([]string, tools)
	for i := range uses {
		uses[i] = writeUse(fmt.Sprintf("out%d.txt", i), "hi")
	}
	h.suspend(t, uses...)
	h.enqueueWork(t)

	w, done := h.newWorker(Config{StallTimeout: time.Second})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if got := len(h.results(t)); got != tools {
		t.Errorf("user.tool_result = %d, want %d (a moving run must not be cut short)", got, tools)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (the run completed)", got)
	}
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
	conflict := &sdk.Error{StatusCode: 409}

	if !isAuthError(auth) || !isAuthError(perm) {
		t.Error("401/403 should be auth errors (fatal poll)")
	}
	if isAuthError(conflict) || isAuthError(errors.New("network")) {
		t.Error("409 and non-API errors are not auth errors")
	}
	// The reference's fatal set (checked against anthropic-sdk-go v1.70.1 —
	// poller.go isFatal4xx): every 4xx but 408, 409 and 429.
	for _, code := range []int{400, 401, 403, 404, 412, 422} {
		if !isFatalHeartbeat(&sdk.Error{StatusCode: code}) {
			t.Errorf("%d should be a fatal heartbeat error", code)
		}
	}
	for _, code := range []int{408, 409, 429, 500, 503} {
		if isFatalHeartbeat(&sdk.Error{StatusCode: code}) {
			t.Errorf("%d is transient, not a fatal heartbeat error", code)
		}
	}
	if isFatalHeartbeat(errors.New("timeout")) {
		t.Error("a network error is transient, not a fatal heartbeat error")
	}
	if !isStatus(conflict, 409) || isStatus(conflict, 412) {
		t.Error("isStatus must match the exact code")
	}
}

// TestHbExitMustStop pins which heartbeat exits stop the item whatever the
// run's outcome. The rule is written as the reference's is, by the exits that
// must not stop it (checked against anthropic-sdk-go v1.70.1 — worker.go
// leaseEndReason.lost), so an exit added later stops the item until it is
// listed among them.
func TestHbExitMustStop(t *testing.T) {
	for exit, want := range map[hbExit]bool{
		hbExitCancelled:     false,
		hbExitStopRequested: true,
		hbExitRejected:      true,
		hbExitLeaseLost:     false,
		hbExitStalled:       false,
		hbExitStalled + 1:   true, // one added later
	} {
		if got := exit.mustStop(); got != want {
			t.Errorf("hbExit(%d).mustStop() = %v, want %v", exit, got, want)
		}
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

// TestWorkerTransientHeartbeatRecovers: beats answered with a status retrying
// can fix must not abandon the run — the worker retries, recovers, claims the
// lease, and still finishes the item. The status is 409, the one #813 moved out
// of the fatal set into the transient one, as the reference has it (checked
// against anthropic-sdk-go v1.70.1 — poller.go isFatal4xx); TestErrorClassification
// pins the rest of that set. heldRun's client retries nothing, so each failure
// reaches the worker's own retry loop rather than being absorbed by the SDK,
// and the tool is held open until the lease is claimed so completion is owned.
func TestWorkerTransientHeartbeatRecovers(t *testing.T) {
	o := &beatOverride{first: 2, beatStatus: http.StatusConflict}
	// A 409 read as fatal cancels the run before its tool or its claim, and
	// heldRun fails waiting for them.
	h, sb, done, cancel, errc := heldRun(t, o.wrap)
	close(sb.gate) // two beats failed, then the third claimed the lease: release the tool

	waitDone(t, done)
	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (recovered and finished)", got)
	}
	if got := h.workState(t); got != "stopped" {
		t.Errorf("work item state = %q, want stopped (owned and completed)", got)
	}
	if got := o.beats.Load(); got < 3 {
		t.Errorf("heartbeat attempts = %d, want the two failures plus a recovery", got)
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
	live, coordinator, err := w.sessionLive(context.Background(), "sesn_does_not_exist_00000000")
	if err == nil {
		t.Fatal("sessionLive returned nil error for a missing session")
	}
	if live || coordinator {
		t.Errorf("sessionLive on a missing session = (live %v, coordinator %v), want both false", live, coordinator)
	}
}

// TestSessionLiveReportsCoordinatorMode: the mode the driver switches on comes
// from the session snapshot the liveness gate already reads — a non-empty
// multiagent roster and nothing else. A single-agent session renders
// "multiagent": null, which is the zero struct rather than a missing field, so
// the test pins both halves against the same wire read.
func TestSessionLiveReportsCoordinatorMode(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	w := NewWorker(h.client, h.prov, Config{EnvironmentID: h.envID.String()}.withDefaults())

	live, coordinator, err := w.sessionLive(context.Background(), h.sid.String())
	if err != nil {
		t.Fatalf("sessionLive: %v", err)
	}
	if !live || coordinator {
		t.Errorf("single-agent session = (live %v, coordinator %v), want (true, false)", live, coordinator)
	}

	h.refRoster(t, rosterMember{name: "researcher"})
	live, coordinator, err = w.sessionLive(context.Background(), h.sid.String())
	if err != nil {
		t.Fatalf("sessionLive after the roster: %v", err)
	}
	if !live || !coordinator {
		t.Errorf("coordinator session = (live %v, coordinator %v), want (true, true)", live, coordinator)
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
