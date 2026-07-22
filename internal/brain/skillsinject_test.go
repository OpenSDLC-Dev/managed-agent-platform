package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestSkillsInjectedIntoModelRequest is the wiring's own test: a session whose
// agent references skills must have the Level-1 block reach the actual provider
// request's system prompt, and a dangling reference must count as one resolve
// miss without failing the turn. Everything about resolution is proven in the
// unit tests; this proves the brain actually calls it and threads the result
// into buildRequest.
func TestSkillsInjectedIntoModelRequest(t *testing.T) {
	collect := collectBrainMetrics(t)
	// A span recorder so the model_request span's skills.* attributes can be read
	// back — otherwise a swapped/dropped SetAttributes would leave every other
	// assertion green.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "ok"),
		done("end_turn", 3),
	}}, nil)
	ctx := context.Background()

	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ('skill_a','custom','alpha','100')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory)
		 VALUES ('skillver_a100','skill_a','100','alpha','Alpha skill','alpha')`); err != nil {
		t.Fatal(err)
	}
	// Point the session's resolved agent at the skill plus a dangling reference.
	resolved := `{"type":"agent","id":"agent_fixture","version":1,"name":"fixture",` +
		`"model":{"id":"fixture-model"},"system":"base prompt","description":"",` +
		`"tools":[],"mcp_servers":[],` +
		`"skills":[{"type":"skill","skill_id":"skill_a","version":"100"},` +
		`{"type":"skill","skill_id":"skill_gone","version":"latest"}],"multiagent":null}`
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resolved_agent=$1 WHERE id=$2`,
		resolved, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "hi")
	h.runOnce(t)

	sys := h.provider.calls[0].System
	if !strings.HasPrefix(sys, "base prompt") {
		t.Errorf("system dropped the agent prompt: %q", sys)
	}
	if !strings.Contains(sys, "alpha - Alpha skill (skills/alpha/SKILL.md)") {
		t.Errorf("system missing the injected skill block: %q", sys)
	}
	// The dangling reference is exactly one resolve miss; the turn still ran.
	if got := resolveMissCount(t, collect()); got != 1 {
		t.Errorf("skills.resolve.misses = %d, want 1", got)
	}
	// The model_request span records the injection: one skill resolved, a
	// non-empty block. These are the exact ints emitted in brain.go, so a
	// name/value swap or a dropped SetAttributes fails here.
	if got := spanIntAttr(t, recorder, "model_request", "skills.injected"); got != 1 {
		t.Errorf("span skills.injected = %d, want 1", got)
	}
	if got := spanIntAttr(t, recorder, "model_request", "skills.block_chars"); got != int64(len(sys)-len("base prompt\n\n")) {
		t.Errorf("span skills.block_chars = %d, want the injected block length", got)
	}
}

// spanIntAttr returns the named int attribute of the first span with the given
// name, failing the test if the span or the attribute is absent.
func spanIntAttr(t *testing.T, recorder *tracetest.SpanRecorder, spanName, key string) int64 {
	t.Helper()
	for _, s := range recorder.Ended() {
		if s.Name() != spanName {
			continue
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == key {
				return kv.Value.AsInt64()
			}
		}
		t.Fatalf("span %q has no %q attribute", spanName, key)
	}
	t.Fatalf("no %q span recorded", spanName)
	return 0
}

// TestResolveMissCountedWhenTurnFailsEarly pins the miss-counter timing: a
// resolve miss is a fact of request assembly, so it must reach
// skills.resolve.misses even when the turn fails before the model request. The
// agent carries a dangling skill (one miss) and a malformed tool that makes
// buildRequest reject the turn — before the fix flushed the counter after
// StartModelRequest, this miss was logged but never counted.
func TestResolveMissCountedWhenTurnFailsEarly(t *testing.T) {
	collect := collectBrainMetrics(t)
	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "unused"),
		done("end_turn", 3),
	}}, nil)
	ctx := context.Background()

	resolved := `{"type":"agent","id":"agent_fixture","version":1,"name":"fixture",` +
		`"model":{"id":"fixture-model"},"system":"base prompt","description":"",` +
		`"tools":["not-a-tool"],"mcp_servers":[],` +
		`"skills":[{"type":"skill","skill_id":"skill_gone","version":"latest"}],"multiagent":null}`
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resolved_agent=$1 WHERE id=$2`,
		resolved, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "hi")
	h.runOnce(t)

	// The turn failed at buildRequest (the malformed tool), so no model request
	// ran — yet the dangling skill is still one counted miss.
	if n := h.countType(t, "session.error"); n != 1 {
		t.Fatalf("session.error count = %d, want 1 (the turn should have failed early)", n)
	}
	if got := resolveMissCount(t, collect()); got != 1 {
		t.Errorf("skills.resolve.misses = %d, want 1 (a miss must count even when the turn fails early)", got)
	}
}

func resolveMissCount(t *testing.T, rm metricdata.ResourceMetrics) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != brain.MetricSkillResolveMisses {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			for _, dp := range s.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}
