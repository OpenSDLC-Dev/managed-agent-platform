package modelgateway_test

import (
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/inferencetoken"
)

func TestGatewayStartsWithoutPresetCredentials(t *testing.T) {
	e := newEnv(t)
	key := e.ownedKey("principal_human", "user", "developer", "owned")
	e.start()
	res, body := e.do("GET", "/v1/models", "", map[string]string{"x-api-key": key})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("owned models without presets: %d %s", res.StatusCode, body)
	}
}

func TestGatewayRefusesEveryUnownedKey(t *testing.T) {
	e := newEnv(t)
	granted := "sk-map-api01-historical-grant"
	pretendToken := inferencetoken.TokenPrefix + "historical-value"
	for id, raw := range map[string]string{"old_granted": granted, "old_preset": bootstrap, "old_token_shape": pretendToken} {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys(id,name,key_hash) VALUES ($1,$1,$2)`, id, apikey.Hash(raw))
		e.must(err)
	}
	_, err := e.pool.Exec(e.ctx, "INSERT INTO modelgateway.key_policies (api_key_id) VALUES ('old_granted')")
	e.must(err)
	e.start()
	for _, raw := range []string{bootstrap, granted, pretendToken} {
		for _, headers := range []map[string]string{{"x-api-key": raw}, {"Authorization": "Bearer " + raw}} {
			for _, path := range []string{"/v1/models", "/v1/messages", "/v1/responses", "/v1/embeddings"} {
				res, body := e.do("POST", path, `{"model":"fast"}`, headers)
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("unowned %s: %d %s", path, res.StatusCode, body)
				}
			}
		}
	}
}
