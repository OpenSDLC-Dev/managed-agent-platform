package modelgateway_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

func TestModelLimitCoversAllOwnedKeys(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m")
	e.alias("fast", target(d, 0))
	e.alias("slow", target(d, 0))
	keys := []string{e.ownedKey("principal_dev", "user", "developer", "key_owned"), e.key(), e.ownedKey("principal_service", "service_account", "developer", "key_service")}
	rpm := int64(2)
	_, err := e.s.PutModelLimit(e.ctx, store.ModelLimit{Model: "fast", RPM: &rpm}, nil)
	e.must(err)
	e.start()
	e.clearOfAMinute()
	for i, key := range keys {
		res, raw := e.do("POST", "/anthropic/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
		want := 200
		if i == 2 {
			want = 429
		}
		if res.StatusCode != want || i == 2 && (res.Header.Get("Retry-After") == "" || !strings.Contains(string(raw), "requests_per_minute")) {
			t.Fatalf("key %d: %d %s", i, res.StatusCode, raw)
		}
	}
	res, raw := e.do("POST", "/v1/responses", `{"model":"fast","input":"hi"}`, map[string]string{"Authorization": "Bearer " + keys[0]})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Responses shares alias limit: %d %s", res.StatusCode, raw)
	}
	res, raw = e.do("POST", "/v1/messages", `{"model":"slow","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": keys[0]})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("another alias: %d %s", res.StatusCode, raw)
	}
}

func TestWildcardModelBudgetSharesTokensAcrossRequestedNames(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("*", target(e.deployment(p, "m"), 0))
	key := e.ownedKey("principal_dev", "user", "developer", "key_owned")
	output := int64(2)
	_, err := e.s.PutModelLimit(e.ctx, store.ModelLimit{Model: "*", OTPM: &output}, nil)
	e.must(err)
	e.start()
	e.clearOfAMinute()
	headers := map[string]string{"x-api-key": key}
	res, raw := e.do("POST", "/v1/messages", `{"model":"first-name","max_tokens":8,"messages":[]}`, headers)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first wildcard request: %d %s", res.StatusCode, raw)
	}
	res, raw = e.do("POST", "/anthropic/v1/messages", `{"model":"another-name","max_tokens":8,"messages":[]}`, headers)
	if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(raw), "output_tokens_per_minute") {
		t.Fatalf("wildcard token budget: %d %s", res.StatusCode, raw)
	}
	if got := len(up.recorded()); got != 1 {
		t.Fatalf("refused wildcard request reached upstream: %d requests", got)
	}
}
