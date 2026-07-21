package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// statusCount returns the session.status.transitions counter value for one
// status, or 0 when the metric or that attribute value is absent.
func statusCount(rm metricdata.ResourceMetrics, status string) int64 {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != events.MetricSessionStatus {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				return 0
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

// statusPointCount returns the total number of session.status.transitions data
// points, for asserting the metric stayed silent.
func statusPointCount(rm metricdata.ResourceMetrics) int {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != events.MetricSessionStatus {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				return len(s.DataPoints)
			}
		}
	}
	return 0
}

// A committed status change is counted once, keyed by the status entered. The
// column moves in AppendInTx but commits with the caller, so AppendWith — the
// self-committing wrapper the recovery path and the test harness use — records
// after its own commit.
func TestAppendWithRecordsSessionStatusTransition(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	running := domain.SessionRunning
	if _, err := log.AppendWith(ctx, sid, []events.NewEvent{
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{SetStatus: &running}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if got := statusCount(collect(), "running"); got != 1 {
		t.Errorf("running transitions = %d, want 1", got)
	}
}

// An append that changes no status counts nothing: the metric measures
// transitions, not writes to the log.
func TestAppendWithoutStatusChangeRecordsNoTransition(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	if _, err := log.AppendWith(ctx, sid, []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: []byte(`{"content":[{"type":"text","text":"hi"}]}`)},
	}, events.AppendOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if got := statusPointCount(collect()); got != 0 {
		t.Errorf("recorded %d status transition point(s) for an append with no status change, want 0", got)
	}
}

// Recording is gated on the append succeeding: a rejected append — here on an
// archived session, refused before the status write is even reached — counts no
// transition. The full post-commit guarantee also covers a commit that fails
// after a successful AppendInTx, which needs fault injection to exercise and is
// left to reading; this pins the reachable half, that a failed append never
// counts.
func TestFailedAppendRecordsNoStatusTransition(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sid.String()); err != nil {
		t.Fatalf("archive session: %v", err)
	}
	collect := collectMetrics(t)

	running := domain.SessionRunning
	_, err := log.AppendWith(ctx, sid, []events.NewEvent{
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{SetStatus: &running})
	if !errors.Is(err, events.ErrSessionArchived) {
		t.Fatalf("append on archived session: err = %v, want ErrSessionArchived", err)
	}

	if got := statusPointCount(collect()); got != 0 {
		t.Errorf("recorded %d transition(s) for an append that never committed, want 0", got)
	}
}
