package gateconfig_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
)

func TestFetchSendsTokenAndParses(t *testing.T) {
	var seen *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"networking": {"type": "limited", "allowed_hosts": ["api.example.com"]},
			"credentials": [{
				"credential_id": "vcrd_abc",
				"placeholder": "vltph_deadbeef",
				"secret": "s3cr3t",
				"networking": {"type": "limited", "allowed_hosts": ["api.example.com"]},
				"injection_location": {"header": true, "body": false}
			}]
		}`))
	}))
	t.Cleanup(srv.Close)

	c := gateconfig.NewClient(srv.URL, "gtk_the_token", srv.Client())
	cfg, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The gate token travels as a Bearer credential, on the fixed path.
	if got := seen.Header.Get("Authorization"); got != "Bearer gtk_the_token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer gtk_the_token")
	}
	if seen.URL.Path != gateconfig.Path {
		t.Errorf("path = %q, want %q", seen.URL.Path, gateconfig.Path)
	}

	if cfg.Networking.Type != domain.NetLimited ||
		len(cfg.Networking.AllowedHosts) != 1 || cfg.Networking.AllowedHosts[0] != "api.example.com" {
		t.Errorf("networking = %+v", cfg.Networking)
	}
	if len(cfg.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(cfg.Credentials))
	}
	cr := cfg.Credentials[0]
	if cr.CredentialID != "vcrd_abc" || cr.Placeholder != "vltph_deadbeef" || cr.Secret != "s3cr3t" {
		t.Errorf("credential fields = %+v", cr)
	}
	if cr.Networking.Type != domain.NetLimited || len(cr.Networking.AllowedHosts) != 1 {
		t.Errorf("credential networking = %+v", cr.Networking)
	}
	if !cr.InjectionLocation.Header || cr.InjectionLocation.Body {
		t.Errorf("injection_location = %+v, want header-only", cr.InjectionLocation)
	}
}

func TestFetchUnauthorizedIsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	t.Cleanup(srv.Close)

	c := gateconfig.NewClient(srv.URL, "gtk_x", srv.Client())
	_, err := c.Fetch(context.Background())
	if !errors.Is(err, gateconfig.ErrUnauthorized) {
		t.Fatalf("Fetch on 401 = %v, want ErrUnauthorized", err)
	}
}

func TestFetchOtherStatusIsPlainError(t *testing.T) {
	// A transient 503 must NOT read as ErrUnauthorized — the gate keeps its
	// last-known-good config on a transient failure, only fails closed on 401.
	const secretPage = "secret-proxy-diagnostics-should-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(secretPage))
	}))
	t.Cleanup(srv.Close)

	c := gateconfig.NewClient(srv.URL, "gtk_x", srv.Client())
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch on 503 returned nil error")
	}
	if errors.Is(err, gateconfig.ErrUnauthorized) {
		t.Errorf("503 mapped to ErrUnauthorized; a transient error must stay distinct")
	}
	if strings.Contains(err.Error(), secretPage) {
		t.Errorf("error echoed the response body: %v", err)
	}
}

func TestFetchTransportErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	c := gateconfig.NewClient(url, "gtk_x", srv.Client())
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch against a closed server returned nil error")
	}
}
