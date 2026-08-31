package api_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// runDeployment fires a manual run and fails the test on a non-200. The body
// is nil on purpose: the action endpoints read no request body.
func runDeployment(t *testing.T, s *tserver, id string) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/deployments/"+id+"/run", nil)
	if status != http.StatusOK {
		t.Fatalf("run deployment %s: status %d, body %v", id, status, res)
	}
	return res
}

// The manual-run happy path, asserted against the schema's whole required
// list: the run settles on its success arm, and the session it created carries
// the deployment's reference back (sessions.deployment_id, migration 0032).
func TestDeploymentRunCreatesASessionAndRendersTheRun(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	d := createDeployment(t, s, deploymentBody(agentID, envID))
	deplID := d["id"].(string)

	run := runDeployment(t, s, deplID)
	wantFields(t, run, "type", "id", "deployment_id", "trigger_context",
		"session_id", "error", "agent", "created_at")
	if run["type"] != "deployment_run" {
		t.Errorf("type = %v, want deployment_run", run["type"])
	}
	runID, _ := run["id"].(string)
	if !strings.HasPrefix(runID, "drun_") {
		t.Errorf("id = %q, want a drun_ prefix", runID)
	}
	if run["deployment_id"] != deplID {
		t.Errorf("deployment_id = %v, want %s", run["deployment_id"], deplID)
	}
	// The manual context is {"type":"manual"} and nothing else: the reference
	// closes it with additionalProperties: false, so a scheduled_at key —
	// even a null one — is out of schema.
	tc, _ := run["trigger_context"].(map[string]any)
	if tc["type"] != "manual" {
		t.Errorf("trigger_context.type = %v, want manual", tc["type"])
	}
	if _, ok := tc["scheduled_at"]; ok {
		t.Errorf("trigger_context = %v; a manual context must not carry scheduled_at", tc)
	}
	if run["error"] != nil {
		t.Errorf("error = %v, want null on success", run["error"])
	}
	agent, _ := run["agent"].(map[string]any)
	if agent["type"] != "agent" || agent["id"] != agentID || agent["version"] != float64(1) {
		t.Errorf("agent = %v, want the pinned {type:agent, id:%s, version:1}", agent, agentID)
	}

	sessionID, _ := run["session_id"].(string)
	if !strings.HasPrefix(sessionID, "sesn_") {
		t.Fatalf("session_id = %v, want a sesn_ id", run["session_id"])
	}
	status, sess := s.do(http.MethodGet, "/v1/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("get fired session: status %d, body %v", status, sess)
	}
	if sess["deployment_id"] != deplID {
		t.Errorf("session.deployment_id = %v, want %s", sess["deployment_id"], deplID)
	}
	// deploymentBody carries one initial user.message, so the fired session is
	// born running, exactly as the same create through POST /v1/sessions is.
	if sess["status"] != "running" {
		t.Errorf("session.status = %v, want running", sess["status"])
	}
	// A fired session inherits no metadata and no title (plan 37 §8.1 entry
	// 24): metadata is the application layer's hook and the deployment's bag
	// is not copied into it.
	if meta, _ := sess["metadata"].(map[string]any); len(meta) != 0 {
		t.Errorf("session.metadata = %v, want empty", sess["metadata"])
	}
	if sess["title"] != "" {
		t.Errorf("session.title = %v, want unset", sess["title"])
	}

	// The success settlement is durable: succeeded_at is stamped beside the
	// session link (#520 — the link may go stale, the marker may not).
	var succeededAt *time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT succeeded_at FROM deployment_runs WHERE id = $1`, runID).Scan(&succeededAt); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if succeededAt == nil {
		t.Errorf("succeeded_at is null on a successful run")
	}

	// A manual run is attributed: the request is authenticated, so the fired
	// session's audit column names the caller's principal. Only a scheduled
	// fire — no caller at 09:00 — creates unattributed (plan §9).
	var createdBy *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT created_by FROM sessions WHERE id = $1`, sessionID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy == nil || *createdBy == "" {
		t.Errorf("created_by is unset on a manually fired session; the /run caller is its cause and the audit trail should name them")
	}

	// The action endpoint reads no request body: garbage is neither parsed
	// nor refused, per the reference's bodyless action params.
	status, res := s.do(http.MethodPost, "/v1/deployments/"+deplID+"/run", "{not json")
	if status != http.StatusOK {
		t.Errorf("run with a garbage body: status %d, body %v — the body must be ignored", status, res)
	}

	status, res = s.do(http.MethodPost, "/v1/deployments/depl_doesnotexist123/run", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")
}

// Archive is terminal for /run (400, §8.1 entry 11); pause is explicitly not
// ("manual runs through the run endpoint are still allowed while paused").
func TestDeploymentRunArchivedRefusesAndPausedAllows(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	archived := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d, body %v", status, res)
	}
	status, res := s.do(http.MethodPost, "/v1/deployments/"+archived+"/run", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "is archived") {
		t.Errorf("message %q does not say the deployment is archived", msg)
	}
	var runs int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM deployment_runs WHERE deployment_id = $1`, archived).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("an archived deployment recorded %d run rows; the refusal must precede the claim", runs)
	}

	paused := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+paused+"/pause", nil); status != http.StatusOK {
		t.Fatalf("pause: status %d, body %v", status, res)
	}
	run := runDeployment(t, s, paused)
	if run["session_id"] == nil {
		t.Errorf("run while paused settled without a session: %v", run)
	}
	status, after := s.do(http.MethodGet, "/v1/deployments/"+paused, nil)
	if status != http.StatusOK {
		t.Fatalf("get deployment: status %d", status)
	}
	if after["status"] != "paused" {
		t.Errorf("status = %v after a manual run; running while paused must not unpause", after["status"])
	}
}

// A classified failure is a 200 carrying an error-bearing run — the endpoint's
// only success shape is the run object, and its error member is where failure
// lives (§5.2). The half-made session is rolled back, and a manual run never
// pauses the deployment: "only a scheduled fire auto-pauses".
func TestDeploymentRunRecordsAClassifiedFailure(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// Each arm blocks on its own resource, so each deployment gets its own
	// live environment — an arm sharing the archived one would classify as
	// environment_archived_error before its own blocker was ever read.
	envB := createEnvironment(t, s, map[string]any{"name": "run-env-vault"})["id"].(string)
	envC := createEnvironment(t, s, map[string]any{"name": "run-env-store"})["id"].(string)

	// Arm 1: archived environment.
	dEnv := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	// Arm 2: archived vault.
	vaultID := createVault(t, s, "run-vault")
	bodyVault := deploymentBody(agentID, envB)
	bodyVault["vault_ids"] = []string{vaultID}
	dVault := createDeployment(t, s, bodyVault)["id"].(string)

	// Arm 3: archived memory store.
	storeID := createMemoryStore(t, s, "run-store")
	bodyStore := deploymentBody(agentID, envC)
	bodyStore["resources"] = []any{map[string]any{"type": "memory_store", "memory_store_id": storeID}}
	dStore := createDeployment(t, s, bodyStore)["id"].(string)

	for _, block := range []struct{ method, path string }{
		{http.MethodDelete, "/v1/environments/" + envID}, // refused by the FK — proves the arm needs archive, not delete
		{http.MethodPost, "/v1/environments/" + envID + "/archive"},
		{http.MethodPost, "/v1/vaults/" + vaultID + "/archive"},
		{http.MethodPost, "/v1/memory_stores/" + storeID + "/archive"},
	} {
		status, res := s.do(block.method, block.path, nil)
		if block.method == http.MethodDelete {
			if status != http.StatusBadRequest {
				t.Fatalf("%s %s: status %d, want the FK's 400 (%v)", block.method, block.path, status, res)
			}
			continue
		}
		if status != http.StatusOK {
			t.Fatalf("%s %s: status %d (%v)", block.method, block.path, status, res)
		}
	}

	for _, tc := range []struct {
		deployment, wantType, wantIn string
	}{
		{dEnv, "environment_archived_error", "is archived"},
		{dVault, "vault_archived_error", "is archived"},
		{dStore, "memory_store_archived_error", "is archived"},
	} {
		run := runDeployment(t, s, tc.deployment)
		if run["session_id"] != nil {
			t.Errorf("%s: session_id = %v, want null on failure", tc.wantType, run["session_id"])
		}
		re, _ := run["error"].(map[string]any)
		if re["type"] != tc.wantType {
			t.Errorf("error.type = %v, want %s (message %v)", re["type"], tc.wantType, re["message"])
		}
		if msg, _ := re["message"].(string); !strings.Contains(msg, tc.wantIn) {
			t.Errorf("error.message = %q, want it to say %q", msg, tc.wantIn)
		}

		status, after := s.do(http.MethodGet, "/v1/deployments/"+tc.deployment, nil)
		if status != http.StatusOK {
			t.Fatalf("get deployment: status %d", status)
		}
		if after["status"] != "active" {
			t.Errorf("%s: deployment status = %v; a failed manual run must not pause", tc.wantType, after["status"])
		}
	}

	// The half-made sessions were rolled back with the savepoint: nothing
	// carries a deployment reference, and the environment holds no session —
	// its own delete said so above by blaming only the deployments.
	var orphans int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE deployment_id IS NOT NULL`).Scan(&orphans); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d session rows survived a failed fire; the savepoint rollback must discard them", orphans)
	}
}

