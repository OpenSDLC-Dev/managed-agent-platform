package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// scheduledBody is deploymentBody plus a schedule, the shape every scheduler
// test starts from.
func scheduledBody(agentID, envID, expr, tz string) map[string]any {
	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": expr, "timezone": tz}
	return body
}

// setResumedAt rewinds schedule_resumed_at by direct SQL — the column is
// stamped from the database clock at create, so a test that drives fixed
// instants has to move the floor under them.
func setResumedAt(t *testing.T, s *tserver, deplID string, at time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE deployments SET schedule_resumed_at = $1 WHERE id = $2`, at, deplID); err != nil {
		t.Fatal(err)
	}
}

type schedRun struct {
	id          string
	scheduledAt time.Time
	sessionID   *string
	errType     *string
	succeededAt *time.Time
	createdAt   time.Time
}

// scheduledRuns reads a deployment's scheduled runs, oldest occurrence first.
func scheduledRuns(t *testing.T, s *tserver, deplID string) []schedRun {
	t.Helper()
	rows, err := s.pool.Query(t.Context(),
		`SELECT id, scheduled_at, session_id, error_type, succeeded_at, created_at
		   FROM deployment_runs
		  WHERE deployment_id = $1 AND trigger_type = 'schedule'
		  ORDER BY scheduled_at`, deplID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []schedRun
	for rows.Next() {
		var r schedRun
		if err := rows.Scan(&r.id, &r.scheduledAt, &r.sessionID, &r.errType, &r.succeededAt, &r.createdAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fireCount reads the deployment.fires counter for one outcome (and, when
// non-empty, one error.type).
func fireCount(t *testing.T, rm metricdata.ResourceMetrics, outcome, errType string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricDeploymentFires {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricDeploymentFires, m.Data)
			}
			for _, dp := range s.DataPoints {
				got := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					got[string(kv.Key)] = kv.Value.Emit()
				}
				if got["outcome"] == outcome && (errType == "" || got["error.type"] == errType) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

func skippedCount(t *testing.T, rm metricdata.ResourceMetrics) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricDeploymentOccurrencesSkipped {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricDeploymentOccurrencesSkipped, m.Data)
			}
			var total int64
			for _, dp := range s.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// The happy path: a tick fires the single most recent due occurrence, the
// collapse of the older backlog is counted, the watermark stops a re-fire,
// and the session is the schedule's — deployment-linked and unattributed
// (plan §9's NULL created_by is the schedule's own; a manual run's caller is
// authenticated, a ticker is nobody).
func TestSchedulerFiresTheMostRecentDueOccurrence(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))

	// Three occurrences are due (03-10, 03-11, 03-12, each 09:00); only the
	// most recent is inside the hour window, and it alone fires.
	now := time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)
	if err := api.SchedulerTick(t.Context(), s.pool, now); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 {
		t.Fatalf("got %d scheduled runs, want 1 (the most recent due occurrence only)", len(runs))
	}
	run := runs[0]
	if want := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC); !run.scheduledAt.Equal(want) {
		t.Errorf("scheduled_at = %s, want the exact cron instant %s", run.scheduledAt, want)
	}
	if run.sessionID == nil || run.succeededAt == nil || run.errType != nil {
		t.Fatalf("run settled as session=%v succeeded=%v err=%v, want the success arm", run.sessionID, run.succeededAt, run.errType)
	}

	// The fired session: linked, running (initial events start the loop),
	// and unattributed.
	var (
		sessDeplID, status *string
		createdBy          *string
	)
	if err := s.pool.QueryRow(t.Context(),
		`SELECT deployment_id, status, created_by FROM sessions WHERE id = $1`, *run.sessionID).
		Scan(&sessDeplID, &status, &createdBy); err != nil {
		t.Fatal(err)
	}
	if sessDeplID == nil || *sessDeplID != deplID {
		t.Errorf("session.deployment_id = %v, want %s", sessDeplID, deplID)
	}
	if status == nil || *status != "running" {
		t.Errorf("session.status = %v, want running", status)
	}
	if createdBy != nil {
		t.Errorf("session.created_by = %q, want NULL — the ticker has no principal", *createdBy)
	}

	// last_run_at is the run's created_at, live on the wire already.
	code, d := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, d)
	}
	lastRaw, _ := d["schedule"].(map[string]any)["last_run_at"].(string)
	last, err := time.Parse(time.RFC3339, lastRaw)
	if err != nil || !last.Equal(run.createdAt) {
		t.Errorf("last_run_at = %q (%v), want the run's created_at %s", lastRaw, err, run.createdAt)
	}

	// A later tick with no newer occurrence finds the watermark and fires
	// nothing.
	if err := api.SchedulerTick(t.Context(), s.pool, now.Add(30*time.Second)); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if runs := scheduledRuns(t, s, deplID); len(runs) != 1 {
		t.Fatalf("second tick grew the run list to %d; the watermark must stop a re-fire", len(runs))
	}

	rm := collect()
	if got := fireCount(t, rm, "created", ""); got != 1 {
		t.Errorf("fires{outcome=created} = %d, want 1", got)
	}
	if got := skippedCount(t, rm); got != 2 {
		t.Errorf("occurrences.skipped = %d, want 2 (03-10 and 03-11 collapsed)", got)
	}
}

// The catch-up window is what bounds a backlog: an occurrence older than it
// never fires, one inside it does.
func TestSchedulerCatchupWindowBoundsTheFire(t *testing.T) {
	restore := api.SetDeploymentCatchupWindowForTest(10 * time.Minute)
	defer restore()
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))

	// 09:00 fell due 30 minutes ago against a 10-minute window: aged out,
	// never selected.
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("an occurrence outside the window fired: %v", runs)
	}

	// The same occurrence 5 minutes after its instant is inside the window.
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
}

// Paused and archived deployments are not candidates, and an unpause resumes
// from the next occurrence: the one that fell due during the pause is behind
// the resume floor and never backfilled.
func TestSchedulerHonorsPauseArchiveAndTheResumeFloor(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))

	if code, res := s.do(http.MethodPost, "/v1/deployments/"+deplID+"/pause", nil); code != http.StatusOK {
		t.Fatalf("pause: %d %v", code, res)
	}
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("a paused deployment fired: %v", runs)
	}

	// Unpause stamps the resume floor from the database clock; pin it to a
	// fixed instant after the missed occurrence so the tick's math is
	// deterministic.
	if code, res := s.do(http.MethodPost, "/v1/deployments/"+deplID+"/unpause", nil); code != http.StatusOK {
		t.Fatalf("unpause: %d %v", code, res)
	}
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 10, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("the occurrence missed during the pause was backfilled: %v", runs)
	}
	// The next occurrence after the resume floor fires normally.
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 13, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || !runs[0].scheduledAt.Equal(time.Date(2026, 3, 13, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("runs = %v, want exactly the 03-13 09:00 occurrence", runs)
	}

	// An archived deployment is no candidate at all.
	archID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, archID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	if code, res := s.do(http.MethodPost, "/v1/deployments/"+archID+"/archive", nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, res)
	}
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, archID); len(runs) != 0 {
		t.Fatalf("an archived deployment fired: %v", runs)
	}
}

// Replicas do not share a tick: each drives its own instant. What keeps the
// fleet honest is that scheduled_at derives from the cron expression, never
// from the observing clock — three ticks at three instants, one run, and its
// scheduled_at is the exact cron instant none of the three clocks read.
func TestSchedulerClaimsAreExactlyOnceAcrossReplicas(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 12 * * *", "UTC"))["id"].(string)
	occurrence := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	setResumedAt(t, s, deplID, occurrence.Add(-time.Hour))

	for _, at := range []time.Time{
		occurrence.Add(-time.Second),     // not yet due
		occurrence.Add(2 * time.Second),  // fires
		occurrence.Add(59 * time.Second), // watermark holds
	} {
		if err := api.SchedulerTick(t.Context(), s.pool, at); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs across three replica ticks, want exactly 1", len(runs))
	}
	if !runs[0].scheduledAt.Equal(occurrence) {
		t.Errorf("scheduled_at = %s, want the cron instant %s — never the observing clock's",
			runs[0].scheduledAt, occurrence)
	}
}

// A caller whose tick begins before the winner's transaction commits blocks
// on the uncommitted claim, times out, and reads that as a lost claim — a
// clean nil, never a unique violation escaping as an error.
func TestSchedulerConcurrentClaimLosesCleanly(t *testing.T) {
	restoreWait := api.SetDeploymentLockWaitForTest(300 * time.Millisecond)
	defer restoreWait()
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 12 * * *", "UTC"))["id"].(string)
	occurrence := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	setResumedAt(t, s, deplID, occurrence.Add(-time.Hour))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once bool
	restoreHook := api.SetDeploymentFireHookInFireForTest(func() error {
		// Only the first caller to win the claim reaches the savepoint; hold
		// it there so the second caller's whole fire runs against an
		// uncommitted claim.
		if !once {
			once = true
			close(entered)
			<-release
		}
		return nil
	})
	defer restoreHook()

	winnerErr := make(chan error, 1)
	go func() {
		winnerErr <- api.SchedulerTick(context.Background(), s.pool, occurrence.Add(2*time.Second))
	}()
	<-entered
	if err := api.SchedulerTick(t.Context(), s.pool, occurrence.Add(3*time.Second)); err != nil {
		t.Fatalf("the losing tick returned %v, want nil — a lost claim is not an error", err)
	}
	close(release)
	if err := <-winnerErr; err != nil {
		t.Fatalf("the winning tick returned %v", err)
	}

	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || runs[0].sessionID == nil {
		t.Fatalf("runs = %+v, want one settled success", runs)
	}
}

// A classified failure settles the run on its error arm and auto-pauses the
// deployment in the same commit — while last_run_at stays null, because the
// run never "actually started" a session (§8.1 entry 13's inferred half),
// and the savepoint rollback leaves no half-made session behind.
func TestSchedulerFireFailureSettlesAndPauses(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	if code, res := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); code != http.StatusOK {
		t.Fatalf("archive environment: %d %v", code, res)
	}

	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1 — the claim is the committed row", len(runs))
	}
	run := runs[0]
	if run.sessionID != nil || run.succeededAt != nil {
		t.Errorf("a failed fire settled its success columns: session=%v succeeded=%v", run.sessionID, run.succeededAt)
	}
	if run.errType == nil || *run.errType != "environment_archived_error" {
		t.Fatalf("error_type = %v, want environment_archived_error", run.errType)
	}

	code, d := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, d)
	}
	if d["status"] != "paused" {
		t.Errorf("status = %v, want paused — a scheduled fire's classified failure auto-pauses", d["status"])
	}
	reason, _ := d["paused_reason"].(map[string]any)
	if reason["type"] != "error" {
		t.Errorf("paused_reason.type = %v, want error", reason["type"])
	}
	if inner, ok := reason["error"].(map[string]any); !ok || inner["type"] != "environment_archived_error" {
		t.Errorf("paused_reason.error = %v, want the run's own type", reason["error"])
	}
	if last := d["schedule"].(map[string]any)["last_run_at"]; last != nil {
		t.Errorf("last_run_at = %v, want null — the failed fire never started a session", last)
	}

	var sessions int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE deployment_id = $1`, deplID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived the savepoint rollback, want 0", sessions)
	}

	rm := collect()
	if got := fireCount(t, rm, "failed", "environment_archived_error"); got != 1 {
		t.Errorf("fires{outcome=failed,error.type=environment_archived_error} = %d, want 1", got)
	}
}

