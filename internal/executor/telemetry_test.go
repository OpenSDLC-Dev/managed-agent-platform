package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordSpans routes the package's tracer through a recorder for one test,
// as production wiring routes it through the global provider.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return recorder.Ended
}

// readUse is a read of one path — the cheapest tool that can fail at the tool
// level (a missing file) without faulting the sandbox.
func readUse(path string) string {
	b, _ := json.Marshal(map[string]any{
		"name": "read", "input": map[string]string{"file_path": path},
	})
	return string(b)
}

// toolExecSpan is the one tool_exec span the run produced.
func toolExecSpan(t *testing.T, spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == "tool_exec" {
			return s
		}
	}
	t.Fatal("no tool_exec span recorded: the platform-managed path never joins the session's trace")
	return nil
}

// TestToolExecJoinsTheEnqueuingTurnsTrace: the work item carries the trace
// context of the turn that enqueued it, and the executor must run the tools
// under it. Without this a session's model turns and its platform-managed tool
// runs land in two unrelated traces — which is exactly what the queue's
// trace_context column exists to prevent, and what the BYOC worker already
// gets right on the same protocol.
func TestToolExecJoinsTheEnqueuingTurnsTrace(t *testing.T) {
	ended := recordSpans(t)

	sb := &fakeSandbox{}
	h := newHarness(t, sb)

	// Suspend the turn from inside a known span, as a real brain does: the
	// queue captures its W3C trace context onto the item it enqueues.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	turnCtx := trace.ContextWithSpanContext(context.Background(), sc)
	h.suspendUnder(t, turnCtx, writeUse("out.txt", "hello"))

	// The executor's own context is unrelated to the turn's — the item is the
	// only link, which is the point.
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	toolSpan := toolExecSpan(t, ended())
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

// A backend fault is the executor breaking, not the agent: the item is left for
// reclaim and no tool result ever lands. The span is the only place a trace
// reader can see that, so ending it green would present a broken sandbox as a
// successful tool run — the exact failure the trace exists to surface.
func TestToolExecSpanRecordsABackendFault(t *testing.T) {
	ended := recordSpans(t)

	h := newHarness(t, &fakeSandbox{writeErr: errors.New("connection refused")})
	h.exec.onFault = func(*queue.Item, error) {}
	h.suspend(t, writeUse("out.txt", "hi"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	span := toolExecSpan(t, ended())
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("faulted run's span status = %v, want %v", got, codes.Error)
	}
	if span.Status().Description == "" {
		t.Error("faulted run's span carries no description, so the trace never says why")
	}
}

// A tool the model can recover from — a missing file, a nonzero exit — is the
// agent doing agent things, and marking the span errored for it would light up
// every dashboard on ordinary agent behaviour. Only the platform's own faults
// are span errors; tool-level failures are counted by the toolset metric's
// error.type and are on the event log verbatim.
func TestToolExecSpanIgnoresAToolLevelError(t *testing.T) {
	ended := recordSpans(t)

	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, readUse("nope.txt"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the tool must have run and failed)", len(results))
	}
	if got := toolExecSpan(t, ended()).Status().Code; got != codes.Unset {
		t.Errorf("span status = %v on a tool-level error, want unset", got)
	}
}
