package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The session metrics the dream runner's own paths owe (plan 41 slice 2).
// interruptSessionInTx and createSessionInTx both leave their post-commit
// observations to whoever commits, and for a dream that is the cancel handler
// and the tick — so every case here would have counted nothing before the
// runner started recording what those two hand back.
//
// Each case swaps the process meter provider through collectMetrics, which is
// why none of them runs in parallel.

// dreamTransitionCount reads dream.transitions for one target status — the
// count of arms that committed a move there, which is how a test tells one
// completion from two that wrote the same terminal row.
func dreamTransitionCount(t *testing.T, rm metricdata.ResourceMetrics, to string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricDreamTransitions {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricDreamTransitions, m.Data)
			}
			for _, p := range sum.DataPoints {
				if v, ok := p.Attributes.Value("to"); ok && v.AsString() == to {
					total += p.Value
				}
			}
		}
	}
	return total
}

// sessionResourceCount reads back how many resources a session really carries,
// so a case asserts the counter against the session rather than a hand-copied
// number that would drift with the renderer.
func sessionResourceCount(t *testing.T, s *tserver, sessionID string) int {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT resources FROM sessions WHERE id = $1`, sessionID).Scan(&raw); err != nil {
		t.Fatalf("read session resources: %v", err)
	}
	var resources []json.RawMessage
	if err := json.Unmarshal(raw, &resources); err != nil {
		t.Fatalf("decode session resources: %v", err)
	}
	return len(resources)
}

// (a) The start commits a pipeline session born running with the clone and
// every rendered transcript attached, so it owes the two counts every other
// committer of a session records.
func TestDreamStartRecordsSessionCreate(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)

	rm := collect()
	if got := apiStatusCount(t, rm, "running"); got != 1 {
		t.Errorf("running transitions = %d, want 1 (the pipeline session was born running)", got)
	}
	// The output store, plus one file per input transcript and INDEX.md.
	files := len(dreamFileIDs(t, s, dreamID))
	want := sessionResourceCount(t, s, sessionID)
	if want != 1+files {
		t.Fatalf("the pipeline session carries %d resources, want the store plus its %d files", want, files)
	}
	if got := resourceMutationCount(t, rm, "ok"); got != int64(want) {
		t.Errorf("session.resources{ok} = %d, want %d (what the pipeline session attached)", got, want)
	}
}

// (b) A cancel on a running dream interrupts its session in the handler's own
// transaction, and the handler is what commits — so the running→idle is its to
// count. The closing tick that follows finds an idle session and moves nothing,
// so the total stays one.
func TestDreamCancelRecordsSessionIdle(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)
	setSessionStatus(t, s, sessionID, "running")

	if status, res := s.do(http.MethodPost, "/v1/dreams/"+dreamID+"/cancel", nil); status != http.StatusOK {
		t.Fatalf("cancel: status %d (%v)", status, res)
	}
	if got := apiStatusCount(t, collect(), "idle"); got != 1 {
		t.Fatalf("idle transitions = %d, want 1 (the cancel's interrupt idled the session)", got)
	}

	tick(t, s) // the closing arm, on a session the cancel already idled
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Fatal("the closing arm did not finish the canceled dream")
	}
	if got := apiStatusCount(t, collect(), "idle"); got != 1 {
		t.Errorf("idle transitions = %d after the closing tick, want 1 (it moved nothing)", got)
	}
}

// (c) Arm 2: the timeout fails a running dream and interrupts its session
// inside the tick's transaction, which runDreamArm commits.
func TestDreamTimeoutRecordsSessionIdle(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)
	setSessionStatus(t, s, sessionID, "running")

	cfg := dreamCfg()
	cfg.Timeout = time.Nanosecond // the started dream is over budget at any now
	tickAt(t, s, dbNow(t, s), cfg)

	if d := getDream(t, s, dreamID); d["status"] != "failed" {
		t.Fatalf("dream is %v, want failed (arm 2)", d["status"])
	}
	if got := apiStatusCount(t, collect(), "idle"); got != 1 {
		t.Errorf("idle transitions = %d, want 1 (the timeout's interrupt idled the session)", got)
	}
}

// (d) Arm 1: a terminal dream whose session is still running is interrupted
// again, and the tick commits that. The archive on the next tick is not a
// status transition, so it adds nothing.
func TestDreamClosingArmRecordsSessionIdle(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)

	tick(t, s) // arm 10 completes the dream, leaving it terminal
	if d := getDream(t, s, dreamID); d["status"] != "completed" {
		t.Fatalf("dream is %v, want completed (arm 10)", d["status"])
	}
	setSessionStatus(t, s, sessionID, "running")

	tick(t, s) // arm 1, on a session still running
	if got := apiStatusCount(t, collect(), "idle"); got != 1 {
		t.Fatalf("idle transitions = %d, want 1 (the closing arm's interrupt)", got)
	}

	tick(t, s) // arm 1 again, now archiving
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Fatal("the closing arm did not stamp closed_at")
	}
	if got := apiStatusCount(t, collect(), "idle"); got != 1 {
		t.Errorf("idle transitions = %d after the archive, want 1 (an archive is not a status)", got)
	}
}