// An unclassified failure rolls the whole transaction back: no run row, no
// pause, nothing in any list — the claim is released and the next tick fires
// the same occurrence. The pair with the classified case above is what pins
// the savepoint boundary: one rollback keeps the claim, the other releases it.
func TestSchedulerUnclassifiedFailureLeavesNoTrace(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))

	restore := api.SetDeploymentFireHookInFireForTest(func() error {
		return errors.New("the database blipped")
	})
	now := time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)
	if err := api.SchedulerTick(t.Context(), s.pool, now); err == nil {
		t.Fatal("tick returned nil; an abandoned fire must surface as the tick's error")
	}
	restore()

	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("an abandoned fire left a run row: %v", runs)
	}
	if code, d := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil); code != http.StatusOK || d["status"] != "active" {
		t.Fatalf("deployment = %d %v, want active — unknown_error is deliberately never recorded", code, d["status"])
	}

	// The occurrence is still the most recent due one, so the next tick
	// retries it — the recovery the rollback bought.
	if err := api.SchedulerTick(t.Context(), s.pool, now.Add(30*time.Second)); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || runs[0].sessionID == nil {
		t.Fatalf("runs = %+v, want the retried occurrence settled as a success", runs)
	}

	rm := collect()
	if got := fireCount(t, rm, "abandoned", ""); got != 1 {
		t.Errorf("fires{outcome=abandoned} = %d, want 1", got)
	}
	if got := fireCount(t, rm, "created", ""); got != 1 {
		t.Errorf("fires{outcome=created} = %d, want 1 (the retry)", got)
	}
}

