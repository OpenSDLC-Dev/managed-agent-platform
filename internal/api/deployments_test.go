package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// deploymentBody is the smallest create body the schema admits: name, agent,
// environment_id and at least one initial event.
func deploymentBody(agentID, envID string) map[string]any {
	return map[string]any{
		"name":           "Daily order report",
		"agent":          agentID,
		"environment_id": envID,
		"initial_events": []any{map[string]any{
			"type":    "user.message",
			"content": []any{map[string]any{"type": "text", "text": "Compile yesterday's orders."}},
		}},
	}
}

func createDeployment(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	if status != http.StatusOK {
		t.Fatalf("create deployment: status %d, body %v", status, res)
	}
	return res
}

// The schema's required list is what a client may read without checking
// presence, so every one of the sixteen is asserted present — and `budget`,
// the seventeenth property and the only optional one, asserted absent.
func TestCreateDeploymentRendersTheWholeRequiredList(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	d := createDeployment(t, s, deploymentBody(agentID, envID))
	wantFields(t, d, "type", "id", "name", "description", "agent", "environment_id",
		"vault_ids", "initial_events", "resources", "metadata", "schedule",
		"status", "paused_reason", "created_at", "updated_at", "archived_at")

	if _, ok := d["budget"]; ok {
		t.Errorf("budget was emitted as %v; no budget is ever set, so the key must be absent", d["budget"])
	}
	if d["type"] != "deployment" {
		t.Errorf("type = %v, want deployment", d["type"])
	}
	if d["status"] != "active" {
		t.Errorf("status = %v, want active", d["status"])
	}
	if d["paused_reason"] != nil {
		t.Errorf("paused_reason = %v, want null on an active deployment", d["paused_reason"])
	}
	if d["schedule"] != nil {
		t.Errorf("schedule = %v, want null — no schedule means manual-only", d["schedule"])
	}
	if d["description"] != nil {
		t.Errorf("description = %v, want null when unset", d["description"])
	}
	// The agent reference is resolved to a concrete version, never a floating
	// latest: publishing a new agent version must not change what tonight's
	// fire runs.
	agent, ok := d["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent = %v, want an object", d["agent"])
	}
	if agent["type"] != "agent" || agent["id"] != agentID || agent["version"] != float64(1) {
		t.Errorf("agent = %v, want {agent, %s, 1}", agent, agentID)
	}
	for _, key := range []string{"vault_ids", "resources"} {
		if got, ok := d[key].([]any); !ok || len(got) != 0 {
			t.Errorf("%s = %v, want []", key, d[key])
		}
	}
	if md, ok := d["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want {}", d["metadata"])
	}
}

