package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustMarshal(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("re-reading %s: %v", b, err)
	}
	return m
}

func field(t *testing.T, m map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("key %q is absent; the wire schema requires it", key)
	}
	return string(raw)
}

// A deployment with nothing optional set. Its schedule is null, which is what
// "presence enables scheduled execution; null means manual-only" makes the
// difference between a scheduled and a manual-only deployment.
func minimalDeployment() Deployment {
	at := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	return Deployment{
		ID:            "depl_011CZkZcDH3vPqd7xnEfwTai",
		Name:          "Daily order report",
		Agent:         AgentReference{Type: "agent", ID: "agent_011CZkYpogX7uDKUyvBTophP", Version: 1},
		EnvironmentID: "env_011CZkZ9X2dpNyB7HsEFoRfW",
		CreatedAt:     at,
		UpdatedAt:     at,
	}
}

// TestDeploymentEmitsEverySixteenRequiredKey pins the object's required list.
// The schema's `required` is what a client may read without checking presence,
// so a key going missing is a wire break a round-trip test would not catch:
// unmarshalling into a Go struct is happy with an absent key.
func TestDeploymentEmitsEverySixteenRequiredKey(t *testing.T) {
	required := []string{
		"type", "id", "name", "description", "agent", "environment_id",
		"vault_ids", "initial_events", "resources", "metadata", "schedule",
		"status", "paused_reason", "created_at", "updated_at", "archived_at",
	}
	m := mustMarshal(t, minimalDeployment())
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("required key %q is absent", key)
		}
	}
	if len(required) != 16 {
		t.Fatalf("the schema's required list has 16 entries, this test pins %d", len(required))
	}
}

// budget is the one property outside that required list — "Absent when no
// budget is set" — and this platform sets none, ever. Emitting the key at all
// would tell an operator a ceiling exists (plan 37 §8.1 entry 2, #432).
func TestDeploymentNeverEmitsBudget(t *testing.T) {
	m := mustMarshal(t, minimalDeployment())
	if _, ok := m["budget"]; ok {
		t.Errorf("budget was emitted as %s; it must be absent, since no budget is ever set", m["budget"])
	}
}