// The candidate scan and the fire are separate statements: an archive
// committed between them must stop the fire at the transaction's re-read —
// archive is terminal, and a session created after its 200 would break that.
func TestSchedulerArchiveBetweenScanAndFire(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))

	restore := api.SetDeploymentFireHookAfterBeginForTest(func() {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE deployments SET archived_at = now() WHERE id = $1`, deplID); err != nil {
			t.Errorf("archive in the window: %v", err)
		}
	})
	defer restore()

	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("a fire outran the archive: %v", runs)
	}
	var sessions int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE deployment_id = $1`, deplID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions exist for a deployment archived before its fire, want 0", sessions)
	}
}

// DST, both directions the arithmetic can go wrong. On a fall-back day a
// wall clock repeats: both instants are real occurrences with different
// scheduled_at values — which also exercises the timestamptz column, though
// only the catalog check in internal/store pins the DDL. Lord Howe's
// half-hour offset catches arithmetic that assumes whole hours.
func TestSchedulerDST(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// America/New_York falls back 2026-11-01: 01:30 EDT is 05:30Z, and the
	// repeated 01:30 EST an hour later is 06:30Z.
	nyID := createDeployment(t, s, scheduledBody(agentID, envID, "30 1 * * *", "America/New_York"))["id"].(string)
	setResumedAt(t, s, nyID, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	for _, at := range []time.Time{first.Add(30 * time.Second), second.Add(30 * time.Second)} {
		if err := api.SchedulerTick(t.Context(), s.pool, at); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
	}
	runs := scheduledRuns(t, s, nyID)
	if len(runs) != 2 {
		t.Fatalf("got %d runs on the fall-back day, want 2 — the repeated wall clock is two occurrences", len(runs))
	}
	if !runs[0].scheduledAt.Equal(first) || !runs[1].scheduledAt.Equal(second) {
		t.Errorf("scheduled_at = %s, %s; want %s, %s", runs[0].scheduledAt, runs[1].scheduledAt, first, second)
	}

	// Lord Howe summer time is +11:00 (its DST shift is itself half an
	// hour): 08:00 on 2026-01-15 is 21:00Z the previous day.
	lhID := createDeployment(t, s, scheduledBody(agentID, envID, "0 8 * * *", "Australia/Lord_Howe"))["id"].(string)
	occurrence := time.Date(2026, 1, 14, 21, 0, 0, 0, time.UTC)
	setResumedAt(t, s, lhID, occurrence.Add(-time.Hour))
	if err := api.SchedulerTick(t.Context(), s.pool, occurrence.Add(30*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	lhRuns := scheduledRuns(t, s, lhID)
	if len(lhRuns) != 1 || !lhRuns[0].scheduledAt.Equal(occurrence) {
		t.Fatalf("Lord Howe runs = %v, want one at exactly %s", lhRuns, occurrence)
	}
}

// The one wall-clock test: the production loop — ticker, database clock,
// candidate scan — actually fires. Everything else drives a fixed now.
func TestSchedulerTickerRuns(t *testing.T) {
	restore := api.SetDeploymentTickIntervalForTest(20 * time.Millisecond)
	defer restore()
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "* * * * *", "UTC"))["id"].(string)
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE deployments SET schedule_resumed_at = now() - interval '2 minutes' WHERE id = $1`, deplID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); api.StartDeploymentScheduler(ctx, s.pool, nil, nil) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if runs := scheduledRuns(t, s, deplID); len(runs) >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the ticker never fired the due occurrence")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The pausing mapping is asserted on a property that can fail — every type
// the Go map would write is admitted by the migration's CHECK — rather than
// against a literal copy of itself. The reachable-path property is the arm
// tests': each classified arm asserts the recorded type. The two run-error
// types the pausing union omits must not be in the map: a pause carrying
// either is unrepresentable on the wire.
func TestSchedulerPausingTypesMatchTheMigrationCheck(t *testing.T) {
	s := newTestServer(t)
	var def string
	if err := s.pool.QueryRow(t.Context(), `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'deployments_paused_error_type'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	types := api.DeploymentPausingErrorTypesForTest()
	if len(types) != 14 {
		t.Fatalf("the pausing map holds %d types, want the union's 14", len(types))
	}
	for _, typ := range types {
		if !strings.Contains(def, "'"+typ+"'") {
			t.Errorf("map type %q is not admitted by the CHECK: %s", typ, def)
		}
	}
	for _, omitted := range []string{"session_rate_limited_error", "session_creation_rejected_error"} {
		for _, typ := range types {
			if typ == omitted {
				t.Errorf("%s is in the pausing map; the paused-reason union omits it", omitted)
			}
		}
	}
}

// The auto-pause deadlock, resolved as a lost race: a loser blocked on the
// winner's uncommitted claim still holds FOR SHARE on the deployment row, so
// a winner whose classified failure reaches the auto-pause UPDATE waits on
// that share lock — a cycle Postgres breaks with 40P01 against either side.
// Both ticks must return nil, and the occurrence must still settle: whichever
// side survives (or the next entrant, when the winner was the victim)
// records the error run and pauses the deployment.
func TestSchedulerAutoPauseDeadlockResolvesCleanly(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 12 * * *", "UTC"))["id"].(string)
	occurrence := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	setResumedAt(t, s, deplID, occurrence.Add(-time.Hour))
	if code, res := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); code != http.StatusOK {
		t.Fatalf("archive environment: %d %v", code, res)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var held bool
	restoreHook := api.SetDeploymentFireHookInFireForTest(func() error {
		if !held {
			held = true
			close(entered)
			<-release
		}
		return nil
	})
	defer restoreHook()

	winnerErr := make(chan error, 1)
	go func() {
		winnerErr <- api.SchedulerTick(context.Background(), s.pool, occurrence.Add(2*time.Second))
	}()
	<-entered

	loserErr := make(chan error, 1)
	go func() {
		loserErr <- api.SchedulerTick(context.Background(), s.pool, occurrence.Add(3*time.Second))
	}()
	// Release the winner only once the loser is genuinely blocked on the
	// claim — the deadlock needs both waiters in place.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND query LIKE '%INSERT INTO deployment_runs%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second caller never blocked on the claim")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	if err := <-winnerErr; err != nil {
		t.Errorf("the winning tick returned %v, want nil — a deadlock victim is a lost race", err)
	}
	if err := <-loserErr; err != nil {
		t.Errorf("the losing tick returned %v, want nil — a deadlock victim is a lost race", err)
	}

	// Whichever side survived, the occurrence settles: run a follow-up tick
	// for the case where both transactions were torn down before settling.
	if err := api.SchedulerTick(t.Context(), s.pool, occurrence.Add(30*time.Second)); err != nil {
		t.Fatalf("follow-up tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want exactly 1 — the deadlock must not duplicate or drop the occurrence", len(runs))
	}
	if runs[0].errType == nil || *runs[0].errType != "environment_archived_error" {
		t.Errorf("error_type = %v, want environment_archived_error", runs[0].errType)
	}
	if code, d := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil); code != http.StatusOK || d["status"] != "paused" {
		t.Errorf("deployment = %d %v, want paused", code, d["status"])
	}
}

