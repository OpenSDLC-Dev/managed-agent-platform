package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

const agentsOverviewPath = "/api/console/organizations/default/workspaces/default/agents/overview"

func TestAgentsWorkspaceOverview(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "owner", "admin", "console")
	c, se := s.issue(admin)
	adminHeaders := nativeHeaders(c, se.CSRFToken)
	s.key, _ = issueOwnedHTTP(t, s, adminHeaders, "report fixture", nil)
	dev := seedMember(t, s, "builder", "developer", "console")
	c, se = s.issue(dev)
	headers := nativeHeaders(c, se.CSRFToken)
	user := seedMember(t, s, "viewer", "user", "console")
	c, se = s.issue(user)
	expectManagement(t, s, "GET", agentsOverviewPath, "", nativeHeaders(c, se.CSRFToken), 403)
	expectManagement(t, s, "GET", agentsOverviewPath, "", http.Header{"X-Api-Key": {s.key}}, 401)
	expectManagement(t, s, "GET", agentsOverviewPath, "", nil, 401)
	expectManagement(t, s, "GET", strings.Replace(agentsOverviewPath, "/workspaces/default", "/workspaces/other", 1), "", headers, 404)
	expectManagement(t, s, "GET", strings.Replace(agentsOverviewPath, "/organizations/default", "/organizations/11111111-1111-4111-8111-111111111111", 1), "", headers, 401)
	expectManagement(t, s, "POST", agentsOverviewPath, "{}", headers, 405)
	expectManagement(t, s, "GET", agentsOverviewPath+"?sort=bogus", "", headers, 400)
	expectManagement(t, s, "GET", agentsOverviewPath+"?direction=invalid", "", headers, 400)
	type totals struct {
		Sessions      int
		ErrorRate     float64 `json:"error_rate"`
		Agents        int
		SessionChange *float64 `json:"session_change"`
		ErrorChange   *float64 `json:"error_change"`
	}
	type row struct {
		ID         string
		Name       string
		Sessions   int
		ErrorRate  float64  `json:"error_rate"`
		Median     *float64 `json:"median_seconds"`
		LastActive *string  `json:"last_active"`
		ArchivedAt *string  `json:"archived_at"`
	}
	type series struct {
		ID     *string
		Name   string
		Counts []int
	}
	read := func(q string) (out struct {
		Summary    totals
		Days       []string
		Series     []series
		ActiveTime []row `json:"active_time"`
		Agents     []row
		Total      int
	}) {
		t.Helper()
		if err := json.Unmarshal(expectManagement(t, s, "GET", agentsOverviewPath+q, "", headers, 200), &out); err != nil {
			t.Fatal(err)
		}
		return
	}
	empty := read("")
	if empty.Total != 0 || len(empty.Days) != 30 || empty.Agents == nil || empty.Series == nil || empty.ActiveTime == nil || empty.Summary.SessionChange != nil {
		t.Fatalf("empty=%+v", empty)
	}
	id, env := fixture(t, s.tserver)
	other := createAgent(t, s.tserver, map[string]any{"name": "A quiet agent", "model": "test"})["id"].(string)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	session := func(agent string, days int, seconds float64) string {
		sid := createSession(t, s.tserver, map[string]any{"agent": agent, "environment_id": env})["id"].(string)
		exec(`UPDATE sessions SET created_at=(now() AT TIME ZONE 'UTC')::date AT TIME ZONE 'UTC' - make_interval(days=>$2), active_seconds=$3 WHERE id=$1`, sid, days, seconds)
		return sid
	}
	first := session(id, 0, 10)
	second := session(id, 29, 30)
	// A rolling migration can leave a stale open clock on an idle session.
	exec("UPDATE sessions SET running_since = now() - interval '1 hour' WHERE id=$1", first)
	session(id, 30, 5)
	session(id, 59, 2)
	outsideWindow := session(id, 60, 300)
	secondSpace := secondWorkspace(t, s.tserver)
	foreignAgent, foreignEnv := fixture(t, secondSpace)
	foreign := createSession(t, secondSpace, map[string]any{"agent": foreignAgent, "environment_id": foreignEnv})["id"].(string)
	exec("UPDATE sessions SET active_seconds=800 WHERE id=$1", foreign)
	internal, _ := fixture(t, s.tserver)
	session(internal, 0, 900)
	exec("UPDATE agents SET internal=true WHERE id=$1", internal)
	exec("UPDATE agents SET archived_at=now() WHERE id=$1", id)
	event := func(sid string, seq int, reason string) {
		exec(`INSERT INTO events(id,session_id,seq,type,payload) VALUES ('overview-'||$1||'-'||$2::int,$1,$2,'session.status_idle',jsonb_build_object('stop_reason',jsonb_build_object('type',$3::text)))`, sid, seq, reason)
	}
	event(first, 50, "retries_exhausted")
	exec("INSERT INTO events(id,session_id,seq,type,payload) VALUES ('overview-restarted',$1,51,'session.status_running','{}')", first)
	event(first, 52, "end_turn")
	event(second, 50, "retries_exhausted")
	report := read("")
	if report.Total != 2 || report.Summary.Sessions != 2 || report.Summary.Agents != 1 || report.Summary.ErrorRate != 50 || report.Summary.SessionChange == nil || *report.Summary.SessionChange != 0 || report.Summary.ErrorChange != nil {
		t.Fatalf("report totals=%+v", report)
	}
	if len(report.Agents) != 2 || report.Agents[0].ID != id || report.Agents[0].LastActive == nil || report.Agents[0].ArchivedAt == nil || report.Agents[1].Sessions != 0 || report.Agents[1].LastActive != nil {
		t.Fatalf("rows=%+v", report.Agents)
	}
	if len(report.ActiveTime) != 1 || report.ActiveTime[0].Median == nil || *report.ActiveTime[0].Median != 20 || len(report.Series) != 1 || len(report.Series[0].Counts) != 30 || report.Series[0].Counts[0] != 1 || report.Series[0].Counts[29] != 1 {
		t.Fatalf("charts=%+v %+v", report.ActiveTime, report.Series)
	}
	clockStart := time.Now()
	exec("UPDATE sessions SET status='running', running_since=now() - interval '2 minutes' WHERE id=$1", first)
	live := read("")
	if median := *live.ActiveTime[0].Median; median < 79.9 || median > 80+time.Since(clockStart).Seconds()/2+0.1 {
		t.Fatalf("open interval median=%v", median)
	}
	if sorted := read("?sort=name&direction=asc"); sorted.Agents[0].ID != other {
		t.Fatalf("name sort=%+v", sorted.Agents)
	}
	if filtered := read("?agent_id=" + other); filtered.Total != 1 || filtered.Summary.Sessions != 0 || filtered.Agents[0].ID != other {
		t.Fatalf("agent filter=%+v", filtered)
	}
	for n := 0; n < 12; n++ {
		added := createAgent(t, s.tserver, map[string]any{"name": fmt.Sprintf("A report agent %02d", n), "model": "test"})["id"].(string)
		session(added, 0, float64(n))
	}
	ranked := read("?sort=name&direction=asc")
	if ranked.Total != 14 || len(ranked.Agents) != 10 || ranked.Agents[0].ID != other || len(ranked.ActiveTime) != 8 || ranked.ActiveTime[0].ID != id || len(ranked.Series) != 6 || ranked.Series[0].ID == nil || *ranked.Series[0].ID != id || ranked.Series[5].Name != "Other" || ranked.Series[5].ID != nil || ranked.Series[5].Counts[29] != 8 {
		t.Fatalf("ranked report=%+v", ranked)
	}
	// Previous-only activity has a count change, but no current error rate.
	oldFailure := session(other, 31, 0)
	event(oldFailure, 50, "retries_exhausted")
	prior := read("?agent_id=" + other)
	if prior.Summary.ErrorChange != nil || prior.Summary.SessionChange == nil || *prior.Summary.SessionChange != -100 {
		t.Fatalf("previous-only=%+v", prior.Summary)
	}
	for _, direction := range []string{"asc", "desc"} {
		sorted := read("?sort=error_rate&direction=" + direction)
		for _, row := range sorted.Agents {
			if row.Sessions == 0 {
				t.Fatalf("unmeasured rate displaced a sampled agent: %v", row)
			}
		}
	}
	exec("UPDATE sessions SET created_at=now()-interval '32 days' WHERE id=$1", outsideWindow)
	event(outsideWindow, 50, "retries_exhausted")
	// Three previous sessions for id, only one failure. Its current 50%
	// is a 50% relative increase over the previous one-third.
	changed := read("?agent_id=" + id)
	if changed.Summary.ErrorChange == nil || *changed.Summary.ErrorChange < 49.99 || *changed.Summary.ErrorChange > 50.01 {
		t.Fatalf("relative rate=%+v", changed.Summary)
	}
	child := insertChild(t, s.tserver, second, "idle")
	exec(`UPDATE session_threads SET stop_reason='{"type":"retries_exhausted"}' WHERE id=$1`, child)
	status, res := s.do("POST", "/v1/sessions/"+second+"/archive", nil)
	if status != 200 {
		t.Fatalf("archive failed child session: %d %v", status, res)
	}
	archived := read("?agent_id=" + id)
	if archived.Summary.ErrorRate != 50 {
		t.Fatalf("child cleanup changed the settled outcome: %+v", archived.Summary)
	}

	for _, bad := range []string{"agent_id=invalid", "agent_id=%zz", "agent_id=", "agent_id=" + id + "&agent_id=" + other, "direction=desc&direction=asc", "sort=name%00"} {
		expectManagement(t, s, "GET", agentsOverviewPath+"?"+bad, "", headers, 400)
	}

}
