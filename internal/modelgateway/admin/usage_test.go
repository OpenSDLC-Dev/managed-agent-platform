package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

func (e *env) record(u store.Usage) {
	e.t.Helper()
	if err := e.store.RecordUsage(context.Background(), u, false); err != nil {
		e.t.Fatal(err)
	}
}

func row(key, alias, dep string, tokens *store.Tokens) store.Usage {
	return store.Usage{RequestID: "req_x", APIKeyID: key, Model: alias, Alias: alias, DeploymentID: dep, CredentialID: "gwcred_1",
		Protocol: "anthropic", Endpoint: "messages", Status: 200, Tokens: tokens, Latency: 1234567 * time.Microsecond}
}

// The daily rollups answer by UTC day, filtered by key, alias or
// deployment, over the 30 days to today unless the caller names its span;
// a viewer may read them.
func TestDailyUsage(t *testing.T) {
	e := newEnv(t, true)
	e.record(row("key_a", "chat", "gwdep_1", &store.Tokens{Input: 10, Output: 5, CacheWrite: 2, CacheRead: 100}))
	e.record(row("key_a", "chat", "gwdep_1", &store.Tokens{Input: 1, Output: 1}))
	bad := row("key_b", "chat", "gwdep_2", nil)
	bad.Status, bad.ErrorType = 529, "overloaded_error"
	e.record(bad)
	// The default span is the 30 days ending today: 29 days ago is in it,
	// 30 is not.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO modelgateway.usage_daily (day, api_key_id, alias, deployment_id, requests)
		SELECT (now() AT TIME ZONE 'UTC')::date - n, 'key_c', 'chat', 'gwdep_9', 9 FROM unnest(ARRAY[29, 30, 40]) n`); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format(time.DateOnly)

	got := (reply{body: e.ok(e.do("GET", "/admin/v1/usage/daily", nil, map[string]string{"Authorization": "Bearer " + e.token("readers")}), "viewer reads")}).list()
	if len(got) != 3 || got[0]["day"] != time.Now().UTC().AddDate(0, 0, -29).Format(time.DateOnly) {
		t.Fatalf("the default span holds %v, want the rollup of 29 days ago and today's two", got)
	}
	got = got[1:]
	a := got[0]
	if a["day"] != today || a["api_key_id"] != "key_a" || a["alias"] != "chat" || a["deployment_id"] != "gwdep_1" ||
		a["requests"] != 2.0 || a["errors"] != 0.0 || a["input_tokens"] != 11.0 || a["output_tokens"] != 6.0 ||
		a["cache_write_tokens"] != 2.0 || a["cache_read_tokens"] != 100.0 || a["cost"] != 0.0 {
		t.Errorf("key_a's day reads %v", a)
	}
	if b := got[1]; b["api_key_id"] != "key_b" || b["errors"] != 1.0 || b["input_tokens"] != 0.0 {
		t.Errorf("key_b's day reads %v", b)
	}

	old := time.Now().UTC().AddDate(0, 0, -40).Format(time.DateOnly)
	for q, want := range map[string]int{
		"?api_key_id=key_b":                  1,
		"?deployment_id=gwdep_1":             1,
		"?alias=other":                       0,
		"?from=" + old + "&to=" + old:        1,
		"?from=" + old:                       5,
		"?to=" + old:                         1,
		"?from=" + old + "&api_key_id=key_b": 1,
	} {
		if n := len((reply{body: e.ok(e.admin("GET", "/admin/v1/usage/daily"+q, nil), q)}).list()); n != want {
			t.Errorf("%s: %d rows, want %d", q, n, want)
		}
	}
	for q, says := range map[string]string{
		"?from=yesterday":                "YYYY-MM-DD",
		"?from=2026-10-07&to=2026-10-01": "after",
		"?from=2025-01-01&to=2026-01-02": "366 days",
		"?api_key=key_a":                 "unknown query parameter",
		"?alias=a&alias=b":               "more than once",
		"?session_id=sesn_1":             "unknown query parameter",
	} {
		e.refused(e.admin("GET", "/admin/v1/usage/daily"+q, nil), http.StatusBadRequest, "invalid_request_error", says)
	}
	if r := e.admin("GET", "/admin/v1/usage/daily?from=2025-01-02&to=2026-01-02", nil); r.status != http.StatusOK {
		t.Errorf("a span of 366 days: %d %s", r.status, r.raw)
	}
}

// The ledger answers newest first, a page at a time, filtered by key,
// alias, deployment or session; what a request did not have is null.
func TestUsageRequests(t *testing.T) {
	e := newEnv(t, false)
	answered := row("key_a", "chat", "gwdep_1", &store.Tokens{Input: 10, Output: 5})
	answered.SessionID, answered.TTFT = "sesn_1", 2500*time.Microsecond
	e.record(answered)
	for i := range 4 {
		u := row(fmt.Sprintf("key_%d", i%2), "chat", "gwdep_2", nil)
		u.Status, u.ErrorType = 529, "overloaded_error"
		e.record(u)
	}

	first := e.ok(e.admin("GET", "/admin/v1/usage/requests?limit=2", nil), "first page")
	page := reply{body: first}.list()
	if len(page) != 2 || first["has_more"] != true {
		t.Fatalf("the first page is %v", first)
	}
	if n := page[0]["error_type"]; n != "overloaded_error" || page[0]["input_tokens"] != nil || page[0]["cost"] != nil ||
		page[0]["session_id"] != nil || page[0]["ttft_ms"] != nil || page[0]["latency_ms"] != 1234.567 {
		t.Errorf("a refused request reads %v", page[0])
	}
	var ids []float64
	before := ""
	for range 10 {
		b := e.ok(e.admin("GET", "/admin/v1/usage/requests?limit=2"+before, nil), "page")
		for _, r := range (reply{body: b}).list() {
			ids = append(ids, r["id"].(float64))
		}
		if b["has_more"] != true {
			break
		}
		before = fmt.Sprintf("&before=%d", int64(ids[len(ids)-1]))
	}
	if len(ids) != 5 || ids[0] <= ids[4] {
		t.Errorf("paged ids %v, want five newest first", ids)
	}

	got := reply{body: e.ok(e.admin("GET", "/admin/v1/usage/requests?session_id=sesn_1", nil), "by session")}.list()
	if len(got) != 1 {
		t.Fatalf("by session: %v", got)
	}
	a := got[0]
	if a["request_id"] != "req_x" || a["api_key_id"] != "key_a" || a["model"] != "chat" || a["alias"] != "chat" ||
		a["deployment_id"] != "gwdep_1" || a["credential_id"] != "gwcred_1" || a["session_id"] != "sesn_1" ||
		a["protocol"] != "anthropic" || a["endpoint"] != "messages" || a["status"] != 200.0 || a["error_type"] != nil ||
		a["input_tokens"] != 10.0 || a["output_tokens"] != 5.0 || a["cache_write_tokens"] != 0.0 || a["cache_read_tokens"] != 0.0 ||
		a["cost"] != 0.0 || a["ttft_ms"] != 2.5 || a["created_at"] == nil {
		t.Errorf("the answered request reads %v", a)
	}
	for q, want := range map[string]int{"?api_key_id=key_0": 2, "?deployment_id=gwdep_2&api_key_id=key_1": 2, "?alias=x": 0} {
		if n := len((reply{body: e.ok(e.admin("GET", "/admin/v1/usage/requests"+q, nil), q)}).list()); n != want {
			t.Errorf("%s: %d rows, want %d", q, n, want)
		}
	}
	for q, says := range map[string]string{
		"?limit=0":        "1 to 1000",
		"?limit=1001":     "1 to 1000",
		"?limit=x":        "1 to 1000",
		"?before=0":       "row id",
		"?before=abc":     "row id",
		"?from=x":         "unknown query parameter",
		"?session_id=%ZZ": "does not parse",
	} {
		e.refused(e.admin("GET", "/admin/v1/usage/requests"+q, nil), http.StatusBadRequest, "invalid_request_error", says)
	}
	e.refused(e.admin("POST", "/admin/v1/usage/requests", nil), http.StatusMethodNotAllowed, "invalid_request_error", "GET")
}
