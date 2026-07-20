package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans routes the package's tracer through a recorder for one test, as
// production wiring routes it through the global provider. Mirrors the
// executor's recordSpans (internal/executor/telemetry_test.go).
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return recorder.Ended
}

// toolExecSpan is the one tool_exec span the run produced.
func toolExecSpan(t *testing.T, spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == "tool_exec" {
			return s
		}
	}
	t.Fatal("no tool_exec span recorded")
	return nil
}

// TestWorkerToolSpanRecordsABackendFault: a sandbox that backend-faults leaves
// the item live for reclaim with the session's tools never answered. The span is
// the only place a trace reader can see that, so ending it green would present a
// broken sandbox as a clean tool run — the exact failure the trace exists to
// surface. Mirrors the executor's TestToolExecSpanRecordsABackendFault.
func TestWorkerToolSpanRecordsABackendFault(t *testing.T) {
	ended := recordSpans(t)

	sb := &fakeSandbox{failPath: "out.txt"}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	span := toolExecSpan(t, ended())
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("backend-faulted run's span status = %v, want %v", got, codes.Error)
	}
	if span.Status().Description == "" {
		t.Error("backend-faulted run's span carries no description, so the trace never says why")
	}
	waitExit(t, cancel, errc)
}

// TestWorkerToolSpanIgnoresAToolLevelError: a read of a missing file is the agent
// doing agent things — the worker posts an is_error result and the turn resumes.
// Marking the span errored for it would light up every trace view on ordinary
// agent behaviour, so the span stays unset. Mirrors the executor's
// TestToolExecSpanIgnoresAToolLevelError.
func TestWorkerToolSpanIgnoresAToolLevelError(t *testing.T) {
	ended := recordSpans(t)

	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, readUse("nope.txt"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)

	if got := len(h.results(t)); got != 1 {
		t.Fatalf("user.tool_result = %d, want 1 (the tool must have run and failed)", got)
	}
	if got := toolExecSpan(t, ended()).Status().Code; got != codes.Unset {
		t.Errorf("tool-level error's span status = %v, want unset", got)
	}
	waitExit(t, cancel, errc)
}

// TestWorkerToolSpanIgnoresACancelledRun: when the control plane stops the item
// mid-run, the heartbeat cancels the in-flight tool. That cancellation is the
// designed lease-loss path, not a platform fault, so the span stays unset —
// erroring it would redden a trace view on routine teardown. This is the case
// where the worker's rule adds to the executor's: the worker's heartbeat cancels
// the run as ordinary lease loss.
func TestWorkerToolSpanIgnoresACancelledRun(t *testing.T) {
	ended := recordSpans(t)

	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)

	<-sb.entered // the tool is held open, mid-run
	// The control plane asks the item to stop; the next heartbeat sees the
	// stopping state and cancels the run, so the held tool unwinds via ctx.
	if _, err := queue.New(h.pool).Stop(context.Background(), h.envID, domain.ID(h.workID(t)), false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}

	waitDone(t, done)
	close(sb.gate) // release, though the tool already returned via cancellation

	if got := toolExecSpan(t, ended()).Status().Code; got != codes.Unset {
		t.Errorf("cancelled run's span status = %v, want unset (a cancellation is not a fault)", got)
	}
	waitExit(t, cancel, errc)
}

// TestReclaimFault pins the classifier handleItem records span status from: a
// genuine platform fault surfaces (to redden the span an operator opens), while
// a fault observed under a cancelled context — or a context.Canceled error —
// reduces to nil, since a cancellation is ordinary teardown, not a fault.
func TestReclaimFault(t *testing.T) {
	fault := errors.New("docker daemon unreachable")
	if got := reclaimFault(context.Background(), fault); got != fault {
		t.Errorf("reclaimFault on a live context = %v, want the platform fault surfaced", got)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := reclaimFault(canceled, fault); got != nil {
		t.Errorf("reclaimFault under a cancelled context = %v, want nil (teardown, not a fault)", got)
	}
	if got := reclaimFault(context.Background(), context.Canceled); got != nil {
		t.Errorf("reclaimFault on a context.Canceled error = %v, want nil (a cancellation)", got)
	}
	if got := reclaimFault(context.Background(), nil); got != nil {
		t.Errorf("reclaimFault on no error = %v, want nil", got)
	}
}
