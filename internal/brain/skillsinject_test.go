package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestSkillsInjectedIntoModelRequest is the wiring's own test: a session whose
// agent references skills must have the Level-1 block reach the actual provider
// request's system prompt, and a dangling reference must count as one resolve
// miss without failing the turn. Everything about resolution is proven in the
// unit tests; this proves the brain actually calls it and threads the result
// into buildRequest.
func TestSkillsInjectedIntoModelRequest(t *testing.T) {
	collect := collectBrainMetrics(t)
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
