package api_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func apiFloatPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
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

// A confirmation that clears the last requires_action gate records how long the
// session waited on the human. The interval spans the suspension the brain wrote
// and the confirmation the API commits, so this is the only place both ends are
// known.
func TestApprovalWaitRecordedOnResume(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sessionID, askID := suspendViaBrain(t, s)

	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))

	pts := apiFloatPoints(t, collect(), events.MetricApprovalWait)
	if len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1", events.MetricApprovalWait, len(pts))
	}
	if pts[0].Count != 1 || pts[0].Sum < 0 {
		t.Errorf("approval wait = count %d / sum %v, want one non-negative reading", pts[0].Count, pts[0].Sum)
	}
}

// A user.message resuming an idle session is not an approval, so it records no
// approval wait — only a confirmation clearing a requires_action gate does.
func TestUserMessageResumeRecordsNoApprovalWait(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	sendEvents(t, s, sid, userMessage("hi"))

	if pts := apiFloatPoints(t, collect(), events.MetricApprovalWait); len(pts) != 0 {
		t.Errorf("recorded %d approval wait point(s) for a non-confirmation resume, want 0", len(pts))
	}
}

// The wait is measured from the thread's own suspension, even when a later
// session-level idle re-advertises the same ask (a sibling moving the fold
// does): the session event counts only for a suspension that predates the
// thread resource entirely.
func TestApprovalWaitMeasuresFromTheThreadSuspension(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sessionID, askID := suspendViaBrain(t, s)
	// The suspension is ten minutes old; the session-level idle beside it —
	// stand-in for a sibling's later fold move carrying the same ask — stays
	// fresh. Measuring from the fresh one would report a near-zero wait.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE events SET created_at = created_at - interval '10 minutes'
		 WHERE session_id = $1 AND type = 'session.thread_status_idle'`, sessionID); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))

	pts := apiFloatPoints(t, collect(), events.MetricApprovalWait)
	if len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1", events.MetricApprovalWait, len(pts))
	}
	if pts[0].Sum < 60 {
		t.Errorf("approval wait = %vs, want the ten-minute-old thread suspension measured, not the fresh session idle", pts[0].Sum)
	}
}
