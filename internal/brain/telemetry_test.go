package brain_test

import (
	"context"
	"log"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// collectBrainMetrics routes the process's meter through a reader for one test,
// as production wiring routes it through the global provider.
func collectBrainMetrics(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

func tokenSums(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gen_ai.client.token.usage" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is %T, want an int64 histogram", m.Data)
			}
			for _, dp := range h.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "gen_ai.token.type" {
						out[kv.Value.Emit()] = dp.Sum
					}
				}
			}
		}
	}
	return out
}

// The GenAI metrics are only as good as the brain's call into them. Everything
// about ModelDone is proven in internal/events, but those tests call it
// directly: nothing there notices if the brain stops calling it, and then the
// token histogram silently reports nothing for every turn while the duration
// quietly re-includes the settlement transaction — with the whole suite green.
// This is the wiring's own test, driving a real turn end to end.
func TestATurnReportsWhatTheModelSpent(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 25), // done() reports 10 input tokens
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	got := tokenSums(t, collect())
	if len(got) == 0 {
		t.Fatal("a completed turn recorded no gen_ai.client.token.usage: the brain never told the span what the model spent")
	}
	if got["input"] != 10 || got["output"] != 25 {
		t.Errorf("usage = in %d / out %d, want 10/25", got["input"], got["output"])
	}
}

// tokenPoints returns the token histogram's data points. tokenSums cannot
// answer this test's question: a zero-valued point and an absent point both
// sum to zero, and the whole point of #90 is that those are different facts.
func tokenPoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.HistogramDataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gen_ai.client.token.usage" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is %T, want an int64 histogram", m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// An endpoint that reports no usage — an OpenAI-compatible gateway ignoring
// stream_options.include_usage — must record no token reading at all. Recording
// zeroes instead would dilute the histogram with turns that look free (#90).
func TestATurnWhoseEndpointReportedNoUsageRecordsNoTokens(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		// Deliberately not done(): that helper always attaches usage.
		{Kind: provider.KindDone, StopReason: "end_turn"},
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	if pts := tokenPoints(t, collect()); len(pts) != 0 {
		t.Errorf("recorded %d token data point(s), want none: the endpoint reported no usage", len(pts))
	}
}

// floatPoints returns a float histogram's data points by name.
func floatPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", name, m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// Time to first token is the platform's responsiveness signal, and it is a brain
// fact: the clock starts when the brain claims the work — replay and request
// assembly are latency the user feels — and stops at the first content the model
// streams. Nothing else in the turn observes both boundaries, so this is the
// wiring's own test that the two are captured and recorded.
func TestATurnRecordsTimeToFirstToken(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 25),
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken)
	if len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1", brain.MetricTimeToFirstToken, len(pts))
	}
	if pts[0].Sum <= 0 {
		t.Errorf("time to first token = %vs, want positive (claim precedes the first token)", pts[0].Sum)
	}
	got := map[string]string{}
	for _, kv := range pts[0].Attributes.ToSlice() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["gen_ai.provider.name"] != "fake" {
		t.Errorf("provider attr = %q, want fake", got["gen_ai.provider.name"])
	}
}

// A turn that streams no content — the model went straight to a tool call — has
// no first token to measure. Recording zero would report an instant response that
// never happened, so the metric stays silent, the same absent-is-not-zero rule
// the token histogram follows.
func TestATurnWithNoStreamedContentRecordsNoTimeToFirstToken(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("tu_1", "bash"),
		done("tool_use", 5),
	}}, nil)
	h.wake(t, "run something")
	h.runOnce(t)

	if pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken); len(pts) != 0 {
		t.Errorf("recorded %d first-token point(s) for a turn that streamed no content, want 0", len(pts))
	}
}

// brainStatusCount reads the session.status.transitions counter for one status.
func brainStatusCount(t *testing.T, rm metricdata.ResourceMetrics, status string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != events.MetricSessionStatus {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", events.MetricSessionStatus, m.Data)
			}
			for _, dp := range s.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "session.status" && kv.Value.Emit() == status {
						return dp.Value
					}
				}
			}
		}
	}
	return 0
}

// The brain drives real status transitions the events unit test cannot: a turn
// that settles moves the session running→idle from inside a brain-owned
// transaction (not the AppendWith wrapper), so this proves that commit site
// records too, and only after it commits.
func TestASettledTurnRecordsSessionStatusTransitions(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 5),
	}}, nil)
	h.wake(t, "hello") // idle→running, via the harness's AppendWith
	h.runOnce(t)       // running→idle, via the brain's own settle commit

	rm := collect()
	if got := brainStatusCount(t, rm, "running"); got < 1 {
		t.Errorf("running transitions = %d, want at least 1 (the wake)", got)
	}
	if got := brainStatusCount(t, rm, "idle"); got != 1 {
		t.Errorf("idle transitions = %d, want 1 (the settle)", got)
	}
}

// A reclaim re-enters running on the log (status_rescheduled + status_running)
// but the session is already running, so it moves no column and must count no
// transition — otherwise crash-recovery churn inflates the very metric an
// operator reads to spot it. The wake's one running is all that is counted.
func TestReclaimRecoveryRecordsNoRunningTransition(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "recovered"),
		done("end_turn", 1),
	}}, nil)
	h.wake(t, "hi") // idle→running, records running = 1

	// A previous brain claimed the turn and died: claim with a tiny lease and let
	// it expire, so this run reclaims it.
	item, err := h.queue.Claim(context.Background(), queue.ModelTurn, 30*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("pre-claim: %+v %v", item, err)
	}
	time.Sleep(40 * time.Millisecond)
	h.runOnce(t) // reclaim recovery (no SetStatus) then settle idle

	if got := brainStatusCount(t, collect(), "running"); got != 1 {
		t.Errorf("running transitions = %d, want 1 — the reclaim's running re-entry is a no-op and must not count", got)
	}
}