// budget is documented on the reference's create and update params and on its
// CLI's flags, so refusing it breaks a real client path — deliberately, because
// storing an unenforced ceiling would let an operator believe one exists.
func TestDeploymentRejectsBudget(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// The reference's own budget shape, so the refusal is of a request a real
	// client would send rather than of an invented one.
	body := deploymentBody(agentID, envID)
	body["budget"] = map[string]any{
		"type":          "limit",
		"max_list_cost": map[string]any{"amount": "2000", "currency": "USD"},
	}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	d := createDeployment(t, s, deploymentBody(agentID, envID))
	status, res = s.do(http.MethodPost, "/v1/deployments/"+d["id"].(string),
		map[string]any{"budget": nil})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// A deployment admits system.message where a session does not — the one arm
// by which the two schemas differ.
func TestDeploymentInitialEventsAdmitSystemMessageAndSessionsDoNot(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	sys := map[string]any{"type": "system.message",
		"content": []any{map[string]any{"type": "text", "text": "Run quietly."}}}
	user := map[string]any{"type": "user.message",
		"content": []any{map[string]any{"type": "text", "text": "Compile yesterday's orders."}}}
	body := deploymentBody(agentID, envID)
	body["initial_events"] = []any{user, sys}
	createDeployment(t, s, body)

	status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "initial_events": []any{user, sys},
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The list has to survive the normalizer that will run at every fire, so it is
// run once here instead: a system.message that is not last, or that follows
// nothing, is refused at create rather than recorded as a failed run every
// night. The rule is events.NormalizeInbound's — called, not copied — and it
// makes this platform narrower than the union the reference publishes.
func TestDeploymentInitialEventsMustSurviveTheNormalizer(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	sys := map[string]any{"type": "system.message",
		"content": []any{map[string]any{"type": "text", "text": "Run quietly."}}}
	user := map[string]any{"type": "user.message",
		"content": []any{map[string]any{"type": "text", "text": "Compile yesterday's orders."}}}

	for name, evs := range map[string][]any{
		"a lone system.message follows nothing": {sys},
		"a system.message that is not last":     {user, sys, user},
	} {
		t.Run(name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			body["initial_events"] = evs
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// The same rule on update, where the list is a full replacement.
	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	status, res := s.do(http.MethodPost, "/v1/deployments/"+id,
		map[string]any{"initial_events": []any{sys}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// Both sub-objects are additionalProperties: false with a required type, and
// enforcing that one level down is the difference between a typo answering 400
// and a schedule firing in the wrong zone for months. The dotted name in the
// message says which object refused the key.
func TestDeploymentSubObjectsRefuseUnknownKeysAndRequireTheirType(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	cases := map[string]map[string]any{
		"schedule typo": {"schedule": map[string]any{
			"type": "cron", "expression": "0 9 * * *", "timezome": "America/New_York"}},
		"schedule without a type": {"schedule": map[string]any{
			"expression": "0 9 * * *", "timezone": "UTC"}},
		"an expression over 256 characters": {"schedule": map[string]any{
			"type": "cron", "timezone": "UTC",
			"expression": "0 " + strings.Repeat("1,", 130) + "1 * * *"}},
		"agent object with an extra key": {"agent": map[string]any{
			"type": "agent", "id": agentID, "system": "an override by another name"}},
		"agent object without a type": {"agent": map[string]any{"id": agentID}},
		"agent version below one":     {"agent": map[string]any{"type": "agent", "id": agentID, "version": 0}},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			for k, v := range patch {
				body[k] = v
			}
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// The refusal is the same on update, where the same parsers run.
	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	status, res := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{
		"schedule": map[string]any{"type": "cron", "expression": "0 9 * * *",
			"timezone": "UTC", "enabled": true}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// An unsupported method on a deployment path answers 405 like every other /v1
// family, not the catch-all 404 an unregistered pattern would give — including
// DELETE, which this surface deliberately does not serve.
func TestDeploymentPathsAnswer405NotTheCatchAll(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	for _, path := range []string{"/v1/deployments", "/v1/deployments/" + id,
		"/v1/deployments/" + id + "/archive", "/v1/deployments/" + id + "/pause",
		"/v1/deployments/" + id + "/unpause"} {
		status, res := s.do(http.MethodDelete, path, nil)
		wantErr(t, status, res, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}

// "At least 1, maximum 50", and the field is required — the floor is the half
// a session's own initial_events does not have.
func TestDeploymentInitialEventsFloorAndCeiling(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	for _, c := range []struct {
		name   string
		events []any
	}{
		{"absent", nil},
		{"empty", []any{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			if c.events == nil {
				delete(body, "initial_events")
			} else {
				body["initial_events"] = c.events
			}
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}

	many := make([]any, 51)
	for i := range many {
		many[i] = map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "text", "text": "x"}}}
	}
	body := deploymentBody(agentID, envID)
	body["initial_events"] = many
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The agent union is narrower than a session's by exactly one arm: there is no
// agent_with_overrides. Both spellings a deployment does admit resolve as the
// reference's do — a bare id pins the latest version, and an object pins the
// version it names or the latest when it names none.
func TestDeploymentAgentUnion(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// Publish a second version so "latest" and "version 1" differ.
	status, res := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"system": "v2"})
	if status != http.StatusOK {
		t.Fatalf("update agent: %d %v", status, res)
	}

	body := deploymentBody(agentID, envID)
	d := createDeployment(t, s, body)
	if got := d["agent"].(map[string]any)["version"]; got != float64(2) {
		t.Errorf("a bare id pinned version %v, want the latest (2)", got)
	}

	body = deploymentBody(agentID, envID)
	body["agent"] = map[string]any{"type": "agent", "id": agentID, "version": 1}
	d = createDeployment(t, s, body)
	if got := d["agent"].(map[string]any)["version"]; got != float64(1) {
		t.Errorf("an explicit reference pinned version %v, want 1", got)
	}

	// version is optional on the object arm — "Omit to use the latest version"
	// — so the versionless object resolves the same way the bare string does.
	body = deploymentBody(agentID, envID)
	body["agent"] = map[string]any{"type": "agent", "id": agentID}
	d = createDeployment(t, s, body)
	if got := d["agent"].(map[string]any)["version"]; got != float64(2) {
		t.Errorf("a versionless reference object pinned version %v, want the latest (2)", got)
	}

	// A malformed shape is the request's fault (400); an agent that is simply
	// not there is a 404, which is how resolveAgent already answers a session's
	// missing agent and how checkID already answers a malformed id.
	cases := map[string]struct {
		agent   any
		status  int
		errType string
	}{
		"with overrides":  {map[string]any{"type": "agent_with_overrides", "id": agentID, "system": "x"}, http.StatusBadRequest, "invalid_request_error"},
		"empty id":        {map[string]any{"type": "agent", "version": 1}, http.StatusBadRequest, "invalid_request_error"},
		"unknown agent":   {"agent_0000000000000000000000", http.StatusNotFound, "not_found_error"},
		"unknown version": {map[string]any{"type": "agent", "id": agentID, "version": 99}, http.StatusNotFound, "not_found_error"},
		"malformed id":    {"not-an-agent-id", http.StatusNotFound, "not_found_error"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			body["agent"] = c.agent
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, c.status, c.errType)
		})
	}
}

// An archived agent is refused wholesale, the rule resolveAgent already applies
// to a session.
func TestDeploymentRefusesArchivedAgentAndEnvironment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	otherAgent, otherEnv := fixture(t, s)
	if status, res := s.do(http.MethodPost, "/v1/agents/"+otherAgent+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive agent: %d %v", status, res)
	}
	if status, res := s.do(http.MethodPost, "/v1/environments/"+otherEnv+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive environment: %d %v", status, res)
	}

	body := deploymentBody(otherAgent, envID)
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	body = deploymentBody(agentID, otherEnv)
	status, res = s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	body = deploymentBody(agentID, "env_0000000000000000000000")
	status, res = s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")
}

// The schedule enables scheduled execution, and its two computed members are
// derived on every read: five occurrences whenever five exist, and null for a
// last run that has not happened.
func TestDeploymentScheduleComputesItsTimestamps(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * 1-5", "timezone": "America/Los_Angeles"}
	// Taken before the request, so the first occurrence has to clear it. The
	// zero time would be cleared by any timestamp at all, including one in
	// 2020, and looking forward is this field's whole contract.
	beforeCreate := time.Now().UTC()
	d := createDeployment(t, s, body)

	sched, ok := d["schedule"].(map[string]any)
	if !ok {
		t.Fatalf("schedule = %v, want an object", d["schedule"])
	}
	if sched["expression"] != "0 9 * * 1-5" || sched["timezone"] != "America/Los_Angeles" || sched["type"] != "cron" {
		t.Errorf("schedule = %v, want the stored pair echoed", sched)
	}
	if sched["last_run_at"] != nil {
		t.Errorf("last_run_at = %v, want null until a scheduled run completes", sched["last_run_at"])
	}
	upcoming, ok := sched["upcoming_runs_at"].([]any)
	if !ok || len(upcoming) != 5 {
		t.Fatalf("upcoming_runs_at = %v, want 5 occurrences", sched["upcoming_runs_at"])
	}
	// Ascending, in the future, and matching the expression's own hour once
	// rendered in the configured zone.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	prev := beforeCreate
	for i, raw := range upcoming {
		at, err := time.Parse(time.RFC3339, raw.(string))
		if err != nil {
			t.Fatalf("occurrence %d: %v", i, err)
		}
		if !at.After(prev) {
			t.Errorf("occurrence %d (%s) is not after %s", i, at, prev)
		}
		if lt := at.In(loc); lt.Hour() != 9 || lt.Minute() != 0 {
			t.Errorf("occurrence %d renders locally as %s, want 09:00", i, lt)
		}
		if wd := at.In(loc).Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Errorf("occurrence %d falls on %s, which 1-5 excludes", i, wd)
		}
		prev = at
	}
}

// An expression that parses but can never fire is refused rather than stored:
// upcoming_runs_at would be [] on an active deployment, the shape the wire
// reserves for an archived one.
func TestDeploymentScheduleRejections(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	for name, sched := range map[string]any{
		"unsatisfiable":    map[string]any{"type": "cron", "expression": "0 0 31 2 *", "timezone": "UTC"},
		"six fields":       map[string]any{"type": "cron", "expression": "* * * * * *", "timezone": "UTC"},
		"predefined":       map[string]any{"type": "cron", "expression": "@daily", "timezone": "UTC"},
		"L is unsupported": map[string]any{"type": "cron", "expression": "0 0 L * *", "timezone": "UTC"},
		"unknown zone":     map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "Mars/Olympus"},
		"host-local zone":  map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "Local"},
		"no timezone":      map[string]any{"type": "cron", "expression": "0 9 * * *"},
		"no expression":    map[string]any{"type": "cron", "timezone": "UTC"},
		"unsupported type": map[string]any{"type": "interval", "expression": "0 9 * * *", "timezone": "UTC"},
		"not an object":    "0 9 * * *",
	} {
		t.Run(name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			body["schedule"] = sched
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}
}

// Pause and unpause are the two halves of one column triple, and status and
// paused_reason are rendered from it — never stored, so they cannot disagree.
func TestDeploymentPauseAndUnpause(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "UTC"}
	id := createDeployment(t, s, body)["id"].(string)

	status, d := s.do(http.MethodPost, "/v1/deployments/"+id+"/pause", nil)
	if status != http.StatusOK {
		t.Fatalf("pause: %d %v", status, d)
	}
	if d["status"] != "paused" {
		t.Errorf("status = %v, want paused", d["status"])
	}
	reason, ok := d["paused_reason"].(map[string]any)
	if !ok || reason["type"] != "manual" {
		t.Fatalf("paused_reason = %v, want {\"type\":\"manual\"}", d["paused_reason"])
	}
	if _, ok := reason["error"]; ok {
		t.Errorf("a manual pause carries an error: %v", reason)
	}
	// "Non-empty for active AND paused deployments (reflects what the schedule
	// would do if unpaused)."
	if up, ok := d["schedule"].(map[string]any)["upcoming_runs_at"].([]any); !ok || len(up) != 5 {
		t.Errorf("a paused deployment's upcoming_runs_at = %v, want 5", d["schedule"])
	}

	// Idempotent, and a body on an action endpoint is neither read nor
	// refused — both halves of what the registry entry claims.
	pausedAt := storedPausedAt(t, s, id)
	status, again := s.do(http.MethodPost, "/v1/deployments/"+id+"/pause",
		map[string]any{"nonsense": true, "budget": 9})
	if status != http.StatusOK || again["status"] != "paused" {
		t.Fatalf("second pause with a body: %d %v", status, again)
	}
	// A second pause must not restamp when the schedule stopped, which is the
	// only record an operator has of it.
	if now := storedPausedAt(t, s, id); !now.Equal(pausedAt) {
		t.Errorf("a second pause moved paused_at from %s to %s", pausedAt, now)
	}

	status, d = s.do(http.MethodPost, "/v1/deployments/"+id+"/unpause", nil)
	if status != http.StatusOK {
		t.Fatalf("unpause: %d %v", status, d)
	}
	if d["status"] != "active" || d["paused_reason"] != nil {
		t.Errorf("after unpause status = %v, paused_reason = %v; want active and null", d["status"], d["paused_reason"])
	}

	// Unpausing an already-active deployment is a 200 too — and must not move
	// the resume watermark, which slice 4 makes the floor of the catch-up
	// scan: a retry that advanced it would silently drop a due occurrence.
	resumed := storedResumedAt(t, s, id)
	status, d = s.do(http.MethodPost, "/v1/deployments/"+id+"/unpause", map[string]any{"nonsense": true})
	if status != http.StatusOK || d["status"] != "active" {
		t.Fatalf("second unpause: %d %v", status, d)
	}
	if now := storedResumedAt(t, s, id); !now.Equal(resumed) {
		t.Errorf("unpausing an active deployment moved schedule_resumed_at from %s to %s", resumed, now)
	}
}

func storedPausedAt(t *testing.T, s *tserver, id string) time.Time {
	t.Helper()
	var at *time.Time
	if err := s.pool.QueryRow(t.Context(),
		`SELECT paused_at FROM deployments WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at == nil {
		t.Fatalf("deployment %s has no paused_at", id)
	}
	return *at
}

func storedResumedAt(t *testing.T, s *tserver, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(t.Context(),
		`SELECT schedule_resumed_at FROM deployments WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// An auto-pause renders the error variant. Slice 4 writes these columns; the
// rendering is slice 1's, so it is asserted against a row written directly.
func TestDeploymentErrorPauseRendersItsErrorType(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	if _, err := s.pool.Exec(t.Context(),
		`UPDATE deployments SET paused_at = now(), paused_kind = 'error',
		   paused_error_type = 'vault_archived_error' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	status, d := s.do(http.MethodGet, "/v1/deployments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, d)
	}
	if d["status"] != "paused" {
		t.Fatalf("status = %v, want paused", d["status"])
	}
	reason := d["paused_reason"].(map[string]any)
	if reason["type"] != "error" {
		t.Fatalf("paused_reason.type = %v, want error", reason["type"])
	}
	inner, ok := reason["error"].(map[string]any)
	if !ok || inner["type"] != "vault_archived_error" {
		t.Errorf("paused_reason.error = %v, want {\"type\":\"vault_archived_error\"}", reason["error"])
	}

	// A manual pause on top of an auto-pause must not rewrite the cause: the
	// error type is the only surviving explanation of why the schedule
	// stopped, and "manual" would erase it while still reporting paused.
	status, d = s.do(http.MethodPost, "/v1/deployments/"+id+"/pause", nil)
	if status != http.StatusOK {
		t.Fatalf("pause over an error pause: %d %v", status, d)
	}
	reason = d["paused_reason"].(map[string]any)
	if reason["type"] != "error" {
		t.Fatalf("a manual pause rewrote the reason to %v", reason)
	}
	if inner, ok := reason["error"].(map[string]any); !ok || inner["type"] != "vault_archived_error" {
		t.Errorf("paused_reason.error = %v after a manual pause, want the auto-pause's cause", reason["error"])
	}
}

// Archive is one-way, idempotent, and terminal for every mutating action —
// while GET still returns the row, reporting active with archived_at set.
func TestArchiveDeploymentIsTerminalForMutation(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "UTC"}
	id := createDeployment(t, s, body)["id"].(string)

	// Paused first, so archive's "reports active whatever the pause columns
	// say" is actually exercised rather than trivially true.
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+id+"/pause", nil); status != http.StatusOK {
		t.Fatalf("pause: %d %v", status, res)
	}
	status, d := s.do(http.MethodPost, "/v1/deployments/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, d)
	}
	if d["archived_at"] == nil {
		t.Fatal("archived_at is null after archive")
	}
	if d["status"] != "active" {
		t.Errorf("status = %v, want active — archived deployments report active", d["status"])
	}
	if d["paused_reason"] != nil {
		t.Errorf("paused_reason = %v, want null; it is non-null exactly when status is paused", d["paused_reason"])
	}
	if up, ok := d["schedule"].(map[string]any)["upcoming_runs_at"].([]any); !ok || len(up) != 0 {
		t.Errorf("an archived deployment's upcoming_runs_at = %v, want []", d["schedule"])
	}

	// Idempotent: a second archive changes nothing.
	status, again := s.do(http.MethodPost, "/v1/deployments/"+id+"/archive", nil)
	if status != http.StatusOK || again["archived_at"] != d["archived_at"] {
		t.Fatalf("second archive moved archived_at: %d %v", status, again)
	}

	// Terminal for update, pause and unpause; GET still answers.
	for _, path := range []string{"", "/pause", "/unpause"} {
		status, res := s.do(http.MethodPost, "/v1/deployments/"+id+path, map[string]any{"name": "renamed"})
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}
	if status, res := s.do(http.MethodGet, "/v1/deployments/"+id, nil); status != http.StatusOK {
		t.Fatalf("get after archive: %d %v", status, res)
	}
}

// "Omit a field to preserve its current value" — with vault_ids,
// initial_events, resources and schedule as full replacements and metadata
// alone as a patch.
func TestUpdateDeploymentPreservesReplacesAndPatches(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["description"] = "Compiles yesterday's orders."
	body["metadata"] = map[string]any{"team": "ops", "tier": "gold"}
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "UTC"}
	d := createDeployment(t, s, body)
	id := d["id"].(string)

	// A rename preserves everything else.
	status, up := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"name": "Renamed"})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, up)
	}
	if up["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", up["name"])
	}
	if up["description"] != "Compiles yesterday's orders." {
		t.Errorf("description = %v; an omitted field preserves", up["description"])
	}
	if md := up["metadata"].(map[string]any); md["team"] != "ops" || md["tier"] != "gold" {
		t.Errorf("metadata = %v; an omitted bag preserves", md)
	}
	if sch := up["schedule"].(map[string]any); sch["expression"] != "0 9 * * *" {
		t.Errorf("schedule = %v; an omitted schedule preserves", sch)
	}

	// metadata is a patch: a string upserts, a null deletes, and the rest stays.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{
		"metadata": map[string]any{"tier": nil, "owner": "ada"}})
	if status != http.StatusOK {
		t.Fatalf("patch metadata: %d %v", status, up)
	}
	md := up["metadata"].(map[string]any)
	if md["team"] != "ops" || md["owner"] != "ada" {
		t.Errorf("metadata = %v, want team preserved and owner upserted", md)
	}
	if _, ok := md["tier"]; ok {
		t.Errorf("metadata still carries tier: %v", md)
	}

	// schedule is a full replacement, and null clears it back to manual-only.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"schedule": nil})
	if status != http.StatusOK {
		t.Fatalf("clear schedule: %d %v", status, up)
	}
	if up["schedule"] != nil {
		t.Errorf("schedule = %v, want null after an explicit null", up["schedule"])
	}

	// description accepts an explicit null.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"description": nil})
	if status != http.StatusOK {
		t.Fatalf("clear description: %d %v", status, up)
	}
	if up["description"] != nil {
		t.Errorf("description = %v, want null", up["description"])
	}

	// "Cannot be cleared."
	status, res := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"agent": nil})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// A bare agent string on update re-pins to the latest version.
func TestUpdateDeploymentRepinsTheAgent(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["agent"] = map[string]any{"type": "agent", "id": agentID, "version": 1}
	id := createDeployment(t, s, body)["id"].(string)

	if status, res := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"system": "v2"}); status != http.StatusOK {
		t.Fatalf("update agent: %d %v", status, res)
	}
	status, up := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"agent": agentID})
	if status != http.StatusOK {
		t.Fatalf("repin: %d %v", status, up)
	}
	if got := up["agent"].(map[string]any)["version"]; got != float64(2) {
		t.Errorf("re-pinned to version %v, want the latest (2)", got)
	}
}

// The list is newest-first and keyset-paged, excludes archived rows by default,
// and refuses status combined with include_archived — the two ask for a set
// whose membership rule contradicts itself, since an archived deployment
// reports active.
func TestListDeployments(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	var ids []string
	for i := range 3 {
		body := deploymentBody(agentID, envID)
		body["name"] = fmt.Sprintf("deployment %d", i)
		ids = append(ids, createDeployment(t, s, body)["id"].(string))
	}
	paused := ids[0]
	archived := ids[1]
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+paused+"/pause", nil); status != http.StatusOK {
		t.Fatalf("pause: %d %v", status, res)
	}
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, res)
	}

	status, body := s.do(http.MethodGet, "/v1/deployments", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	got := listData(t, body)
	if len(got) != 2 {
		t.Fatalf("default list returned %d rows, want 2 (archived excluded)", len(got))
	}
	// Newest first.
	if got[0]["id"] != ids[2] {
		t.Errorf("first row is %v, want the newest %s", got[0]["id"], ids[2])
	}

	status, body = s.do(http.MethodGet, "/v1/deployments?include_archived=true", nil)
	if status != http.StatusOK || len(listData(t, body)) != 3 {
		t.Fatalf("include_archived: %d %v", status, body)
	}

	status, body = s.do(http.MethodGet, "/v1/deployments?status=paused", nil)
	if status != http.StatusOK {
		t.Fatalf("status filter: %d %v", status, body)
	}
	rows := listData(t, body)
	if len(rows) != 1 || rows[0]["id"] != paused {
		t.Errorf("status=paused returned %v, want just %s", rows, paused)
	}

	status, body = s.do(http.MethodGet, "/v1/deployments?status=active&include_archived=true", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	status, body = s.do(http.MethodGet, "/v1/deployments?status=nonsense", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// One row per page walks the keyset to the end.
	seen := map[string]bool{}
	path := "/v1/deployments?limit=1"
	for range 5 {
		status, body = s.do(http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("page: %d %v", status, body)
		}
		for _, row := range listData(t, body) {
			id := row["id"].(string)
			if seen[id] {
				t.Fatalf("page repeated %s", id)
			}
			seen[id] = true
		}
		cur := nextPage(t, body)
		if cur == "" {
			break
		}
		path = "/v1/deployments?limit=1&page=" + cur
	}
	if len(seen) != 2 {
		t.Errorf("paging saw %d rows, want 2", len(seen))
	}
}

// created_at range filters, through the shared parseTimeParam idiom.
func TestListDeploymentsFiltersByCreatedAt(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	d := createDeployment(t, s, deploymentBody(agentID, envID))
	created, err := time.Parse(time.RFC3339, d["created_at"].(string))
	if err != nil {
		t.Fatal(err)
	}

	before := created.Add(-time.Hour).UTC().Format(time.RFC3339)
	after := created.Add(time.Hour).UTC().Format(time.RFC3339)

	status, body := s.do(http.MethodGet, "/v1/deployments?created_at%5Bgte%5D="+before, nil)
	if status != http.StatusOK || len(listData(t, body)) != 1 {
		t.Errorf("gte before creation returned %v", body)
	}
	status, body = s.do(http.MethodGet, "/v1/deployments?created_at%5Bgte%5D="+after, nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("gte after creation returned %v", body)
	}
	status, body = s.do(http.MethodGet, "/v1/deployments?created_at%5Blte%5D="+before, nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("lte before creation returned %v", body)
	}
}

// The documented agent_id filter — "Filter by agent ID" — and the shape rule
// it shares with the sessions list: a malformed id is a 400 rather than a bind
// parameter that fails as a 500, while a well-formed id naming nothing filters
// to an empty page (#135). A filter that is accepted and ignored would answer
// the wrong set with a 200, which is worse than either.
func TestListDeploymentsFilterByAgent(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	otherAgent, _ := fixture(t, s)

	mine := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	createDeployment(t, s, deploymentBody(otherAgent, envID))

	status, body := s.do(http.MethodGet, "/v1/deployments?agent_id="+agentID, nil)
	if status != http.StatusOK {
		t.Fatalf("agent_id filter: %d %v", status, body)
	}
	rows := listData(t, body)
	if len(rows) != 1 || rows[0]["id"] != mine {
		t.Errorf("agent_id=%s returned %v, want just %s", agentID, rows, mine)
	}

	status, body = s.do(http.MethodGet, "/v1/deployments?agent_id=agent_0000000000000000000000", nil)
	if status != http.StatusOK || len(listData(t, body)) != 0 {
		t.Errorf("an agent with no deployments returned %d %v, want an empty page", status, body)
	}

	status, body = s.do(http.MethodGet, "/v1/deployments?agent_id=%00", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

// A cursor this listing cannot honor is refused rather than ignored. The
// deployments keyset is by created_at and walks forward only, so a version
// cursor — taken here from the listing that really issues one — would
// otherwise silently return the first page again.
func TestListDeploymentsRejectsACursorItCannotHonor(t *testing.T) {
	s := newTestServer(t)
	agentID, _ := fixture(t, s)
	if status, res := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"system": "v2"}); status != http.StatusOK {
		t.Fatalf("publish a second agent version: %d %v", status, res)
	}
	status, body := s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("agent versions: %d %v", status, body)
	}
	versionCursor := nextPage(t, body)
	if versionCursor == "" {
		t.Fatal("the agent versions listing issued no cursor to borrow")
	}

	for _, cur := range []string{versionCursor, "not-a-cursor"} {
		status, body := s.do(http.MethodGet, "/v1/deployments?page="+url.QueryEscape(cur), nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

// Every id-shaped path answers a malformed id exactly as it answers an unknown
// one — checkID's rule, so that a caller cannot probe which ids exist by
// telling the two apart (#135).
func TestDeploymentIDShapes(t *testing.T) {
	s := newTestServer(t)

	for _, path := range []string{"", "/archive", "/pause", "/unpause"} {
		for _, id := range []string{"not-an-id", "depl_0000000000000000000000"} {
			status, res := s.do(http.MethodPost, "/v1/deployments/"+id+path, map[string]any{})
			wantErr(t, status, res, http.StatusNotFound, "not_found_error")
		}
	}
	for _, id := range []string{"not-an-id", "depl_0000000000000000000000"} {
		status, res := s.do(http.MethodGet, "/v1/deployments/"+id, nil)
		wantErr(t, status, res, http.StatusNotFound, "not_found_error")
	}
}

// Unknown keys are refused on create and on update, the createSession
// precedent — which is also the mechanism that turns `budget` into a 400.
func TestDeploymentRejectsUnknownKeys(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["schedul"] = map[string]any{"type": "cron"}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"nmae": "typo"})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The documented caps no column enforces.
func TestDeploymentBounds(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	long := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'x'
		}
		return string(b)
	}
	cases := map[string]func(map[string]any){
		"name empty":       func(b map[string]any) { b["name"] = "" },
		"name too long":    func(b map[string]any) { b["name"] = long(257) },
		"description long": func(b map[string]any) { b["description"] = long(2049) },
		"too many vaults": func(b map[string]any) {
			ids := make([]any, 51)
			for i := range ids {
				ids[i] = "vlt_0000000000000000000000"
			}
			b["vault_ids"] = ids
		},
		"environment_id too long": func(b map[string]any) { b["environment_id"] = long(129) },
		"too many resources": func(b map[string]any) {
			rs := make([]any, 501)
			for i := range rs {
				rs[i] = map[string]any{"type": "file",
					"file_id": "file_0000000000000000000000gk", "mount_path": fmt.Sprintf("/w/f%d", i)}
			}
			b["resources"] = rs
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := deploymentBody(agentID, envID)
			mutate(body)
			status, res := s.do(http.MethodPost, "/v1/deployments", body)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}
}

// errorMessage pairs a run's error type with the message the CHECK requires
// beside it, and stays null when there is no error.
func errorMessage(errorType any) any {
	if errorType == nil {
		return nil
	}
	return fmt.Sprintf("%v recorded by a test fixture", errorType)
}

// last_run_at is derived: the created_at of the run with the greatest
// scheduled_at, counting only scheduled runs that actually started a session.
// Slice 4 writes these rows; the derivation is slice 1's.
func TestLastRunAtIsDerivedFromTheRunHistory(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "UTC"}
	id := createDeployment(t, s, body)["id"].(string)

	sessionID := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)

	// errorType keeps each fixture in the one shape a committed run may take —
	// exactly one of session_id and error_type set, which the runs table holds
	// by construction rather than by a CHECK. The message travels with it
	// because deployment_runs_error_pair does check that the two agree.
	insert := func(runID, trigger string, scheduledAt any, session any, errorType any, createdAt string) {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(),
			`INSERT INTO deployment_runs (id, deployment_id, trigger_type, scheduled_at,
			   agent_id, agent_version, session_id, error_type, error_message, created_at)
			 VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9)`,
			runID, id, trigger, scheduledAt, agentID, session, errorType, errorMessage(errorType), createdAt); err != nil {
			t.Fatal(err)
		}
	}

	// A manual run does not move the field, and neither does a scheduled run
	// that never started a session.
	insert("drun_manual", "manual", nil, sessionID, nil, "2026-03-20T10:00:00Z")
	insert("drun_failed", "schedule", "2026-03-21T09:00:00Z", nil,
		"environment_archived_error", "2026-03-21T09:00:05Z")

	status, d := s.do(http.MethodGet, "/v1/deployments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, d)
	}
	if got := d["schedule"].(map[string]any)["last_run_at"]; got != nil {
		t.Errorf("last_run_at = %v; a manual run and a failed fire must not move it", got)
	}

	// A successful scheduled fire does, and reports created_at — when the run
	// actually started — not scheduled_at, the pre-jitter cron match.
	insert("drun_ok", "schedule", "2026-03-19T09:00:00Z", sessionID, nil, "2026-03-19T09:00:07Z")
	status, d = s.do(http.MethodGet, "/v1/deployments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, d)
	}
	if got := d["schedule"].(map[string]any)["last_run_at"]; got != "2026-03-19T09:00:07Z" {
		t.Errorf("last_run_at = %v, want the run's created_at 2026-03-19T09:00:07Z", got)
	}

	// Ordering is by scheduled_at, not by created_at: a later occurrence
	// inserted earlier still wins, which is what "the most recent scheduled
	// run" names.
	insert("drun_later_occurrence", "schedule", "2026-03-22T09:00:00Z", sessionID, nil, "2026-03-19T09:00:01Z")
	status, d = s.do(http.MethodGet, "/v1/deployments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, d)
	}
	if got := d["schedule"].(map[string]any)["last_run_at"]; got != "2026-03-19T09:00:01Z" {
		t.Errorf("last_run_at = %v, want the greatest scheduled_at's created_at", got)
	}

	// "Preserved after the deployment is archived" — archive touches no run row.
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+id+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, res)
	}
	status, d = s.do(http.MethodGet, "/v1/deployments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, d)
	}
	sched := d["schedule"].(map[string]any)
	if sched["last_run_at"] != "2026-03-19T09:00:01Z" {
		t.Errorf("last_run_at = %v after archive; it is preserved", sched["last_run_at"])
	}
	if up := sched["upcoming_runs_at"].([]any); len(up) != 0 {
		t.Errorf("upcoming_runs_at = %v after archive, want []", up)
	}
}

// created_by is written for the audit trail and never reaches the wire.
func TestDeploymentCreatedByIsAuditOnly(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	d := createDeployment(t, s, deploymentBody(agentID, envID))

	if _, ok := d["created_by"]; ok {
		t.Errorf("created_by reached the wire as %v", d["created_by"])
	}
	var createdBy *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT created_by FROM deployments WHERE id = $1`, d["id"]).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy == nil || *createdBy == "" {
		t.Error("created_by was not recorded; it is the only audit trail a fired session leaves")
	}
}

// The resources echo drops the write-only token and keeps everything else.
func TestDeploymentResourcesEchoWithoutTheToken(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["resources"] = []any{map[string]any{
		"type":                "github_repository",
		"url":                 "https://github.com/acme/orders",
		"authorization_token": "ghp_secret",
		"mount_path":          "/workspace/orders",
	}}
	d := createDeployment(t, s, body)

	rs, ok := d["resources"].([]any)
	if !ok || len(rs) != 1 {
		t.Fatalf("resources = %v, want one element", d["resources"])
	}
	el := rs[0].(map[string]any)
	if el["type"] != "github_repository" || el["url"] != "https://github.com/acme/orders" {
		t.Errorf("resource = %v, want the input echoed", el)
	}
	// "The authorization token is write-only and never returned", under either
	// spelling — the input's and the one a careless echo might invent.
	for _, key := range []string{"authorization_token", "token"} {
		if _, ok := el[key]; ok {
			t.Errorf("the token was echoed as %q: %v", key, el)
		}
	}

	// Stored sealed, never in plaintext (plan 25 decision 2).
	var stored []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT resources FROM deployments WHERE id = $1`, d["id"]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stored) {
		t.Fatalf("stored resources are not valid JSON: %s", stored)
	}
	if strings.Contains(string(stored), "ghp_secret") {
		t.Error("the repository token is stored in plaintext")
	}
}

// A file rubric needs object storage to snapshot into. Refusing it at create
// beats recording a failed run every night on a deployment that can never
// grade, so the check runs here rather than at the fire — and this is the only
// path that reaches it, since the ordinary test server always has storage.
func TestDeploymentFileRubricNeedsObjectStorage(t *testing.T) {
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}
	agentID, envID := fixture(t, s)

	rubric := map[string]any{
		"type":        "user.define_outcome",
		"description": "Grade the report.",
		"rubric":      map[string]any{"type": "file", "file_id": "file_0000000000000000000000gk"},
	}
	body := deploymentBody(agentID, envID)
	body["initial_events"] = []any{rubric}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// A text rubric is unaffected — it needs no blob to snapshot.
	body = deploymentBody(agentID, envID)
	body["initial_events"] = []any{map[string]any{
		"type":        "user.define_outcome",
		"description": "Grade the report.",
		"rubric":      map[string]any{"type": "text", "content": "The report exists."},
	}}
	createDeployment(t, s, body)
}

// A repository resource on a cipher-less deployment is refused rather than
// stored: plan 25 decision 2's "never stored unencrypted" applies to a
// deployment's copy of the token exactly as it does to a session's.
func TestDeploymentRepositoryNeedsACipher(t *testing.T) {
	s := newTestServerWithCipher(t, nil)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["resources"] = []any{map[string]any{
		"type": "github_repository", "url": "https://github.com/acme/orders",
		"authorization_token": "ghp_secret",
	}}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErr(t, status, res, http.StatusInternalServerError, "api_error")

	// The refusal happens before the row is written.
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d deployments were stored despite the refusal", count)
	}
}

// vault_ids are validated against live rows, FOR SHARE, so an archive cannot
// land between the check and the insert.
func TestDeploymentVaultIDs(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	vaultID := createVault(t, s, "prod-creds")

	body := deploymentBody(agentID, envID)
	body["vault_ids"] = []any{vaultID}
	d := createDeployment(t, s, body)
	if got := d["vault_ids"].([]any); len(got) != 1 || got[0] != vaultID {
		t.Errorf("vault_ids = %v, want [%s]", d["vault_ids"], vaultID)
	}

	// Unknown, malformed and archived are each a 400.
	for name, ids := range map[string][]any{
		"unknown":   {"vlt_0000000000000000000000"},
		"malformed": {"not-a-vault"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := deploymentBody(agentID, envID)
			bad["vault_ids"] = ids
			status, res := s.do(http.MethodPost, "/v1/deployments", bad)
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}

	if status, res := s.do(http.MethodPost, "/v1/vaults/"+vaultID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive vault: %d %v", status, res)
	}
	bad := deploymentBody(agentID, envID)
	bad["vault_ids"] = []any{vaultID}
	status, res := s.do(http.MethodPost, "/v1/deployments", bad)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// An update replacing vault_ids revalidates them.
	status, res = s.do(http.MethodPost, "/v1/deployments/"+d["id"].(string),
		map[string]any{"vault_ids": []any{"vlt_0000000000000000000000"}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The four full-replacement fields replace rather than merge, and an omitted
// one is preserved — including resources, whose stored form carries a sealed
// token the echo never shows, so preserving it cannot go through the echo.
func TestUpdateDeploymentReplacesTheCollections(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["resources"] = []any{map[string]any{
		"type": "github_repository", "url": "https://github.com/acme/orders",
		"authorization_token": "ghp_first", "mount_path": "/workspace/orders",
	}}
	d := createDeployment(t, s, body)
	id := d["id"].(string)

	// An update that does not mention resources preserves them — and preserves
	// the sealed token with them, which a round-trip through the echo would
	// have dropped.
	status, up := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"name": "Renamed"})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, up)
	}
	if rs := up["resources"].([]any); len(rs) != 1 {
		t.Fatalf("resources = %v after an unrelated update, want the one element preserved", up["resources"])
	}
	var stored []byte
	if err := s.pool.QueryRow(t.Context(),
		`SELECT resources FROM deployments WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "ciphertext") {
		t.Errorf("the sealed token was lost by an update that did not mention resources: %s", stored)
	}

	// Replacing resources with an empty list clears them.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"resources": []any{}})
	if status != http.StatusOK {
		t.Fatalf("clear resources: %d %v", status, up)
	}
	if rs := up["resources"].([]any); len(rs) != 0 {
		t.Errorf("resources = %v, want [] after an explicit empty replacement", rs)
	}

	// initial_events is a full replacement too. A system.message has to follow
	// a user.message to survive the normalizer, so the replacement is the pair.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{
		"initial_events": []any{
			map[string]any{"type": "user.message",
				"content": []any{map[string]any{"type": "text", "text": "again"}}},
			map[string]any{"type": "system.message",
				"content": []any{map[string]any{"type": "text", "text": "quietly"}}},
		}})
	if status != http.StatusOK {
		t.Fatalf("replace initial_events: %d %v", status, up)
	}
	ev := up["initial_events"].([]any)
	if len(ev) != 2 || ev[1].(map[string]any)["type"] != "system.message" {
		t.Errorf("initial_events = %v, want the two-event replacement", ev)
	}

	// And an update cannot empty it: the floor applies to the stored result.
	status, res := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"initial_events": []any{}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The update params draw a line that the two spellings of "empty" have to
// respect. vault_ids and resources read "send empty array or null to clear",
// so an explicit null clears them; initial_events, environment_id and agent
// read "cannot be cleared", so an explicit null is a 400. Testing null through
// present() would have passed either way, because present() reports the same
// false for a key that is absent and a key that is null.
func TestUpdateDeploymentNullClearsOnlyWhatMayBeCleared(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["vault_ids"] = []any{createVault(t, s, "prod-creds")}
	body["resources"] = []any{map[string]any{
		"type": "github_repository", "url": "https://github.com/acme/orders",
		"authorization_token": "ghp_first", "mount_path": "/workspace/orders",
	}}
	id := createDeployment(t, s, body)["id"].(string)

	status, up := s.do(http.MethodPost, "/v1/deployments/"+id,
		map[string]any{"vault_ids": nil, "resources": nil})
	if status != http.StatusOK {
		t.Fatalf("clear with null: %d %v", status, up)
	}
	if got := up["vault_ids"].([]any); len(got) != 0 {
		t.Errorf("vault_ids = %v after an explicit null, want [] — null clears", got)
	}
	if got := up["resources"].([]any); len(got) != 0 {
		t.Errorf("resources = %v after an explicit null, want [] — null clears", got)
	}

	for _, key := range []string{"initial_events", "environment_id", "agent"} {
		t.Run(key, func(t *testing.T) {
			status, res := s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{key: nil})
			wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// metadata is the third answer, and deliberately so: it is a patch rather
	// than a replacement, and a null patch preserves the bag — the rule the
	// shared patchMetadata already applies to agents and memory stores.
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"metadata": map[string]any{"team": "ops"}})
	if status != http.StatusOK {
		t.Fatalf("set metadata: %d %v", status, up)
	}
	status, up = s.do(http.MethodPost, "/v1/deployments/"+id, map[string]any{"metadata": nil})
	if status != http.StatusOK {
		t.Fatalf("null metadata: %d %v", status, up)
	}
	if md, ok := up["metadata"].(map[string]any); !ok || md["team"] != "ops" {
		t.Errorf("metadata = %v after an explicit null, want the bag preserved", up["metadata"])
	}
}

// Moving a deployment to another environment revalidates the target, and an
// archived or unknown one is refused.
func TestUpdateDeploymentChangesEnvironment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	id := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	other := createEnvironment(t, s, map[string]any{"name": "second-env"})["id"].(string)
	status, up := s.do(http.MethodPost, "/v1/deployments/"+id,
		map[string]any{"environment_id": other})
	if status != http.StatusOK {
		t.Fatalf("move: %d %v", status, up)
	}
	if up["environment_id"] != other {
		t.Errorf("environment_id = %v, want %s", up["environment_id"], other)
	}

	status, res := s.do(http.MethodPost, "/v1/deployments/"+id,
		map[string]any{"environment_id": "env_0000000000000000000000"})
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")

	if status, res := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, res)
	}
	status, res = s.do(http.MethodPost, "/v1/deployments/"+id,
		map[string]any{"environment_id": envID})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}
