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

// A denial clears the gate too, and records the wait the same way — though
// the send now writes the denial's result itself, answering the call on the
// log before the settlement looks at the gate.
func TestApprovalWaitRecordedOnDenial(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sessionID, askID := suspendViaBrain(t, s)

	sendEvents(t, s, sessionID, confirm(askID, "deny", nil))

	if pts := apiFloatPoints(t, collect(), events.MetricApprovalWait); len(pts) != 1 || pts[0].Count != 1 {
		t.Fatalf("%s points = %v, want one reading", events.MetricApprovalWait, pts)
	}
}

// A confirmation posted with an interrupt of its thread: receipt order says
// who cleared the gate. Received first, the confirmation cleared it — the
// human answered before stopping the session — so the wait is recorded, deny
// or allow. Received after, the interrupt had already cleared it, so the
// confirmation ends no wait and nothing is recorded.
func TestApprovalWaitBesideAnInterruptFollowsReceiptOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       string
		confirmFirst bool
		want         int
	}{
		{"deny then interrupt", "deny", true, 1},
		{"allow then interrupt", "allow", true, 1},
		{"interrupt then deny", "deny", false, 0},
		{"interrupt then allow", "allow", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collect := collectMetrics(t)
			s := newTestServer(t)
			sessionID, askID := suspendViaBrain(t, s)

			posted := []map[string]any{{"type": "user.interrupt"}, confirm(askID, tc.result, nil)}
			if tc.confirmFirst {
				posted[0], posted[1] = posted[1], posted[0]
			}
			sendEvents(t, s, sessionID, posted...)

			n := 0
			for _, pt := range apiFloatPoints(t, collect(), events.MetricApprovalWait) {
				n += int(pt.Count)
			}
			if n != tc.want {
				t.Errorf("%s readings = %d, want %d", events.MetricApprovalWait, n, tc.want)
			}
		})
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

	// The discrimination below needs both rows in place: the backdated
	// thread suspension and a fresh session-level idle carrying the same
	// requires_action (what a sibling's later fold move would leave). If the
	// suspension ever stops writing the session-level row, this test would
	// otherwise pass while exercising only the thread arm of the query.
	var fresh int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'session.status_idle'
		  AND payload->'stop_reason'->>'type' = 'requires_action'
		  AND created_at > now() - interval '1 minute'`, sessionID).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh == 0 {
		t.Fatal("no fresh session-level requires_action idle beside the backdated thread event")
	}

	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))

	pts := apiFloatPoints(t, collect(), events.MetricApprovalWait)
	if len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1", events.MetricApprovalWait, len(pts))
	}
	if pts[0].Count != 1 || pts[0].Sum < 60 {
		t.Errorf("approval wait = count %d / %vs, want one reading measuring the ten-minute-old thread suspension, not the fresh session idle", pts[0].Count, pts[0].Sum)
	}
}
