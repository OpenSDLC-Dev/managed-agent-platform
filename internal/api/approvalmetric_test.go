package api_test

import (
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
