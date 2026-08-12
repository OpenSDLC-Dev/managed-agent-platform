package identity_test

import (
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// TestGCPIAPPresetLiterals pins every value the gcp-iap preset supplies, as a
// literal. The preset is data an operator cannot override — the four variables
// that would change these are a boot error under this preset — so an edit to the
// header name or the key URL has to fail here rather than in a deployment.
//
// ConfigFromEnv is the exported path to the preset, and it is pure: the getenv
// function is the whole input.
func TestGCPIAPPresetLiterals(t *testing.T) {
	t.Parallel()
	const audience = "/projects/1/global/backendServices/2"
	env := map[string]string{
		"IDENTITY_MODE":           "trusted_proxy",
		"IDENTITY_PROXY_PRESET":   "gcp-iap",
		"IDENTITY_PROXY_AUDIENCE": audience,
		"IDENTITY_ROLE_MAP":       "platform-admins=admin",
	}
	cfg, err := identity.ConfigFromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	if got, want := cfg.AssertionHeader, "x-goog-iap-jwt-assertion"; got != want {
		t.Errorf("AssertionHeader = %q, want %q", got, want)
	}
	if got, want := cfg.Issuer, "https://cloud.google.com/iap"; got != want {
		t.Errorf("Issuer = %q, want %q", got, want)
	}
	if got, want := cfg.JWKSURL, "https://www.gstatic.com/iap/verify/public_key-jwk"; got != want {
		t.Errorf("JWKSURL = %q, want %q", got, want)
	}
	if want := []string{"ES256"}; !slices.Equal(cfg.Algorithms, want) {
		t.Errorf("Algorithms = %q, want %q", cfg.Algorithms, want)
	}
	// The audience is deliberately not part of the preset: the gstatic key set is
	// global across every Google Cloud customer, so this one value is the entire
	// tenant boundary and must come from the deployment.
	if got := cfg.Audience; got != audience {
		t.Errorf("Audience = %q, want the configured %q — the preset must supply no audience", got, audience)
	}
}
