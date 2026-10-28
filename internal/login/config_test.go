package login

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http/httptest"
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

// Accepted origins must already be in the form a browser sends. Requiring an
// operator correction avoids silently broadening the exact-origin comparison.
func TestCanonicalPublicOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://console.example", "https://console.example:8443", "https://xn--bcher-kva.example",
		"http://localhost", "http://localhost:3000", "http://127.0.0.1", "http://127.0.0.1:8080", "http://[::1]", "http://[::1]:8080",
		"https://192.0.2.1", "https://[2001:db8::1]", "https://[2001:db8::1]:8443", "https://[::c000:201]",
	} {
		t.Run("canonical "+origin, func(t *testing.T) {
			cfg, err := ConfigFromEnv(func(k string) string {
				return map[string]string{"IDENTITY_MODE": "password", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": origin}[k]
			})
			if err != nil {
				t.Fatal(err)
			}
			m := New(nil, cfg)
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.Header.Set("Origin", origin)
			if !m.NativeEnabled() || m.CheckOrigin(r) != nil {
				t.Fatal("canonical origin cannot log in")
			}
			r.Header.Set("Origin", "https://different.example")
			if m.CheckOrigin(r) == nil {
				t.Fatal("different origin was admitted")
			}
		})
	}
	for _, origin := range []string{
		"http://localhost:80", "https://CONSOLE.example:443", "https://CONSOLE.example", "HTTPS://console.example", "https://console.example:443",
		"http://localhost:080", "https://console.example:08443", "https://bücher.example", "https://127.1", "https://127.000.000.001", "https://0x7f000001", "https://2130706433",
		"https://console.123", "https://example.0x1", "https://console.example.", "https://xn--a.example", "https://[2001:DB8::1]", "https://[0:0:0:0:0:0:0:1]", "https://[::ffff:192.0.2.1]", "https://[::ffff:c000:201]",
		"https://" + strings.Repeat("a", 64) + ".example", "https://" + strings.Repeat("a.", 127) + "example",
		"https://sensitive:secret@CONSOLE.example:443",
	} {
		t.Run("noncanonical "+origin, func(t *testing.T) {
			_, err := ConfigFromEnv(func(k string) string {
				return map[string]string{"IDENTITY_MODE": "password", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": origin}[k]
			})
			if err == nil {
				t.Error("accepted an origin that is not supported browser serialization")
			}
			if err != nil && (strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret")) {
				t.Fatal("validation error exposed credentials")
			}
			m := New(nil, Config{Mode: ModePassword, PublicOrigin: origin, Development: true})
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.Header.Set("Origin", origin)
			if m.NativeEnabled() || m.CheckOrigin(r) == nil {
				t.Error("direct Manager accepted unusable native origin")
			}
		})
	}
	for _, mode := range []string{"disabled", "oidc", "trusted_proxy"} {
		t.Run("legacy "+mode, func(t *testing.T) {
			cfg, err := ConfigFromEnv(func(k string) string {
				if k == "IDENTITY_MODE" {
					return mode
				}
				return ""
			})
			if err != nil || New(nil, cfg).NativeEnabled() {
				t.Fatalf("verifier-only/disabled changed: %v", err)
			}
		})
	}
}

func TestBrowserLoginConfigurationDiagnosticFormatting(t *testing.T) {
	const secret = "FIX1-SYNTHETIC-CONFIG-CLIENT-SECRET"
	cfg := Config{Mode: ModeOIDC, PublicOrigin: "https://console.example", OIDCClientID: "console", OIDCClientSecret: secret}
	for _, form := range []struct {
		name  string
		value any
	}{
		{"value", cfg}, {"pointer", &cfg},
		{"nested value", struct{ Config Config }{cfg}},
		{"nested pointer", struct{ Config *Config }{&cfg}},
		{"slice", []any{cfg, &cfg}},
		{"map", map[string]any{"config": cfg, "pointer": &cfg}},
	} {
		t.Run(form.name, func(t *testing.T) {
			formats := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%t", "%f"}
			if form.name == "pointer" {
				formats = append(formats, "%p")
			}
			for _, format := range formats {
				output := fmt.Sprintf(format, form.value)
				for _, canary := range []string{secret, fmt.Sprintf("%x", secret), fmt.Sprintf("%X", secret)} {
					if strings.Contains(output, canary) {
						t.Errorf("%s of %s disclosed the synthetic client secret", format, form.name)
					}
				}
			}
		})
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