// The collections are required and typed as arrays, so an unset one is [] and
// never null — the distinction a client indexing into the value depends on.
func TestDeploymentEmptyCollectionsAreArraysNotNull(t *testing.T) {
	m := mustMarshal(t, minimalDeployment())
	for _, key := range []string{"vault_ids", "initial_events", "resources"} {
		if got := field(t, m, key); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
	if got := field(t, m, "metadata"); got != "{}" {
		t.Errorf("metadata = %s, want {}", got)
	}
}

// type is a constant on the wire, and nothing stores it.
func TestDeploymentTypeIsConstant(t *testing.T) {
	m := mustMarshal(t, minimalDeployment())
	if got := field(t, m, "type"); got != `"deployment"` {
		t.Errorf("type = %s, want \"deployment\"", got)
	}
}

// status and paused_reason are computed from the three pause columns, never
// stored — "non-null exactly when status is paused; null otherwise" is an
// invariant a renderer can hold and two independently-written columns cannot.
func TestDeploymentStatusAndPausedReasonAreComputedTogether(t *testing.T) {
	paused := time.Date(2026, 3, 16, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name             string
		mutate           func(*Deployment)
		wantStatus       string
		wantPausedReason string
	}{
		{
			name:             "live",
			mutate:           func(*Deployment) {},
			wantStatus:       `"active"`,
			wantPausedReason: `null`,
		},
		{
			name: "paused by the pause endpoint",
			mutate: func(d *Deployment) {
				d.PausedAt, d.PausedKind = &paused, PausedManual
			},
			wantStatus:       `"paused"`,
			wantPausedReason: `{"type":"manual"}`,
		},
		{
			name: "auto-paused by a failed fire",
			mutate: func(d *Deployment) {
				d.PausedAt, d.PausedKind = &paused, PausedError
				d.PausedErrorType = "vault_archived_error"
			},
			wantStatus:       `"paused"`,
			wantPausedReason: `{"type":"error","error":{"type":"vault_archived_error"}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := minimalDeployment()
			c.mutate(&d)
			m := mustMarshal(t, d)
			if got := field(t, m, "status"); got != c.wantStatus {
				t.Errorf("status = %s, want %s", got, c.wantStatus)
			}
			if got := field(t, m, "paused_reason"); got != c.wantPausedReason {
				t.Errorf("paused_reason = %s, want %s", got, c.wantPausedReason)
			}
		})
	}
}

// "Archived deployments report `active` with `archived_at` set." Archive leaves
// the pause columns untouched, so a deployment archived while paused would
// otherwise report paused forever on a row nothing can fire — and since nothing
// unarchives, nothing could ever observe it returning to active.
func TestArchivedDeploymentReportsActiveWhateverThePauseColumnsSay(t *testing.T) {
	paused := time.Date(2026, 3, 16, 9, 0, 0, 0, time.UTC)
	archived := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)

	d := minimalDeployment()
	d.PausedAt, d.PausedKind = &paused, PausedManual
	d.ArchivedAt = &archived

	m := mustMarshal(t, d)
	if got := field(t, m, "status"); got != `"active"` {
		t.Errorf("status = %s, want \"active\" — archived reports active", got)
	}
	if got := field(t, m, "paused_reason"); got != `null` {
		t.Errorf("paused_reason = %s, want null — non-null exactly when status is paused", got)
	}
	if got := field(t, m, "archived_at"); got == `null` {
		t.Error("archived_at is null on an archived deployment")
	}
}

// The schedule is required and nullable: null is manual-only, and an object
// enables scheduled execution. last_run_at and upcoming_runs_at are the two
// computed members, both optional in the schema — last_run_at is api:"nullable"
// and upcoming_runs_at carries no api: tag at all.
func TestScheduleRendersItsComputedMembers(t *testing.T) {
	d := minimalDeployment()
	m := mustMarshal(t, d)
	if got := field(t, m, "schedule"); got != `null` {
		t.Errorf("an unscheduled deployment's schedule = %s, want null", got)
	}

	last := time.Date(2026, 3, 16, 16, 0, 9, 0, time.UTC)
	d.Schedule = &Schedule{
		Type:       ScheduleCron,
		Expression: "0 9 * * 1-5",
		Timezone:   "America/Los_Angeles",
		LastRunAt:  &last,
		UpcomingRunsAt: []time.Time{
			time.Date(2026, 3, 17, 16, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 18, 16, 0, 0, 0, time.UTC),
		},
	}
	want := `{"type":"cron","expression":"0 9 * * 1-5","timezone":"America/Los_Angeles",` +
		`"last_run_at":"2026-03-16T16:00:09Z",` +
		`"upcoming_runs_at":["2026-03-17T16:00:00Z","2026-03-18T16:00:00Z"]}`
	if got := field(t, mustMarshal(t, d), "schedule"); got != want {
		t.Errorf("schedule =\n  %s\nwant\n  %s", got, want)
	}
}

// A schedule that has never fired reports last_run_at null — "null until one
// completes" — and an archived deployment's upcoming_runs_at is [], not null,
// because the member is an array whenever it is present.
func TestScheduleNeverFiredAndArchivedShapes(t *testing.T) {
	d := minimalDeployment()
	d.Schedule = &Schedule{Type: ScheduleCron, Expression: "0 9 * * 1-5", Timezone: "UTC"}

	var sched map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, d)["schedule"], &sched); err != nil {
		t.Fatal(err)
	}
	if got := string(sched["last_run_at"]); got != `null` {
		t.Errorf("last_run_at = %s, want null before the first fire completes", got)
	}
	if got := string(sched["upcoming_runs_at"]); got != `[]` {
		t.Errorf("upcoming_runs_at = %s, want []", got)
	}
}

// The agent reference is resolved: a concrete version, never a floating "latest".
func TestAgentReferenceCarriesAConcreteVersion(t *testing.T) {
	m := mustMarshal(t, minimalDeployment())
	want := `{"type":"agent","id":"agent_011CZkYpogX7uDKUyvBTophP","version":1}`
	if got := field(t, m, "agent"); got != want {
		t.Errorf("agent = %s, want %s", got, want)
	}
}

// created_by is audit-only and never reaches the wire — sessions.created_by's
// rule. It matters more on a deployment: a session a schedule fires has no
// creator, so this row is where the audit trail survives.
func TestDeploymentCreatedByNeverReachesTheWire(t *testing.T) {
	d := minimalDeployment()
	who := "apikey_011CZkZcDH3vPqd7xnEfwTai"
	d.CreatedBy = &who
	if m := mustMarshal(t, d); m["created_by"] != nil {
		t.Errorf("created_by was emitted as %s; it is audit-only", m["created_by"])
	}
}

// The pause columns are storage, not wire: paused_reason is the rendering of
// all three, and emitting them beside it would publish a second, unschema'd
// spelling of the same fact.
func TestDeploymentPauseColumnsAreNotOnTheWire(t *testing.T) {
	paused := time.Date(2026, 3, 16, 9, 0, 0, 0, time.UTC)
	d := minimalDeployment()
	d.PausedAt, d.PausedKind = &paused, PausedManual

	m := mustMarshal(t, d)
	for _, key := range []string{"paused_at", "paused_kind", "paused_error_type"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s reached the wire as %s", key, m[key])
		}
	}
}

// Every timestamp renders in UTC. The database hands them back in the
// connection's zone, so the same instant would otherwise reach one client as
// 2026-03-19T09:00:07Z and another as 2026-03-19T17:00:07+08:00 — the same
// moment, a different string, and a spurious diff for anything comparing two
// reads or a recorded response.
func TestDeploymentTimestampsRenderInUTC(t *testing.T) {
	east := time.FixedZone("UTC+8", 8*60*60)
	at := time.Date(2026, 3, 19, 9, 0, 7, 0, time.UTC).In(east)
	archived := time.Date(2026, 3, 20, 1, 2, 3, 0, time.UTC).In(east)

	d := minimalDeployment()
	d.CreatedAt, d.UpdatedAt, d.ArchivedAt = at, at, &archived
	d.Schedule = &Schedule{
		Type: ScheduleCron, Expression: "0 9 * * *", Timezone: "UTC",
		LastRunAt:      &at,
		UpcomingRunsAt: []time.Time{at},
	}

	m := mustMarshal(t, d)
	for _, key := range []string{"created_at", "updated_at", "archived_at"} {
		if got := field(t, m, key); !strings.HasSuffix(got, `Z"`) {
			t.Errorf("%s = %s, want a Z-suffixed UTC instant", key, got)
		}
	}
	var sched map[string]json.RawMessage
	if err := json.Unmarshal(m["schedule"], &sched); err != nil {
		t.Fatal(err)
	}
	if got := string(sched["last_run_at"]); got != `"2026-03-19T09:00:07Z"` {
		t.Errorf("last_run_at = %s, want \"2026-03-19T09:00:07Z\"", got)
	}
	if got := string(sched["upcoming_runs_at"]); got != `["2026-03-19T09:00:07Z"]` {
		t.Errorf("upcoming_runs_at = %s, want the same instant in UTC", got)
	}
}