// The sessions-list deployment_id filter is real from this slice (it
// short-circuited to an empty page while no session could hold a reference):
// scoped to the deployment, keyset-paged under the filter, 200-with-empty for
// a shape-valid unknown id, 400 for a shape-invalid one — and the session
// create surface still refuses the key, POST /run being the only way in
// (§8.1 entry 17).
func TestSessionsListFiltersByDeployment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deplA := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	deplB := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	first := runDeployment(t, s, deplA)["session_id"].(string)
	second := runDeployment(t, s, deplA)["session_id"].(string)
	runDeployment(t, s, deplB)
	plain := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	if plain["deployment_id"] != nil {
		t.Errorf("a plain session renders deployment_id = %v, want an explicit null", plain["deployment_id"])
	}

	status, page := s.do(http.MethodGet, "/v1/sessions?deployment_id="+deplA, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered list: status %d, body %v", status, page)
	}
	got := listData(t, page)
	if len(got) != 2 {
		t.Fatalf("filtered list returned %d sessions, want the deployment's 2", len(got))
	}
	for _, row := range got {
		if row["deployment_id"] != deplA {
			t.Errorf("filtered row carries deployment_id %v, want %s", row["deployment_id"], deplA)
		}
	}

	// Newest-first under the filter, and the keyset cursor respects it.
	status, page = s.do(http.MethodGet, "/v1/sessions?deployment_id="+deplA+"&limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("filtered page 1: status %d", status)
	}
	got = listData(t, page)
	if len(got) != 1 || got[0]["id"] != second {
		t.Fatalf("page 1 = %v, want the newer session %s", got, second)
	}
	cursor, _ := page["next_page"].(string)
	if cursor == "" {
		t.Fatalf("next_page missing with one of two rows served")
	}
	status, page = s.do(http.MethodGet,
		"/v1/sessions?deployment_id="+deplA+"&limit=1&page="+url.QueryEscape(cursor), nil)
	if status != http.StatusOK {
		t.Fatalf("filtered page 2: status %d", status)
	}
	got = listData(t, page)
	if len(got) != 1 || got[0]["id"] != first {
		t.Fatalf("page 2 = %v, want the older session %s", got, first)
	}

	// "Filtering by a non-existent deployment_id returns 200 with empty data."
	// The token stays inside the Crockford alphabet (no i, l, o, u) so the
	// shape check passes and absence is what answers.
	status, page = s.do(http.MethodGet, "/v1/sessions?deployment_id=depl_absent123", nil)
	if status != http.StatusOK {
		t.Fatalf("unknown deployment filter: status %d, body %v", status, page)
	}
	if got := listData(t, page); len(got) != 0 {
		t.Errorf("unknown deployment filter returned %d rows, want 0", len(got))
	}

	status, res := s.do(http.MethodGet, "/v1/sessions?deployment_id=bogus", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// The one way a session gets its reference is POST /run: the create
	// surface has no deployment field and refuses the key.
	status, res = s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "deployment_id": deplA,
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// Deleting a fired session must not unsettle its run (#520): the success
// marker is succeeded_at, not the session link, so run history and a
// schedule's last_run_at stay true about a run that did start.
func TestDeletingTheSessionDoesNotUnsettleTheRun(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	body := deploymentBody(agentID, envID)
	body["schedule"] = map[string]any{"type": "cron", "expression": "0 9 * * *", "timezone": "UTC"}
	deplID := createDeployment(t, s, body)["id"].(string)

	run := runDeployment(t, s, deplID)
	runID := run["id"].(string)
	sessionID := run["session_id"].(string)

	// A scheduled sibling, inserted directly: the scheduler is slice 4, and
	// this test is about the read path last_run_at takes over settled rows.
	schedSession := runDeployment(t, s, deplID)["session_id"].(string)
	scheduledAt := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	var schedRunID string
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO deployment_runs (id, deployment_id, trigger_type, scheduled_at,
		    agent_id, agent_version, session_id, succeeded_at)
		 VALUES ('drun_testscheduled01', $1, 'schedule', $2, $3, 1, $4, now())
		 RETURNING id`,
		deplID, scheduledAt, agentID, schedSession).Scan(&schedRunID); err != nil {
		t.Fatalf("insert scheduled run fixture: %v", err)
	}

	lastRunAt := func() any {
		t.Helper()
		status, d := s.do(http.MethodGet, "/v1/deployments/"+deplID, nil)
		if status != http.StatusOK {
			t.Fatalf("get deployment: status %d", status)
		}
		return d["schedule"].(map[string]any)["last_run_at"]
	}
	before := lastRunAt()
	if before == nil {
		t.Fatalf("last_run_at is null with a settled scheduled run on record")
	}

	for _, id := range []string{sessionID, schedSession} {
		// A fired session is born running (the deployment's initial events)
		// and a running session refuses deletion; park it idle first — the
		// deletion rule is not what this test is about.
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE sessions SET status = 'idle' WHERE id = $1`, id); err != nil {
			t.Fatalf("park session %s idle: %v", id, err)
		}
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE session_threads SET status = 'idle' WHERE session_id = $1`, id); err != nil {
			t.Fatalf("park %s's threads idle: %v", id, err)
		}
		if status, res := s.do(http.MethodDelete, "/v1/sessions/"+id, nil); status != http.StatusOK {
			t.Fatalf("delete session %s: status %d, body %v", id, status, res)
		}
	}

	if after := lastRunAt(); after != before {
		t.Errorf("last_run_at moved %v -> %v when the session was deleted; it reports a run that did start", before, after)
	}
	var sessionRef *string
	var succeededAt *time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT session_id, succeeded_at FROM deployment_runs WHERE id = $1`, runID).
		Scan(&sessionRef, &succeededAt); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if sessionRef != nil {
		t.Errorf("session_id = %v after the delete, want the FK's null", *sessionRef)
	}
	if succeededAt == nil {
		t.Errorf("succeeded_at was lost with the session; the marker must be durable")
	}

	// The read surface renders the stale link honestly: session_id null AND
	// error null — the published "exactly one is non-null" cannot survive
	// session deletion, and a null error is what keeps the run a legible
	// success (#520; the docs/DIVERGENCES.md entry).
	status, got := s.do(http.MethodGet, "/v1/deployment_runs/"+runID, nil)
	if status != http.StatusOK {
		t.Fatalf("get run after session delete: status %d, body %v", status, got)
	}
	wantFields(t, got, "type", "id", "deployment_id", "trigger_context",
		"session_id", "error", "agent", "created_at")
	if got["session_id"] != nil {
		t.Errorf("session_id = %v, want null after the session's deletion", got["session_id"])
	}
	if got["error"] != nil {
		t.Errorf("error = %v, want null — the run is still a success", got["error"])
	}

	// And the has_error filter keys off the durable marker, not the stale
	// link: the run stays in the success set and out of the failure set.
	for _, c := range []struct {
		param string
		want  bool
	}{{"has_error=false", true}, {"has_error=true", false}} {
		status, res := s.do(http.MethodGet, "/v1/deployment_runs?"+c.param, nil)
		if status != http.StatusOK {
			t.Fatalf("list runs ?%s: status %d, body %v", c.param, status, res)
		}
		found := false
		for _, item := range res["data"].([]any) {
			if item.(map[string]any)["id"] == runID {
				found = true
			}
		}
		if found != c.want {
			t.Errorf("?%s finds the deleted-session run: %v, want %v", c.param, found, c.want)
		}
	}
}

