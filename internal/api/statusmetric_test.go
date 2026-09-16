package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetrics routes the process's meter through a manual reader for one
// test, so the handler's in-process metric recording can be read back.
func collectMetrics(t *testing.T) func() metricdata.ResourceMetrics {
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

func apiStatusCount(t *testing.T, rm metricdata.ResourceMetrics, status string) int64 {
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

// A user.message waking an idle session flips it to running through the API's own
// commit — the production path the brain harness never exercises. This is where
// most idle→running transitions really happen, so it must be counted here.
func TestUserMessageRecordsRunningTransition(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	sendEvents(t, s, sid, userMessage("hello"))

	if got := apiStatusCount(t, collect(), "running"); got != 1 {
		t.Errorf("running transitions = %d, want 1 (the user.message woke the session)", got)
	}
}

// statusCounts reads session.status.transitions for every status a session can
// enter, so a test can pin the one move it expects and the absence of any other.
func statusCounts(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, st := range []string{"idle", "rescheduling", "running", "terminated"} {
		out[st] = apiStatusCount(t, rm, st)
	}
	return out
}

// wantStatusDeltas compares two statusCounts readings against the moves
// expected between them; a status absent from want must not have moved, and
// want may name only a status statusCounts reads.
func wantStatusDeltas(t *testing.T, before, after map[string]int64, want map[string]int64) {
	t.Helper()
	for st := range want {
		if _, ok := after[st]; !ok {
			t.Errorf("want names %q, which statusCounts does not read", st)
		}
	}
	for st, n := range after {
		if got := n - before[st]; got != want[st] {
			t.Errorf("%s transitions = %d, want %d", st, got, want[st])
		}
	}
}

// statusEvents counts each session.status_* event type on a session's log.
func statusEvents(t *testing.T, s *tserver, sid string) map[string]int {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events?limit=1000", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d %v", status, res)
	}
	n := map[string]int{}
	for _, ev := range listData(t, res) {
		if typ, _ := ev["type"].(string); strings.HasPrefix(typ, "session.status_") {
			n[typ]++
		}
	}
	return n
}

// wantStatusEvents is wantStatusDeltas for two statusEvents readings.
func wantStatusEvents(t *testing.T, before, after, want map[string]int) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range []map[string]int{after, want} {
		for typ := range m {
			if seen[typ] {
				continue
			}
			seen[typ] = true
			if got := after[typ] - before[typ]; got != want[typ] {
				t.Errorf("%s events = %d, want %d", typ, got, want[typ])
			}
		}
	}
}

// sessionColumn reads sessions.status, the column the metric's moves are made to.
func sessionColumn(t *testing.T, s *tserver, sid string) string {
	t.Helper()
	var column string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM sessions WHERE id = $1`, sid).Scan(&column); err != nil {
		t.Fatal(err)
	}
	return column
}

// An archive counts each status move its children's endings make, as the log
// records them, and nothing else (#731). A session archive counts nothing for an
// idle child under an idle session; one idle move when the last of two retrying
// children goes; and, for a session stored terminated, an idle move and a
// terminated one back — which a net of the status before and after would count
// as nothing. A thread archive counts the move its one ending makes: ending one
// of two idle children of a session stored terminated folds it idle. Nothing
// leaves a thread at rescheduling or stores a session terminated today, so the
// rows are forged.
func TestArchivesCountEachStatusMoveTheirEndingsMake(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	archive := func(t *testing.T, sid, path string, want map[string]int64, wantEvents map[string]int, wantColumn string) {
		t.Helper()
		before, eventsBefore := statusCounts(t, collect()), statusEvents(t, s, sid)
		if status, res := s.do(http.MethodPost, path, nil); status != http.StatusOK {
			t.Fatalf("archive %s: %d %v", path, status, res)
		}
		wantStatusDeltas(t, before, statusCounts(t, collect()), want)
		wantStatusEvents(t, eventsBefore, statusEvents(t, s, sid), wantEvents)
		if column := sessionColumn(t, s, sid); column != wantColumn {
			t.Errorf("sessions.status = %q, want %q", column, wantColumn)
		}
	}
	newSession := func(t *testing.T) string {
		t.Helper()
		return createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	}

	t.Run("an idle session with an idle child moves nothing", func(t *testing.T) {
		sid := newSession(t)
		insertChild(t, s, sid, "idle")
		archive(t, sid, "/v1/sessions/"+sid+"/archive", map[string]int64{}, map[string]int{}, "idle")
	})
	t.Run("the last retrying child folds the session idle once", func(t *testing.T) {
		sid := newSession(t)
		insertChild(t, s, sid, "rescheduling")
		insertChild(t, s, sid, "rescheduling")
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE sessions SET status = 'rescheduling' WHERE id = $1`, sid); err != nil {
			t.Fatal(err)
		}
		archive(t, sid, "/v1/sessions/"+sid+"/archive",
			map[string]int64{"idle": 1}, map[string]int{"session.status_idle": 1}, "idle")
	})
	t.Run("a terminated session's endings move it idle and back, each counted", func(t *testing.T) {
		sid := newSession(t)
		insertChild(t, s, sid, "idle")
		insertChild(t, s, sid, "idle")
		pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "terminated")
		archive(t, sid, "/v1/sessions/"+sid+"/archive", map[string]int64{"idle": 1, "terminated": 1},
			map[string]int{"session.status_idle": 1, "session.status_terminated": 1}, "terminated")
	})
	t.Run("a thread archive counts the move its ending makes", func(t *testing.T) {
		sid := newSession(t)
		child := insertChild(t, s, sid, "idle")
		insertChild(t, s, sid, "idle")
		pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "terminated")
		archive(t, sid, "/v1/sessions/"+sid+"/threads/"+child+"/archive",
			map[string]int64{"idle": 1}, map[string]int{"session.status_idle": 1}, "idle")
	})
}
