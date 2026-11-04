package modelgateway_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
)

// Mounting cookie-enabled administration must never add a cookie lane to inference or model discovery.
func TestGatewayCookieNeverAuthenticatesInference(t *testing.T) {
	e := newEnv(t)
	manager := login.New(e.pool, login.Config{Mode: login.ModePassword, PublicOrigin: "https://console.example", Development: true})
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_browser','https://browser.example','browser','admin')`); err != nil {
		t.Fatal(err)
	}
	issued, err := manager.Issue(context.Background(), "principal_browser")
	e.must(err)
	e.start(func(cfg *modelgateway.Config) {
		prober, err := modelgateway.NewProber(*cfg)
		e.must(err)
		cfg.Admin, err = admin.New(admin.Config{Store: e.s, Cipher: e.cipher, BootstrapKey: bootstrap, Prober: prober, Login: manager})
		e.must(err)
	})
	h := map[string]string{"Cookie": "map_session=" + issued.Token}
	res, raw := e.do("GET", "/admin/v1/profiles", "", h)
	if res.StatusCode != 200 {
		t.Fatalf("cookie-enabled admin control=%d %s", res.StatusCode, raw)
	}
	for _, prefix := range []string{"", "/anthropic", "/openai"} {
		for _, route := range []struct{ method, path string }{
			{"POST", "/v1/messages"}, {"POST", "/v1/messages/count_tokens"}, {"POST", "/v1/chat/completions"},
			{"POST", "/v1/embeddings"}, {"POST", "/v1/rerank"}, {"POST", "/v1/responses"},
			{"GET", "/v1/responses/response_fixture"}, {"DELETE", "/v1/responses/response_fixture"},
			{"GET", "/v1/models"}, {"GET", "/v1/models/fixture"},
		} {
			t.Run(prefix+route.method+route.path, func(t *testing.T) {
				res, raw := e.do(route.method, prefix+route.path, `{}`, h)
				if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "" {
					t.Fatalf("inference took browser authority=%d %s marker=%q", res.StatusCode, raw, res.Header.Get("x-platform-signed-out"))
				}
			})
		}
	}
}
