package provider_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model_providers.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRoutes(t *testing.T) {
	t.Setenv("TEST_GW_KEY", "sk-from-env")
	path := writeConfig(t, `[
	  {"model": "claude-opus-4-8", "protocol": "anthropic", "base_url": "http://gw-a",
	   "upstream_model": "upstream-opus", "api_key": "sk-inline",
	   "headers": {"x-route": "pool-1"}, "stall_timeout": "90s", "max_tokens": 32768},
	  {"model": "*", "protocol": "anthropic", "base_url": "http://gw-default",
	   "api_key_env": "TEST_GW_KEY"}
	]`)

	routes, err := provider.LoadRoutes(path)
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d", len(routes))
	}
	a := routes[0]
	if a.Model != "claude-opus-4-8" || a.Config.Protocol != "anthropic" ||
		a.Config.BaseURL != "http://gw-a" || a.Config.Model != "upstream-opus" ||
		a.Config.APIKey != "sk-inline" || a.Config.Headers["x-route"] != "pool-1" {
		t.Errorf("route[0] = %+v", a)
	}
	if a.Config.StallTimeout != 90*time.Second {
		t.Errorf("stall_timeout = %s, want 90s", a.Config.StallTimeout)
	}
	if a.Config.MaxTokens != 32768 {
		t.Errorf("max_tokens = %d, want 32768", a.Config.MaxTokens)
	}
	if routes[1].Config.APIKey != "sk-from-env" {
		t.Errorf("api_key_env not resolved: %+v", routes[1].Config)
	}
	// Absent means "take the default", which the guard applies — not zero
	// meaning "no wait at all".
	if routes[1].Config.StallTimeout != 0 {
		t.Errorf("stall_timeout = %s on a route that set none, want the zero that means default", routes[1].Config.StallTimeout)
	}
	if routes[1].Config.MaxTokens != 0 {
		t.Errorf("max_tokens = %d on a route that set none, want the zero that means the adapter default", routes[1].Config.MaxTokens)
	}

	// The loaded routes construct a working registry.
	if _, err := provider.NewRegistry(routes, factories); err != nil {
		t.Errorf("NewRegistry over loaded routes: %v", err)
	}
}

func TestLoadRoutesValidation(t *testing.T) {
	cases := []struct {
		name, content string
	}{
		{"malformed JSON", `{not json`},
		{"not a list", `{"model":"m"}`},
		{"unknown key", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_keyy":"typo"}]`},
		{"both key forms", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","api_key_env":"B"}]`},
		{"unset env key", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key_env":"DEFINITELY_NOT_SET_XYZ"}]`},
		{"empty file", ``},
		{"unparsable stall_timeout", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","stall_timeout":"2 minutes"}]`},
		{"zero stall_timeout", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","stall_timeout":"0s"}]`},
		{"negative stall_timeout", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","stall_timeout":"-1m"}]`},
		{"zero max_tokens", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","max_tokens":0}]`},
		{"negative max_tokens", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a","max_tokens":-1}]`},
		{"trailing data", `[{"model":"m","protocol":"anthropic","base_url":"http://x","api_key":"a"}]
[{"model":"n","protocol":"anthropic","base_url":"http://y","api_key":"b"}]`},
	}
	for _, tc := range cases {
		path := writeConfig(t, tc.content)
		if _, err := provider.LoadRoutes(path); err == nil {
			t.Errorf("%s: LoadRoutes accepted it", tc.name)
		}
	}

	if _, err := provider.LoadRoutes(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: LoadRoutes accepted it")
	}
}