// The fire's re-read covers the scheduling state, not only archive/pause: an
// occurrence computed from a schedule that an update has since replaced, or
// one behind a resume floor a pause/unpause round-trip restamped, is stale —
// "missed triggers are not backfilled" — and the fire lets it go.
func TestSchedulerSchedulingStateChangeBetweenScanAndFire(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// A replaced expression: the tick computed 09:00 from the old schedule;
	// the fire finds a different one and stands down.
	exprID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, exprID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	restore := api.SetDeploymentFireHookAfterBeginForTest(func() {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE deployments SET schedule_expression = '30 4 * * *' WHERE id = $1`, exprID); err != nil {
			t.Errorf("swap expression: %v", err)
		}
	})
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	restore()
	if runs := scheduledRuns(t, s, exprID); len(runs) != 0 {
		t.Fatalf("a fire ran an occurrence of a replaced schedule: %v", runs)
	}

	// A restamped resume floor: the occurrence fell behind it in the window
	// between the scan and the fire.
	floorID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, floorID, time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC))
	restore = api.SetDeploymentFireHookAfterBeginForTest(func() {
		// t.Errorf, not the t.Fatal-ing helper: the hook runs on a fire
		// goroutine, where Fatal's Goexit is documented misuse.
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE deployments SET schedule_resumed_at = $1 WHERE id = $2`,
			time.Date(2026, 3, 13, 9, 30, 0, 0, time.UTC), floorID); err != nil {
			t.Errorf("restamp resume floor: %v", err)
		}
	})
	defer restore()
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 13, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, floorID); len(runs) != 0 {
		t.Fatalf("a fire ran an occurrence behind the restamped resume floor: %v", runs)
	}
	restore()

	// A replaced timezone alone — the same wall-clock expression names a
	// different instant under it, so the occurrence is equally stale.
	tzID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, tzID, time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC))
	restore = api.SetDeploymentFireHookAfterBeginForTest(func() {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE deployments SET schedule_timezone = 'America/New_York' WHERE id = $1`, tzID); err != nil {
			t.Errorf("swap timezone: %v", err)
		}
	})
	defer restore()
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 14, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, tzID); len(runs) != 0 {
		t.Fatalf("a fire ran an occurrence of a replaced timezone: %v", runs)
	}
}

// The two lost-race codes part ways past the claim phase, and each half is
// pinned: a synthetic 40P01 from inside the fire is the quiet retry a
// deadlock victim earns (its transaction committed nothing), while a 55P03
// there is a diagnosable contention fault and must surface as a counted
// abandonment — swallowing it would make a wedged environment row
// indistinguishable from "nothing was due".
func TestSchedulerLostRaceShapesPastTheClaim(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)

	restore := api.SetDeploymentFireHookInFireForTest(func() error {
		return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	})
	if err := api.SchedulerTick(t.Context(), s.pool, now); err != nil {
		t.Errorf("a deadlock victim surfaced as the tick's error: %v", err)
	}
	restore()
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("a deadlock victim left a run row: %v", runs)
	}

	restore = api.SetDeploymentFireHookInFireForTest(func() error {
		return &pgconn.PgError{Code: "55P03", Message: "lock timeout"}
	})
	if err := api.SchedulerTick(t.Context(), s.pool, now.Add(30*time.Second)); err == nil {
		t.Error("a deep lock timeout returned nil; it must surface as a counted abandonment")
	}
	restore()
	if runs := scheduledRuns(t, s, deplID); len(runs) != 0 {
		t.Fatalf("an abandoned fire left a run row: %v", runs)
	}
	if got := fireCount(t, collect(), "abandoned", ""); got != 1 {
		t.Errorf("fires{outcome=abandoned} = %d, want 1 (the 55P03, not the 40P01)", got)
	}
}

// The uncapped skipped branch — a floor already inside the window — computes
// the exact count from the due list, no saturating walk involved.
func TestSchedulerSteadyStateSkippedCountIsExact(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "*/10 * * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC))

	// The floor (09:00) is 35 minutes old against a one-hour window, so the
	// clamp never engages; 09:10 and 09:20 collapse under the 09:30 fire.
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 35, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || !runs[0].scheduledAt.Equal(time.Date(2026, 3, 12, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("runs = %v, want exactly the 09:30 occurrence", runs)
	}
	if got := skippedCount(t, collect()); got != 2 {
		t.Errorf("occurrences.skipped = %d, want the exact 2", got)
	}
}

// An idle sweep exports no span at all — 2,880 empty root traces a day per
// replica would bury the fires an operator looks for — while a firing sweep
// roots deployment.tick with its deployment.fire child.
func TestSchedulerIdleTickExportsNoSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)

	// Nothing is due: the resume floor (stamped at create, "now") is not
	// before this past instant, so the candidate contributes no fire.
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("idle tick: %v", err)
	}
	names := map[string]int{}
	for _, sp := range recorder.Ended() {
		names[sp.Name()]++
	}
	if names["deployment.tick"] != 0 || names["deployment.fire"] != 0 {
		t.Fatalf("an idle sweep exported scheduler spans: %v", names)
	}

	setResumedAt(t, s, deplID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("firing tick: %v", err)
	}
	names = map[string]int{}
	for _, sp := range recorder.Ended() {
		names[sp.Name()]++
	}
	if names["deployment.tick"] != 1 || names["deployment.fire"] != 1 {
		t.Fatalf("a firing sweep exported %v, want one deployment.tick and one deployment.fire", names)
	}
}

// One poisoned schedule costs its own deployment its fires, never the
// fleet's: a stored timezone the walk cannot load (a tzdata bump, a
// hand-written migration — create and update refuse it, so only SQL can
// plant it) surfaces as the tick's error while every healthy deployment
// still fires.
func TestSchedulerOnePoisonedScheduleDoesNotStopTheSweep(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	healthyID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, healthyID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	poisonedID := createDeployment(t, s, scheduledBody(agentID, envID, "0 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, poisonedID, time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC))
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE deployments SET schedule_timezone = 'Not/AZone' WHERE id = $1`, poisonedID); err != nil {
		t.Fatal(err)
	}

	err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC))
	if err == nil {
		t.Error("tick returned nil; the poisoned row must surface, not vanish")
	}
	if runs := scheduledRuns(t, s, healthyID); len(runs) != 1 {
		t.Fatalf("the healthy deployment got %d runs, want 1 — a poisoned sibling must not stop the sweep", len(runs))
	}
	if runs := scheduledRuns(t, s, poisonedID); len(runs) != 0 {
		t.Fatalf("the poisoned deployment fired: %v", runs)
	}
}

// A cold backlog is bounded twice over: the fire lookup is clamped to the
// catch-up window (a months-old */1 schedule must not walk every minute since
// its creation on every tick), and the skipped count saturates at the scan
// cap rather than enumerating the backlog exactly.
func TestSchedulerColdStartBacklogIsBoundedAndSaturates(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplID := createDeployment(t, s, scheduledBody(agentID, envID, "* * * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, deplID, time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC))

	start := time.Now()
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 30, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("tick over a 45-day backlog took %s; the clamp exists so it never walks the backlog", took)
	}
	runs := scheduledRuns(t, s, deplID)
	if len(runs) != 1 || !runs[0].scheduledAt.Equal(time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("runs = %v, want exactly the most recent occurrence", runs)
	}
	if got := skippedCount(t, collect()); got != int64(api.DeploymentSkipScanCapForTest()) {
		t.Errorf("occurrences.skipped = %d, want the saturated cap %d", got, api.DeploymentSkipScanCapForTest())
	}
}