// A reasoning model streams thinking before any text, so the first thinking delta
// is the first token. This exercises the thinking-delta stamp path (the TTFT tests
// above use text/tool-only streams).
func TestTimeToFirstTokenStampsFirstThinkingDelta(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		{Kind: provider.KindThinkingDelta, Index: 0, Text: "hmm"},
		textChunk(1, "answer"),
		done("end_turn", 3),
	}}, nil)
	h.wake(t, "think then answer")
	h.runOnce(t)

	if pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken); len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1 (the thinking delta is the first token)", brain.MetricTimeToFirstToken, len(pts))
	}
}

// recordBrainSpans routes the process's tracer through a recorder for one test,
// as production wiring routes it through the global provider.
func recordBrainSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return recorder.Ended
}

// namedSpan returns the span the run recorded under name.
func namedSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %q span recorded", name)
	return nil
}

// captureBrainLogs redirects slog for one test and keeps each record's logging
// context alongside its message: the span context in that context is exactly
// what the OTLP bridge reads to correlate a log line to a trace, so it is what
// a correlation claim has to be asserted on.
func captureBrainLogs(t *testing.T) func() []loggedRecord {
	t.Helper()
	h := &capturingHandler{}
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return func() []loggedRecord {
		h.mu.Lock()
		defer h.mu.Unlock()
		return slices.Clone(h.records)
	}
}

type loggedRecord struct {
	message string
	span    trace.SpanContext
}

type capturingHandler struct {
	mu      sync.Mutex
	records []loggedRecord
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, loggedRecord{r.Message, trace.SpanContextFromContext(ctx)})
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// The turn-fault log is the line an operator greps for when a session stalls,
// and uncorrelated it is worth nothing — "lease lost" with no trace to hang it
// on. The executor's tool_exec faults already answer from the span they
// describe (TestFaultLogLandsOnTheToolExecSpan); a failed model turn is the
// more common cause of a stalled session and has to answer the same way, or
// the operator opens the trace and finds every fault except the one that
// stopped the turn. The assertion is on the span id, not just the trace id: a
// parent and its child share a trace id, so only the span id tells a log that
// landed on the turn's own span from one that landed anywhere in its trace.
func TestTurnFaultLogLandsOnTheModelTurnSpan(t *testing.T) {
	ended := recordBrainSpans(t)
	logged := captureBrainLogs(t)

	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_x", "lookup"), done("tool_use", 3)},
	}, nil)
	h.provider.onGenerate = func(int) {
		// Mid-turn the lease expires and a rival claims the item, so
		// settlement fails its lease proof: an infra fault, which abandons the
		// turn to reclaim with nothing on the wire — this log is the only
		// signal it ever happened.
		if _, err := h.pool.Exec(context.Background(),
			`UPDATE work_items SET lease_expires_at = now() - interval '1 second'
			 WHERE session_id = $1`, h.sessionID.String()); err != nil {
			t.Errorf("expire lease: %v", err)
		}
		item, err := h.queue.Claim(context.Background(), queue.ModelTurn, time.Minute)
		if err != nil || item == nil {
			t.Errorf("rival claim: %+v %v", item, err)
		}
	}
	h.wake(t, "hi")

	found, err := h.brain.RunOnce(context.Background())
	if !found || err == nil {
		t.Fatalf("RunOnce = (%v, %v), want found with a lease error", found, err)
	}

	turn := namedSpan(t, ended(), "model_turn")
	records := logged()
	i := slices.IndexFunc(records, func(r loggedRecord) bool {
		return strings.Contains(r.message, "turn failed")
	})
	if i < 0 {
		t.Fatalf("no turn fault logged; records = %v", records)
	}
	if got := records[i].span.TraceID(); got != turn.SpanContext().TraceID() {
		t.Errorf("fault log trace id = %s, want the turn's %s", got, turn.SpanContext().TraceID())
	}
	if got := records[i].span.SpanID(); got != turn.SpanContext().SpanID() {
		t.Errorf("fault log span id = %s, want the model_turn span %s — the failed turn must answer with this log",
			got, turn.SpanContext().SpanID())
	}
	// The log is only reachable because the span is red: an operator scans a
	// trace for the failure and asks that span for its logs.
	if got := turn.Status().Code; got != codes.Error {
		t.Errorf("faulted turn's span status = %v, want %v", got, codes.Error)
	}
	if turn.Status().Description == "" {
		t.Error("faulted turn's span carries no description, so the trace never says why")
	}
}

// The fault above surfaces at settlement — after the model_request span has
// ended — so only a span that covers the whole claimed item can carry it.
// Pinning model_request as its child is what makes model_turn the handling of
// one claimed work item end to end, the edge the executor's tool_exec and the
// BYOC worker's already give their deployment points.
func TestModelRequestSpanIsAChildOfTheModelTurnSpan(t *testing.T) {
	ended := recordBrainSpans(t)

	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "hi"), done("end_turn", 2)}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	spans := ended()
	turn := namedSpan(t, spans, "model_turn")
	if got := turn.SpanKind(); got != trace.SpanKindConsumer {
		t.Errorf("model_turn span kind = %v, want consumer — it stands for the handling of one claimed work item", got)
	}
	if got := namedSpan(t, spans, "model_request").Parent().SpanID(); got != turn.SpanContext().SpanID() {
		t.Errorf("model_request parent span = %s, want the model_turn span %s", got, turn.SpanContext().SpanID())
	}
}