// listRuns fetches GET /v1/deployment_runs with the given query and fails on a
// non-200.
func listRuns(t *testing.T, s *tserver, query string) map[string]any {
	t.Helper()
	path := "/v1/deployment_runs"
	if query != "" {
		path += "?" + query
	}
	status, res := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("list runs %q: status %d, body %v", query, status, res)
	}
	return res
}

// runIDs projects a list page to its data ids, in page order.
func runIDs(t *testing.T, res map[string]any) []string {
	t.Helper()
	data, ok := res["data"].([]any)
	if !ok {
		t.Fatalf("data is %T, want an array", res["data"])
	}
	ids := make([]string, 0, len(data))
	for _, item := range data {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	return ids
}

// The runs list: newest-first, keyset-paged, and each published filter — the
// deployment_id 200-empty rule included — against one seeded population of
// six runs across four deployments (three manual successes, one manual
// success elsewhere, one scheduled success, one manual failure).
func TestDeploymentRunsListFiltersAndPages(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	dA := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	var aRuns []string
	for range 3 {
		aRuns = append(aRuns, runDeployment(t, s, dA)["id"].(string))
	}
	dB := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	runDeployment(t, s, dB)

	// One scheduled fire, driven at a fixed instant (the slice-4 idiom).
	dC := createDeployment(t, s, scheduledBody(agentID, envID, "30 9 * * *", "UTC"))["id"].(string)
	setResumedAt(t, s, dC, time.Date(2026, 3, 12, 8, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 30, 30, 0, time.UTC)); err != nil {
		t.Fatalf("scheduler tick: %v", err)
	}
	scheduledRunID := scheduledRuns(t, s, dC)[0].id

	// One failed manual run: its own environment, archived before the fire.
	envFail := createEnvironment(t, s, map[string]any{"name": "runs-list-fail-env"})["id"].(string)
	dD := createDeployment(t, s, deploymentBody(agentID, envFail))["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/environments/"+envFail+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive environment: status %d, body %v", status, res)
	}
	failedRun := runDeployment(t, s, dD)
	if failedRun["error"] == nil {
		t.Fatalf("the archived-environment run settled without an error: %v", failedRun)
	}
	failedRunID := failedRun["id"].(string)

	// The list's published order, read back from the store it pages over.
	var expected []string
	rows, err := s.pool.Query(t.Context(),
		`SELECT id FROM deployment_runs ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 6 {
		t.Fatalf("seeded %d runs, want 6", len(expected))
	}

	// The whole list, in one page: created_at descending, next_page null.
	full := listRuns(t, s, "")
	wantFields(t, full, "data", "next_page")
	if got := runIDs(t, full); !slices.Equal(got, expected) {
		t.Errorf("list order = %v, want %v", got, expected)
	}
	if full["next_page"] != nil {
		t.Errorf("next_page = %v on a complete page, want null", full["next_page"])
	}

	// The scheduled run renders its occurrence in the list, not just the get.
	for _, item := range full["data"].([]any) {
		run := item.(map[string]any)
		if run["id"] != scheduledRunID {
			continue
		}
		tc := run["trigger_context"].(map[string]any)
		if tc["type"] != "schedule" || tc["scheduled_at"] != "2026-03-12T09:30:00Z" {
			t.Errorf("scheduled run's trigger_context = %v, want the 09:30 occurrence", tc)
		}
	}

	// Keyset walk at limit=2: three pages, no duplicate, no skip.
	var walked []string
	query := "limit=2"
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatalf("the cursor walk did not terminate: %v", walked)
		}
		res := listRuns(t, s, query)
		walked = append(walked, runIDs(t, res)...)
		next, _ := res["next_page"].(string)
		if next == "" {
			break
		}
		query = "limit=2&page=" + url.QueryEscape(next)
	}
	if !slices.Equal(walked, expected) {
		t.Errorf("cursor walk = %v, want %v", walked, expected)
	}

	// deployment_id: a real one isolates, an absent well-formed one is the
	// published 200-with-empty-data, a malformed one is a 400 (#135).
	got := runIDs(t, listRuns(t, s, "deployment_id="+dA))
	slices.Sort(got)
	want := slices.Clone(aRuns)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("deployment_id=%s = %v, want %v", dA, got, want)
	}
	if got := runIDs(t, listRuns(t, s, "deployment_id=depl_absent123")); len(got) != 0 {
		t.Errorf("a non-existent deployment_id returned %v, want empty data", got)
	}
	status, res := s.do(http.MethodGet, "/v1/deployment_runs?deployment_id=not-an-id", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// trigger_type: the enum filters, anything else is a 400.
	if got := runIDs(t, listRuns(t, s, "trigger_type=schedule")); !slices.Equal(got, []string{scheduledRunID}) {
		t.Errorf("trigger_type=schedule = %v, want %v", got, []string{scheduledRunID})
	}
	if got := runIDs(t, listRuns(t, s, "trigger_type=manual")); len(got) != 5 {
		t.Errorf("trigger_type=manual returned %d runs, want 5", len(got))
	}
	status, res = s.do(http.MethodGet, "/v1/deployment_runs?trigger_type=cron", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// has_error: true is the failure set, false the success set.
	if got := runIDs(t, listRuns(t, s, "has_error=true")); !slices.Equal(got, []string{failedRunID}) {
		t.Errorf("has_error=true = %v, want %v", got, []string{failedRunID})
	}
	if got := runIDs(t, listRuns(t, s, "has_error=false")); len(got) != 5 || slices.Contains(got, failedRunID) {
		t.Errorf("has_error=false = %v, want the 5 successes", got)
	}
	status, res = s.do(http.MethodGet, "/v1/deployment_runs?has_error=maybe", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// The four created_at comparators, split at a middle run's own rendered
	// timestamp: gte keeps it, gt drops it, lte/lt mirror.
	boundary := full["data"].([]any)[2].(map[string]any)["created_at"].(string)
	for _, c := range []struct {
		op   string
		want []string
	}{
		{"gte", expected[:3]}, {"gt", expected[:2]},
		{"lte", expected[2:]}, {"lt", expected[3:]},
	} {
		q := url.Values{"created_at[" + c.op + "]": {boundary}}.Encode()
		if got := runIDs(t, listRuns(t, s, q)); !slices.Equal(got, c.want) {
			t.Errorf("created_at[%s]=%s = %v, want %v", c.op, boundary, got, c.want)
		}
	}
	status, res = s.do(http.MethodGet, "/v1/deployment_runs?"+url.Values{"created_at[gte]": {"yesterday"}}.Encode(), nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// The published cap is 1000, not the shared 100 (§2.6): 500 is legal,
	// 1001 and 0 are not.
	listRuns(t, s, "limit=500")
	status, res = s.do(http.MethodGet, "/v1/deployment_runs?limit=1001", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, "/v1/deployment_runs?limit=0", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// The single read renders both settled arms: a manual success and a scheduled
// failure, plus the two not-found spellings.
func TestDeploymentRunReadRendersBothArms(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	dManual := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	posted := runDeployment(t, s, dManual)
	manualID := posted["id"].(string)

	status, run := s.do(http.MethodGet, "/v1/deployment_runs/"+manualID, nil)
	if status != http.StatusOK {
		t.Fatalf("get manual run: status %d, body %v", status, run)
	}
	wantFields(t, run, "type", "id", "deployment_id", "trigger_context",
		"session_id", "error", "agent", "created_at")
	if run["type"] != "deployment_run" || run["id"] != manualID || run["deployment_id"] != dManual {
		t.Errorf("identity fields = %v/%v/%v", run["type"], run["id"], run["deployment_id"])
	}
	if run["session_id"] != posted["session_id"] || run["error"] != nil {
		t.Errorf("success arm = (%v, %v), want (%v, null)", run["session_id"], run["error"], posted["session_id"])
	}
	tc := run["trigger_context"].(map[string]any)
	if tc["type"] != "manual" {
		t.Errorf("trigger_context.type = %v, want manual", tc["type"])
	}
	if _, ok := tc["scheduled_at"]; ok {
		t.Errorf("a manual context must not carry scheduled_at: %v", tc)
	}
	agent := run["agent"].(map[string]any)
	if agent["type"] != "agent" || agent["id"] != agentID || agent["version"] != float64(1) {
		t.Errorf("agent = %v, want the pinned reference", agent)
	}

	// A scheduled failure: the fire finds its environment archived.
	envSched := createEnvironment(t, s, map[string]any{"name": "runs-get-sched-env"})["id"].(string)
	dSched := createDeployment(t, s, scheduledBody(agentID, envSched, "30 9 * * *", "UTC"))["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/environments/"+envSched+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive environment: status %d, body %v", status, res)
	}
	setResumedAt(t, s, dSched, time.Date(2026, 3, 12, 8, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 30, 30, 0, time.UTC)); err != nil {
		t.Fatalf("scheduler tick: %v", err)
	}
	schedID := scheduledRuns(t, s, dSched)[0].id

	status, run = s.do(http.MethodGet, "/v1/deployment_runs/"+schedID, nil)
	if status != http.StatusOK {
		t.Fatalf("get scheduled run: status %d, body %v", status, run)
	}
	if run["session_id"] != nil {
		t.Errorf("session_id = %v on a failed run, want null", run["session_id"])
	}
	re, _ := run["error"].(map[string]any)
	if re["type"] != "environment_archived_error" || re["message"] == "" {
		t.Errorf("error = %v, want environment_archived_error with a message", run["error"])
	}
	tc = run["trigger_context"].(map[string]any)
	if tc["type"] != "schedule" || tc["scheduled_at"] != "2026-03-12T09:30:00Z" {
		t.Errorf("trigger_context = %v, want the 09:30 schedule occurrence", tc)
	}

	// Two not-found spellings: a well-formed id that names nothing reaches
	// the query's no-rows arm; a malformed one stops at checkID. Same 404.
	status, res := s.do(http.MethodGet, "/v1/deployment_runs/drun_absent123", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")
	status, res = s.do(http.MethodGet, "/v1/deployment_runs/not-an-id", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")
}
