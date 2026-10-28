package login

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBrowserLoginConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		wantMode Mode
		wantErr  bool
	}{
		{"default", nil, ModeDisabled, false},
		{"legacy oidc", map[string]string{"IDENTITY_MODE": "oidc"}, ModeOIDC, false},
		{"legacy proxy", map[string]string{"IDENTITY_MODE": "trusted_proxy"}, ModeTrustedProxy, false},
		{"password needs development", map[string]string{"IDENTITY_MODE": "password", "CONSOLE_PUBLIC_ORIGIN": "https://console.example"}, "", true},
		{"password needs origin", map[string]string{"IDENTITY_MODE": "password", "DEVELOPMENT_MODE": "true"}, "", true},
		{"development password", map[string]string{"IDENTITY_MODE": "password", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": "http://localhost:3000"}, ModePassword, false},
		{"native oidc", map[string]string{"IDENTITY_MODE": "oidc", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_OIDC_CLIENT_ID": "console", "IDENTITY_OIDC_AUDIENCE": "console"}, ModeOIDC, false},
		{"native oidc needs client", map[string]string{"IDENTITY_MODE": "oidc", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_OIDC_AUDIENCE": "console"}, "", true},
		{"native oidc audience mismatch", map[string]string{"IDENTITY_MODE": "oidc", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_OIDC_CLIENT_ID": "console", "IDENTITY_OIDC_AUDIENCE": "other"}, "", true},
		{"native proxy", map[string]string{"IDENTITY_MODE": "trusted_proxy", "CONSOLE_PUBLIC_ORIGIN": "https://console.example"}, ModeTrustedProxy, false},
		{"unknown mode", map[string]string{"IDENTITY_MODE": "typo"}, "", true},
		{"invalid development bool", map[string]string{"IDENTITY_MODE": "password", "DEVELOPMENT_MODE": "maybe", "CONSOLE_PUBLIC_ORIGIN": "http://localhost:3000"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ConfigFromEnv(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("configuration error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cfg.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", cfg.Mode, tc.wantMode)
			}
		})
	}

	for _, origin := range []string{
		"https://console.example", "https://console.example:8443", "http://localhost:3000", "http://127.0.0.1:8080", "http://[::1]:8080",
	} {
		t.Run("origin "+origin, func(t *testing.T) {
			_, err := ConfigFromEnv(func(k string) string {
				return map[string]string{"IDENTITY_MODE": "trusted_proxy", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": origin}[k]
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, origin := range []string{
		"https://console.example/", "https://console.example/path", "https://console.example?", "https://console.example?q=sensitive", "https://console.example#", "https://console.example#fragment",
		"https://user:sensitive@console.example", "http://console.example", "http://localhost.example", "http://127.0.0.1.example", "ftp://console.example", "https://", "//console.example", "https://console.example:bad", "https://console.example:0", "https://console.example:65536", "https://console.example:", "https://[not-ip]", "https://[127.0.0.1]", "https://::1", " https://console.example", "https://console.example ",
	} {
		t.Run("bad origin "+origin, func(t *testing.T) {
			_, err := ConfigFromEnv(func(k string) string {
				return map[string]string{"IDENTITY_MODE": "trusted_proxy", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": origin}[k]
			})
			if err == nil {
				t.Fatalf("accepted non-origin %q", origin)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("configuration error discloses origin credentials: %v", err)
			}
		})
	}
	_, err := ConfigFromEnv(func(k string) string {
		return map[string]string{"IDENTITY_MODE": "trusted_proxy", "CONSOLE_PUBLIC_ORIGIN": "http://localhost:3000"}[k]
	})
	if err == nil {
		t.Fatal("loopback HTTP accepted without development opt-in")
	}
}

func TestBrowserLoginConfigurationSecretBoundaries(t *testing.T) {
	const secret = "fixture-client-secret-never-render"
	env := map[string]string{"IDENTITY_MODE": "oidc", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_OIDC_CLIENT_ID": "console", "IDENTITY_OIDC_AUDIENCE": "console", "IDENTITY_OIDC_CLIENT_SECRET": secret}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDCClientID != "console" || cfg.OIDCClientSecret != secret {
		t.Fatal("RP client configuration was not retained")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	xmlEncoded, err := xml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	yamlEncoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("configuration", "config", cfg)
	for _, rendered := range []string{string(encoded), string(xmlEncoded), string(yamlEncoded), fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), logs.String()} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("client secret disclosed: %s", rendered)
		}
	}
	_, err = ConfigFromEnv(func(k string) string {
		if k == "CONSOLE_PUBLIC_ORIGIN" {
			return "https://user:" + secret + "@console.example"
		}
		return env[k]
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe invalid-config error: %v", err)
	}
	for _, mode := range []string{"disabled", "oidc", "trusted_proxy", "password"} {
		t.Run("session-only "+mode, func(t *testing.T) {
			cfg, err := SessionConfigFromEnv(func(k string) string {
				if strings.HasPrefix(k, "IDENTITY_OIDC_") {
					t.Fatalf("session-only configuration read RP variable %s", k)
				}
				return map[string]string{"IDENTITY_MODE": mode, "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "DEVELOPMENT_MODE": "true"}[k]
			})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OIDCClientSecret != "" {
				t.Fatal("session-only config carries a secret")
			}
		})
	}
}
