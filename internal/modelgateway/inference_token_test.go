package modelgateway_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestDelegatedTokenCannotReachManagement(t *testing.T) {
	e := newEnv(t)
	_, item := e.delegated("legacy")
	adminCalls := 0
	e.start(func(cfg *modelgateway.Config) {
		cfg.Admin = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { adminCalls++; w.WriteHeader(200) })
	})
	for _, path := range []string{"/admin/v1/providers", "/admin/v1/rate_limits/fast", "/v1/sessions", "/unknown", "/openai/v1/messages", "/anthropic/v1/chat/completions"} {
		res, raw := e.do("GET", path, "", map[string]string{"Authorization": "Bearer " + item.InferenceToken})
		if res.StatusCode != 403 {
			t.Errorf("gateway scope %s: %d %s", path, res.StatusCode, raw)
		}
	}
	if adminCalls != 0 {
		t.Error("delegated token reached the admin handler")
	}
	control := api.NewHandler(e.pool, nil, nil, nil)
	for _, path := range []string{"/v1/agents", "/api/console/organizations/default/members", "/api/console/organizations/default/api_keys", "/v1/environments/env_x/work/poll", "/internal/v1/gate/config", "/v1/../v1/agents"} {
		for _, header := range []string{"x-api-key", "Authorization"} {
			r := httptest.NewRequest("GET", path, nil)
			value := item.InferenceToken
			if header == "Authorization" {
				value = "Bearer " + value
			}
			r.Header.Set(header, value)
			w := httptest.NewRecorder()
			control.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Errorf("control-plane scope %s %s: %d %s", header, path, w.Code, w.Body.String())
			}
		}
	}
}

func TestLegacyKeyWithInferencePrefixKeepsItsAuthority(t *testing.T) {
	e := newEnv(t)
	const key = "itk_operator_chose_this_legacy_value"
	_, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys(id,name,key_hash) VALUES ('legacy_prefix','legacy',$1)`, apikey.Hash(key))
	e.must(err)
	_, err = e.s.PutKeyPolicy(e.ctx, store.KeyPolicy{APIKeyID: "legacy_prefix"})
	e.must(err)
	e.start()
	res, raw := e.do("GET", "/v1/models", "", map[string]string{"x-api-key": key})
	if res.StatusCode != 200 {
		t.Fatalf("legacy value was mistaken for a delegated token: %d %s", res.StatusCode, raw)
	}
}

func (e *env) delegated(creator string) (*queue.Queue, *queue.Item) {
	e.t.Helper()
	sid, eid := pgtest.NewSession(e.t, e.pool, "cloud")
	_, err := e.pool.Exec(e.ctx, `UPDATE sessions SET created_by=$2,initiator_principal_id=NULL,initiator_api_key_id=NULL WHERE id=$1`, sid, creator)
	e.must(err)
	q := queue.New(e.pool)
	_, err = q.Enqueue(e.ctx, e.pool, eid, sid, queue.ModelTurn)
	e.must(err)
	item, err := q.Claim(e.ctx, queue.ModelTurn, time.Minute)
	e.must(err)
	if item == nil || item.InferenceToken == "" {
		e.t.Fatal("fixture failed to obtain inference token")
	}
	return q, item
}

func TestDelegatedInferenceUsesMembershipAndPrincipalUsage(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.ownedKey("principal_member", "user", "developer", "key_owner")
	_, err := e.pool.Exec(e.ctx, `UPDATE principals SET role='user' WHERE id='principal_member'`)
	e.must(err)
	q, item := e.delegated("key_owner")
	e.start()
	res, raw := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if res.StatusCode != 403 {
		t.Fatalf("demoted direct caller: %d %s", res.StatusCode, raw)
	}
	for _, prefix := range []string{"", "/anthropic", "/openai"} {
		res, raw = e.do("GET", prefix+"/v1/models", "", map[string]string{"Authorization": "Bearer " + item.InferenceToken})
		if res.StatusCode != 200 {
			t.Fatalf("delegated list: %d %s", res.StatusCode, raw)
		}
	}
	res, raw = e.do("POST", "/anthropic/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": item.InferenceToken, modelgateway.SessionHeader: "sesn_spoofed"})
	if res.StatusCode != 200 {
		t.Fatalf("delegated inference: %d %s", res.StatusCode, raw)
	}
	rows, _, err := e.s.ListUsage(e.ctx, store.UsageFilter{PrincipalID: "principal_member"}, 0, 10)
	e.must(err)
	if len(rows) != 1 || rows[0].APIKeyID != "" || rows[0].SessionID != string(item.SessionID) {
		t.Fatalf("delegated ledger: %+v", rows)
	}
	e.must(q.Complete(e.ctx, e.pool, item))
	res, raw = e.do("GET", "/v1/models", "", map[string]string{"Authorization": "Bearer " + item.InferenceToken})
	if res.StatusCode != 401 {
		t.Fatalf("completed token: %d %s", res.StatusCode, raw)
	}
}

func TestDelegatedCallsShareOrganizationBudgetAndRecheckMembership(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.ownedKey("principal_member", "service_account", "developer", "key_owner")
	_, item := e.delegated("key_owner")
	rpm := int64(1)
	_, err := e.s.PutModelLimit(e.ctx, store.ModelLimit{Model: "fast", RPM: &rpm}, nil)
	e.must(err)
	e.start()
	e.clearOfAMinute()
	res, raw := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if res.StatusCode != 200 {
		t.Fatalf("direct call: %d %s", res.StatusCode, raw)
	}
	res, raw = e.do("POST", "/anthropic/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"Authorization": "Bearer " + item.InferenceToken})
	if res.StatusCode != 429 {
		t.Fatalf("shared budget: %d %s", res.StatusCode, raw)
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE principals SET status='deactivated',deactivated_at=clock_timestamp() WHERE id='principal_member'`)
	e.must(err)
	res, raw = e.do("GET", "/v1/models", "", map[string]string{"x-api-key": item.InferenceToken})
	if res.StatusCode != 401 {
		t.Fatalf("archived initiator: %d %s", res.StatusCode, raw)
	}
}

func TestDelegatedInferenceRouteScope(t *testing.T) {
	e := newEnv(t)
	_, item := e.delegated("legacy")
	e.start()
	// A malformed body reaches each inference handler; stored responses retain
	// their documented stateless 404. Scope rejection would return 403 first.
	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages", "/v1/messages/count_tokens", "/anthropic/v1/messages/count_tokens", "/v1/chat/completions", "/openai/v1/chat/completions", "/v1/responses", "/openai/v1/responses", "/v1/embeddings", "/openai/v1/embeddings", "/v1/rerank"} {
		res, raw := e.do("POST", path, `{`, map[string]string{"x-api-key": item.InferenceToken})
		if res.StatusCode != 400 {
			t.Errorf("inference %s: %d %s", path, res.StatusCode, raw)
		}
	}
	for _, path := range []string{"/v1/responses/resp_x", "/openai/v1/responses/resp_x", "/v1/responses/resp_x/input_items"} {
		res, raw := e.do("GET", path, "", map[string]string{"Authorization": "Bearer " + item.InferenceToken})
		if res.StatusCode != 404 {
			t.Errorf("stored response %s: %d %s", path, res.StatusCode, raw)
		}
	}
}
