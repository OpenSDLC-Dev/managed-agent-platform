package brain_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

// Every model call the brain makes — a primary turn, a child thread's turn,
// the outcome grader's — names its session in provider.SessionHeader and
// carries the trace of the span it was made under, so the model gateway can
// key its per-session cost and cache locality and continue the trace
// (docs/plan/59_model-gateway.md).
func TestModelCallsCarryTheirSessionAndTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var mu sync.Mutex
	var sessions, parents []string
	replies := []string{"coordinator done", "child done", "all done\nVERDICT: satisfied"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sessions = append(sessions, r.Header.Get(provider.SessionHeader))
		parents = append(parents, r.Header.Get("traceparent"))
		if len(sessions) > len(replies) {
			t.Error("unexpected extra model request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", replies[len(sessions)-1])
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	reg, err := provider.NewRegistry([]provider.Route{{Model: "*", Config: provider.Config{
		Protocol: "anthropic", Model: "upstream-model", BaseURL: srv.URL, APIKey: "test-key",
	}}}, map[string]provider.Factory{"anthropic": anthropic.New})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		brain: brain.New(pool, reg, nil, brain.Config{}), sessionID: sid, envID: envID}
	h.wakeOutcome(t, "finish", 2)
	h.childTurn(t, "finish child")
	h.drain(t)

	recorded := map[trace.SpanID]bool{}
	for _, s := range recorder.Ended() {
		recorded[s.SpanContext().SpanID()] = true
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 3 {
		t.Fatalf("requests = %d, want primary + child + grader", len(sessions))
	}
	for i, call := range []string{"primary", "child", "grader"} {
		if sessions[i] != sid.String() {
			t.Errorf("%s: %s = %q, want %q", call, provider.SessionHeader, sessions[i], sid)
		}
		sc := trace.SpanContextFromContext(telemetry.Extract(context.Background(), map[string]string{"traceparent": parents[i]}))
		if !sc.IsValid() || !recorded[sc.SpanID()] {
			t.Errorf("%s: traceparent %q names no span the brain recorded", call, parents[i])
		}
	}
}
