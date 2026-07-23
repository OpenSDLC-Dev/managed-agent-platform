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

// TestFilesInjectedIntoModelRequest is the file-injection wiring's own test, the
// twin of TestSkillsInjectedIntoModelRequest: a session whose resources[] mount a
// file must have the "Mounted files" block reach the actual provider request's
// system prompt, and a dangling mount must be skipped without failing the turn.
// resolveFilesBlock's resolution is proven in the unit tests; this proves the
// brain actually calls it, threads the result into buildRequest, and records the
// injection on the model_request span — so a dropped call, a passed-through "",
// or a swapped/dropped SetAttributes fails here rather than only under the opt-in
// RUN_EVALS=1 eval.
func TestFilesInjectedIntoModelRequest(t *testing.T) {
	collect := collectBrainMetrics(t)
	// A span recorder so the model_request span's files.* attributes can be read
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

	// A files-table row the mount resolves against, as an upload would plant.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable)
		 VALUES ('file_inj','notes.txt','text/plain',12,false)`); err != nil {
		t.Fatal(err)
	}
	// Point the session's resources[] at that mount plus a dangling one (no row),
	// which must be skipped rather than fail the turn.
	resources := `[{"type":"file","file_id":"file_inj","mount_path":"/mnt/session/uploads/notes.txt"},` +
		`{"type":"file","file_id":"file_gone","mount_path":"/mnt/session/uploads/gone.txt"}]`
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resources=$1 WHERE id=$2`,
		resources, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	// A base prompt with no skills, so the files block is the only injection and
	// its length is everything after the agent prompt and its separator.
	resolved := `{"type":"agent","id":"agent_fixture","version":1,"name":"fixture",` +
		`"model":{"id":"fixture-model"},"system":"base prompt","description":"",` +
		`"tools":[],"mcp_servers":[],"skills":[],"multiagent":null}`
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
	if !strings.Contains(sys, "Mounted files.") {
		t.Errorf("system missing the mounted-files block: %q", sys)
	}
	if !strings.Contains(sys, "/mnt/session/uploads/notes.txt (notes.txt, text/plain, 12 bytes)") {
		t.Errorf("system missing the mount bullet: %q", sys)
	}
	// The dangling mount was skipped: exactly one file injected.
	if got := spanIntAttr(t, recorder, "model_request", "files.injected"); got != 1 {
		t.Errorf("span files.injected = %d, want 1", got)
	}
	// block_chars is the injected block's length. These are the exact ints emitted
	// in brain.go, so a name/value swap or a dropped SetAttributes fails here.
	if got := spanIntAttr(t, recorder, "model_request", "files.block_chars"); got != int64(len(sys)-len("base prompt\n\n")) {
		t.Errorf("span files.block_chars = %d, want the injected block length", got)
	}
	// The dangling mount is one counted resolve miss, the skills-parity metric.
	if got := fileResolveMissCount(t, collect()); got != 1 {
		t.Errorf("files.resolve.misses = %d, want 1", got)
	}
}

// fileResolveMissCount sums the files.resolve.misses counter, the mounted-file
// twin of resolveMissCount (skillsinject_test.go).
func fileResolveMissCount(t *testing.T, rm metricdata.ResourceMetrics) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != brain.MetricFileResolveMisses {
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
