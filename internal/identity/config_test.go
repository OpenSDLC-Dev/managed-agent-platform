package identity_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	identity "github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// The IDENTITY_* names are spelled out here rather than shared with config.go:
// these are the strings an operator types into a deployment, so a rename has to
// be a deliberate edit on both sides rather than a constant renamed once.
const (
	configXVarMode          = "IDENTITY_MODE"
	configXVarOIDCIssuer    = "IDENTITY_OIDC_ISSUER"
	configXVarOIDCAudience  = "IDENTITY_OIDC_AUDIENCE"
	configXVarOIDCJWKSURL   = "IDENTITY_OIDC_JWKS_URL"
	configXVarProxyPreset   = "IDENTITY_PROXY_PRESET"
	configXVarProxyHeader   = "IDENTITY_PROXY_HEADER"
	configXVarProxyIssuer   = "IDENTITY_PROXY_ISSUER"
	configXVarProxyAudience = "IDENTITY_PROXY_AUDIENCE"
	configXVarProxyKeysURL  = "IDENTITY_PROXY_KEYS_URL"
	configXVarProxyAlgs     = "IDENTITY_PROXY_ALGS"
	configXVarClaimRoles    = "IDENTITY_CLAIM_ROLES"
	configXVarClaimEmail    = "IDENTITY_CLAIM_EMAIL"
	configXVarClaimName     = "IDENTITY_CLAIM_NAME"
	configXVarRoleMap       = "IDENTITY_ROLE_MAP"
)

// configXAllVars is every name this package reads, for the one test that has to
// clear the real process environment.
var configXAllVars = []string{
	configXVarMode, configXVarOIDCIssuer, configXVarOIDCAudience, configXVarOIDCJWKSURL,
	configXVarProxyPreset, configXVarProxyHeader, configXVarProxyIssuer,
	configXVarProxyAudience, configXVarProxyKeysURL, configXVarProxyAlgs,
	configXVarClaimRoles, configXVarClaimEmail, configXVarClaimName, configXVarRoleMap,
}

// configXAllAlgorithms is the settled allowlist, written out as literals so
// widening it quietly fails a test rather than a security review.
var configXAllAlgorithms = []string{"RS256", "RS512", "ES256", "ES384", "ES512"}

// configXEnv turns a map into the getenv ConfigFromEnv reads. An absent key and a
// key whose value is "" are the same thing here, exactly as os.Getenv cannot tell
// them apart — which is why every "missing variable" row below sets "" rather
// than deleting the key.
func configXEnv(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

// configXRecorder is configXEnv plus a log of the names looked up, so "no other
// variable is read" can be asserted mechanically and not only by its effects.
// ConfigFromEnv is called from one goroutine per row, so no lock is needed.
type configXRecorder struct {
	env  map[string]string
	read []string
}

func (r *configXRecorder) getenv(name string) string {
	r.read = append(r.read, name)
	return r.env[name]
}

// configXOIDC returns a minimal valid IDENTITY_MODE=oidc environment, with over
// applied on top. A fresh map every call, so a row may mutate its own copy.
func configXOIDC(over map[string]string) map[string]string {
	m := map[string]string{
		configXVarMode:         "oidc",
		configXVarOIDCIssuer:   "https://idp.example",
		configXVarOIDCAudience: "console",
		configXVarRoleMap:      "platform-admins=admin",
	}
	maps.Copy(m, over)
	return m
}

// configXCustom returns a minimal valid trusted_proxy/custom environment.
func configXCustom(over map[string]string) map[string]string {
	m := map[string]string{
		configXVarMode:          "trusted_proxy",
		configXVarProxyPreset:   "custom",
		configXVarProxyHeader:   "x-proxy-assertion",
		configXVarProxyIssuer:   "https://proxy.example",
		configXVarProxyAudience: "console",
		configXVarProxyKeysURL:  "https://proxy.example/keys",
		configXVarRoleMap:       "eng=developer",
	}
	maps.Copy(m, over)
	return m
}

// configXIAP returns a minimal valid trusted_proxy/gcp-iap environment. The
// audience is the only variable the preset does not supply, and it is required.
func configXIAP(over map[string]string) map[string]string {
	m := map[string]string{
		configXVarMode:          "trusted_proxy",
		configXVarProxyPreset:   "gcp-iap",
		configXVarProxyAudience: "/projects/1/global/backendServices/2",
		configXVarRoleMap:       "eng=developer",
	}
	maps.Copy(m, over)
	return m
}

// configXWantErr fails when err is nil, and when its text does not name every
// fragment the operator needs to fix the deployment. These are boot errors
// printed to a console, so naming the defective variable is the whole point —
// the uniform-message rule belongs to Verify, not here.
func configXWantErr(t *testing.T, err error, mentions ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error naming %v, got nil", mentions)
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error %q does not mention %q", err, m)
		}
	}
}

// configXWantDistinct fails when two rows produced the same error text: a table
// of separate rejection rules that all render identically is a table where one
// rule could be missing and nothing would notice.
func configXWantDistinct(t *testing.T, msgs map[string]string) {
	t.Helper()
	seen := make(map[string]string, len(msgs))
	for row, msg := range msgs {
		if other, dup := seen[msg]; dup {
			t.Errorf("rows %q and %q produce the same error %q", other, row, msg)
			continue
		}
		seen[msg] = row
	}
}

func TestConfigFromEnvDisabled(t *testing.T) {
	t.Parallel()
	// Each of these would be a boot error if it were read: an ftp issuer, an
	// unknown preset, a role-map pair with no '='. So "no other variable is read"
	// is asserted by its effect (a nil error) as well as by the recorder.
	poison := map[string]string{
		configXVarOIDCIssuer:  "ftp://idp.example",
		configXVarProxyPreset: "aws-alb",
		configXVarRoleMap:     "eng",
	}
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "unset", mode: ""},
		{name: "explicitly disabled", mode: "disabled"},
		{name: "surrounding whitespace is trimmed", mode: "  disabled  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := maps.Clone(poison)
			env[configXVarMode] = tc.mode
			rec := &configXRecorder{env: env}

			cfg, err := identity.ConfigFromEnv(rec.getenv)
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s=%q): %v", configXVarMode, tc.mode, err)
			}
			if cfg.Mode != identity.ModeDisabled {
				t.Errorf("Mode = %q, want %q", cfg.Mode, identity.ModeDisabled)
			}
			configXWantUnconfigured(t, cfg)
			// A staged rollout places the configuration first and flips the mode
			// second, which only works if the disabled mode reads nothing else.
			if !slices.Equal(rec.read, []string{configXVarMode}) {
				t.Errorf("read %v, want only %s", rec.read, configXVarMode)
			}
		})
	}
}

// configXWantUnconfigured fails when any field beyond Mode carries a value.
func configXWantUnconfigured(t *testing.T, cfg identity.Config) {
	t.Helper()
	for _, f := range []struct {
		name string
		got  string
	}{
		{"Issuer", cfg.Issuer},
		{"Audience", cfg.Audience},
		{"JWKSURL", cfg.JWKSURL},
		{"AssertionHeader", cfg.AssertionHeader},
		{"RolesClaim", cfg.RolesClaim},
		{"EmailClaim", cfg.EmailClaim},
		{"NameClaim", cfg.NameClaim},
	} {
		if f.got != "" {
			t.Errorf("%s = %q, want empty", f.name, f.got)
		}
	}
	if len(cfg.Algorithms) != 0 {
		t.Errorf("Algorithms = %v, want empty", cfg.Algorithms)
	}
	if len(cfg.RoleMap) != 0 {
		t.Errorf("RoleMap = %v, want empty", cfg.RoleMap)
	}
}

func TestConfigFromEnvUnknownMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"bogus", "OIDC", "Disabled", "trusted-proxy", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(map[string]string{configXVarMode: mode}))
			configXWantErr(t, err, configXVarMode, mode, "disabled", "oidc", "trusted_proxy")
			// A mistyped mode must not fall back to disabled: that would turn a
			// typo into silently unauthenticated operation.
			if cfg.Mode != "" {
				t.Errorf("Mode = %q alongside an error, want the zero Config", cfg.Mode)
			}
		})
	}
}

func TestConfigFromEnvOIDCDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(nil)))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Mode != identity.ModeOIDC {
		t.Errorf("Mode = %q, want %q", cfg.Mode, identity.ModeOIDC)
	}
	if cfg.Issuer != "https://idp.example" {
		t.Errorf("Issuer = %q", cfg.Issuer)
	}
	if cfg.Audience != "console" {
		t.Errorf("Audience = %q", cfg.Audience)
	}
	// Unset, so discovery runs at New rather than a pinned key URL being used.
	if cfg.JWKSURL != "" {
		t.Errorf("JWKSURL = %q, want empty so discovery runs", cfg.JWKSURL)
	}
	// oidc mode owns no header: the credential arrives as a bearer token, and a
	// trusted-proxy assertion header would be an unauthenticated lane.
	if cfg.AssertionHeader != "" {
		t.Errorf("AssertionHeader = %q, want empty in oidc mode", cfg.AssertionHeader)
	}
	if cfg.RolesClaim != "roles" || cfg.EmailClaim != "email" || cfg.NameClaim != "name" {
		t.Errorf("claim defaults = %q/%q/%q, want roles/email/name",
			cfg.RolesClaim, cfg.EmailClaim, cfg.NameClaim)
	}
	if want := map[string]identity.Role{"platform-admins": identity.RoleAdmin}; !maps.Equal(cfg.RoleMap, want) {
		t.Errorf("RoleMap = %v, want %v", cfg.RoleMap, want)
	}
	// Mode A has no allowlist override — the five-algorithm default is its whole
	// contract — but the parsed Config still carries the five, so reading a
	// Config tells you the allowlist without also knowing New's defaulting rule.
	// Mode B custom materialises the same five when _ALGS is unset, which
	// TestConfigFromEnvProxyCustom pins.
	if !slices.Equal(cfg.Algorithms, configXAllAlgorithms) {
		t.Errorf("Algorithms = %v, want all five %v", cfg.Algorithms, configXAllAlgorithms)
	}
	// ConfigFromEnv is a pure parse: the client and the clock are New's to fill in.
	if cfg.HTTPClient != nil {
		t.Error("HTTPClient is set; ConfigFromEnv must not choose a client")
	}
	if cfg.Now != nil {
		t.Error("Now is set; ConfigFromEnv must not choose a clock")
	}
}

func TestConfigFromEnvOIDCRequired(t *testing.T) {
	t.Parallel()
	msgs := map[string]string{}
	for _, tc := range []struct {
		name    string
		missing string
	}{
		{name: "no issuer", missing: configXVarOIDCIssuer},
		{name: "no audience", missing: configXVarOIDCAudience},
		{name: "no role map", missing: configXVarRoleMap},
	} {
		_, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{tc.missing: ""})))
		if err == nil {
			t.Errorf("%s: ConfigFromEnv accepted a config with no %s", tc.name, tc.missing)
			continue
		}
		if !strings.Contains(err.Error(), tc.missing) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.missing)
		}
		msgs[tc.name] = err.Error()
	}
	configXWantDistinct(t, msgs)
}

func TestConfigFromEnvJWKSURLOverride(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		url  string
		want string // "" with wantErr false means "the field stays empty"
		err  bool
	}{
		{name: "https url is pinned, skipping discovery", url: "https://idp.example/keys", want: "https://idp.example/keys"},
		{name: "http to loopback is the test exception", url: "http://127.0.0.1:9/keys", want: "http://127.0.0.1:9/keys"},
		{name: "unset leaves discovery to run", url: ""},
		{name: "http to a non-loopback host", url: "http://idp.example/keys", err: true},
		{name: "scheme that is neither", url: "ftp://idp.example/keys", err: true},
		{name: "credentials smuggled into the key url", url: "https://u:p@idp.example/keys", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
				configXVarOIDCJWKSURL: tc.url,
			})))
			if tc.err {
				configXWantErr(t, err, configXVarOIDCJWKSURL)
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s=%q): %v", configXVarOIDCJWKSURL, tc.url, err)
			}
			if cfg.JWKSURL != tc.want {
				t.Errorf("JWKSURL = %q, want %q", cfg.JWKSURL, tc.want)
			}
		})
	}
}

func TestConfigFromEnvIssuerScheme(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		issuer string
		err    bool
	}{
		{name: "https", issuer: "https://idp.example"},
		{name: "https with a port and a path", issuer: "https://idp.example:8443/realms/console"},
		{name: "http to 127.0.0.1", issuer: "http://127.0.0.1:9/"},
		{name: "http to localhost", issuer: "http://localhost:9"},
		{name: "http to ::1", issuer: "http://[::1]:9"},
		{name: "http to a non-loopback host", issuer: "http://idp.example", err: true},
		{name: "another scheme entirely", issuer: "ftp://x", err: true},
		{name: "not a url", issuer: "not a url", err: true},
		{name: "empty", issuer: "", err: true},
		{name: "no host", issuer: "https://", err: true},
		{name: "credentials in the url", issuer: "https://u:p@h/", err: true},
		// An OIDC issuer identifier carries neither (Discovery §2). Both would
		// also break the discovery URL, which is built by appending to this.
		{name: "query", issuer: "https://idp.example?tenant=acme", err: true},
		{name: "empty query", issuer: "https://idp.example?", err: true},
		{name: "fragment", issuer: "https://idp.example#f", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
				configXVarOIDCIssuer: tc.issuer,
			})))
			if tc.err {
				configXWantErr(t, err, configXVarOIDCIssuer)
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s=%q): %v", configXVarOIDCIssuer, tc.issuer, err)
			}
			// Stored exactly as configured: iss is compared byte for byte at
			// verification time, so normalising here would move the comparison.
			if cfg.Issuer != tc.issuer {
				t.Errorf("Issuer = %q, want %q verbatim", cfg.Issuer, tc.issuer)
			}
		})
	}
}

func TestConfigFromEnvIssuerTrailingSlashAccepted(t *testing.T) {
	t.Parallel()
	// A trailing slash is not a configuration error: an OP whose real iss ends in
	// '/' must stay configurable, and the discovery URL trims the slash before
	// appending the well-known path rather than the config normalising the issuer.
	const issuer = "https://idp.example/"
	cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
		configXVarOIDCIssuer: issuer,
	})))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Issuer != issuer {
		t.Errorf("Issuer = %q, want %q verbatim", cfg.Issuer, issuer)
	}
}

func TestConfigFromEnvProxyPresetRequired(t *testing.T) {
	t.Parallel()
	for _, preset := range []string{"", "aws-alb", "gcp_iap", "Custom"} {
		t.Run("preset "+preset, func(t *testing.T) {
			t.Parallel()
			_, err := identity.ConfigFromEnv(configXEnv(configXCustom(map[string]string{
				configXVarProxyPreset: preset,
			})))
			// aws-alb gets this error rather than a preset of its own: ALB's
			// assertion is not JWKS-shaped, so it needs a second key source, not
			// a table entry.
			configXWantErr(t, err, configXVarProxyPreset, "gcp-iap", "custom")
		})
	}
}

func TestConfigFromEnvGCPIAP(t *testing.T) {
	t.Parallel()

	t.Run("resolves to the preset literals", func(t *testing.T) {
		t.Parallel()
		cfg, err := identity.ConfigFromEnv(configXEnv(configXIAP(nil)))
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Mode != identity.ModeTrustedProxy {
			t.Errorf("Mode = %q, want %q", cfg.Mode, identity.ModeTrustedProxy)
		}
		if cfg.AssertionHeader != "x-goog-iap-jwt-assertion" {
			t.Errorf("AssertionHeader = %q, want x-goog-iap-jwt-assertion", cfg.AssertionHeader)
		}
		if cfg.Issuer != "https://cloud.google.com/iap" {
			t.Errorf("Issuer = %q, want https://cloud.google.com/iap", cfg.Issuer)
		}
		if cfg.JWKSURL != "https://www.gstatic.com/iap/verify/public_key-jwk" {
			t.Errorf("JWKSURL = %q, want the gstatic JWK Set", cfg.JWKSURL)
		}
		if want := []string{"ES256"}; !slices.Equal(cfg.Algorithms, want) {
			t.Errorf("Algorithms = %v, want %v", cfg.Algorithms, want)
		}
		if cfg.Audience != "/projects/1/global/backendServices/2" {
			t.Errorf("Audience = %q", cfg.Audience)
		}
	})

	for _, tc := range []struct {
		name     string
		audience string
		err      bool
	}{
		{name: "backend service audience", audience: "/projects/1/global/backendServices/2"},
		// IAP also mints App Engine audiences, so the shape check is loose on
		// purpose: a tighter regexp would refuse a valid deployment.
		{name: "app engine audience", audience: "/projects/1/apps/x"},
		{name: "missing", audience: "", err: true},
		{name: "not an IAP audience", audience: "console", err: true},
		{name: "no leading slash", audience: "projects/1/global/backendServices/2", err: true},
	} {
		t.Run("audience "+tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(configXIAP(map[string]string{
				configXVarProxyAudience: tc.audience,
			})))
			if tc.err {
				// The gstatic key set is global across every Google Cloud
				// customer, so this audience is the entire tenant boundary: an
				// empty or wrong one is cross-customer authentication, not a
				// cosmetic misconfiguration.
				configXWantErr(t, err, configXVarProxyAudience)
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(audience %q): %v", tc.audience, err)
			}
			if cfg.Audience != tc.audience {
				t.Errorf("Audience = %q, want %q", cfg.Audience, tc.audience)
			}
		})
	}
}

func TestConfigFromEnvGCPIAPRefusesOverrides(t *testing.T) {
	t.Parallel()
	// These four variables ARE the verification parameters — the issuer and the
	// key source — so an operator's attempt to change one is a boot error rather
	// than a silently ignored variable.
	msgs := map[string]string{}
	for _, tc := range []struct {
		variable string
		value    string
	}{
		{variable: configXVarProxyHeader, value: "x-my-assertion"},
		{variable: configXVarProxyIssuer, value: "https://evil.example"},
		{variable: configXVarProxyKeysURL, value: "https://evil.example/keys"},
		{variable: configXVarProxyAlgs, value: "RS256"},
		// Even an override that agrees with the preset is refused: the rule is
		// "the preset owns these", not "the preset wins".
		{variable: configXVarProxyHeader, value: "x-goog-iap-jwt-assertion"},
	} {
		_, err := identity.ConfigFromEnv(configXEnv(configXIAP(map[string]string{
			tc.variable: tc.value,
		})))
		if err == nil {
			t.Errorf("gcp-iap accepted %s=%q", tc.variable, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.variable) {
			t.Errorf("error %q does not name %s", err, tc.variable)
		}
		if !strings.Contains(err.Error(), "gcp-iap") {
			t.Errorf("error %q does not name the preset", err)
		}
		msgs[tc.variable+"="+tc.value] = err.Error()
	}
	// The two rows for the same variable share a message by construction; the
	// four distinct variables must not.
	delete(msgs, configXVarProxyHeader+"=x-goog-iap-jwt-assertion")
	configXWantDistinct(t, msgs)
}

func TestConfigFromEnvProxyCustom(t *testing.T) {
	t.Parallel()

	t.Run("every field lands", func(t *testing.T) {
		t.Parallel()
		cfg, err := identity.ConfigFromEnv(configXEnv(configXCustom(nil)))
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Mode != identity.ModeTrustedProxy {
			t.Errorf("Mode = %q, want %q", cfg.Mode, identity.ModeTrustedProxy)
		}
		if cfg.AssertionHeader != "x-proxy-assertion" {
			t.Errorf("AssertionHeader = %q", cfg.AssertionHeader)
		}
		if cfg.Issuer != "https://proxy.example" {
			t.Errorf("Issuer = %q", cfg.Issuer)
		}
		if cfg.Audience != "console" {
			t.Errorf("Audience = %q", cfg.Audience)
		}
		// Pinned, so discovery never runs in this mode.
		if cfg.JWKSURL != "https://proxy.example/keys" {
			t.Errorf("JWKSURL = %q", cfg.JWKSURL)
		}
		// _ALGS is the one optional variable here precisely because it is the one
		// with a safe default.
		if !slices.Equal(cfg.Algorithms, configXAllAlgorithms) {
			t.Errorf("Algorithms = %v, want all five %v", cfg.Algorithms, configXAllAlgorithms)
		}
	})

	msgs := map[string]string{}
	for _, missing := range []string{
		configXVarProxyHeader,
		configXVarProxyIssuer,
		configXVarProxyAudience,
		configXVarProxyKeysURL,
	} {
		_, err := identity.ConfigFromEnv(configXEnv(configXCustom(map[string]string{missing: ""})))
		if err == nil {
			t.Errorf("custom preset accepted a config with no %s", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name %s", err, missing)
		}
		msgs[missing] = err.Error()
	}
	configXWantDistinct(t, msgs)
}

func TestConfigFromEnvProxyAlgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		algs string
		want []string
		err  bool
	}{
		{name: "a subset, in the order given", algs: "ES256,RS512", want: []string{"ES256", "RS512"}},
		{name: "whitespace around elements", algs: " RS256 , ES384 ", want: []string{"RS256", "ES384"}},
		{name: "one element", algs: "ES256", want: []string{"ES256"}},
		// getenv cannot tell unset from set-empty, so a whole value that is empty
		// is ABSENCE — the five-algorithm default — while an empty ELEMENT is the
		// error. The two rows below and the four after them are that distinction.
		{name: "empty whole value is absence", algs: "", want: configXAllAlgorithms},
		{name: "whitespace-only whole value is absence", algs: "   ", want: configXAllAlgorithms},
		{name: "trailing empty element", algs: "RS256,", err: true},
		{name: "leading empty element", algs: ",RS256", err: true},
		{name: "nothing but a separator", algs: ",", err: true},
		{name: "none", algs: "none", err: true},
		{name: "HS256", algs: "HS256", err: true},
		{name: "PS256", algs: "PS256", err: true},
		{name: "EdDSA", algs: "EdDSA", err: true},
		{name: "wrong case", algs: "rs256", err: true},
		{name: "one good element does not carry a bad one", algs: "RS256,HS256", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(configXCustom(map[string]string{
				configXVarProxyAlgs: tc.algs,
			})))
			if tc.err {
				configXWantErr(t, err, configXVarProxyAlgs)
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s=%q): %v", configXVarProxyAlgs, tc.algs, err)
			}
			if !slices.Equal(cfg.Algorithms, tc.want) {
				t.Errorf("Algorithms = %v, want %v", cfg.Algorithms, tc.want)
			}
		})
	}
}

func TestParseRoleMap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		spec string
		want map[string]identity.Role
	}{
		{
			name: "one pair",
			spec: "platform-admins=admin",
			want: map[string]identity.Role{"platform-admins": identity.RoleAdmin},
		},
		{
			name: "whitespace around pairs and around each '='",
			spec: " eng = developer , ops=admin ",
			want: map[string]identity.Role{"eng": identity.RoleDeveloper, "ops": identity.RoleAdmin},
		},
		{
			// Duplicate TARGETS are ordinary: two groups may hold the same role.
			// Only a duplicate source is an error.
			name: "two values mapping to the same role",
			spec: "a=admin,b=admin",
			want: map[string]identity.Role{"a": identity.RoleAdmin, "b": identity.RoleAdmin},
		},
		{
			name: "all three roles",
			spec: "r=viewer,d=developer,a=admin",
			want: map[string]identity.Role{
				"r": identity.RoleViewer, "d": identity.RoleDeveloper, "a": identity.RoleAdmin,
			},
		},
		{
			name: "an empty pair is skipped rather than refused",
			spec: "eng=viewer,",
			want: map[string]identity.Role{"eng": identity.RoleViewer},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
				configXVarRoleMap: tc.spec,
			})))
			if err != nil {
				t.Fatalf("ConfigFromEnv(%s=%q): %v", configXVarRoleMap, tc.spec, err)
			}
			if !maps.Equal(cfg.RoleMap, tc.want) {
				t.Errorf("RoleMap = %v, want %v", cfg.RoleMap, tc.want)
			}
		})
	}

	// The six rejection rules. Every defect is an error rather than a dropped
	// entry, because a dropped entry is a silent authority change.
	msgs := map[string]string{}
	for _, tc := range []struct {
		name     string
		spec     string
		mentions []string
	}{
		{name: "empty", spec: "", mentions: []string{configXVarRoleMap}},
		{name: "nothing but separators", spec: " , , ", mentions: []string{configXVarRoleMap}},
		{name: "no '='", spec: "eng", mentions: []string{configXVarRoleMap, "eng"}},
		{name: "empty claim value", spec: "=admin", mentions: []string{configXVarRoleMap, "=admin"}},
		{name: "empty role", spec: "eng=", mentions: []string{configXVarRoleMap, "eng="}},
		{name: "role outside the three", spec: "eng=owner", mentions: []string{configXVarRoleMap, "owner"}},
		{name: "role names are case-sensitive", spec: "eng=Admin", mentions: []string{configXVarRoleMap, "Admin"}},
		{
			name:     "duplicate claim value",
			spec:     "eng=developer,eng=viewer",
			mentions: []string{configXVarRoleMap, "eng"},
		},
		{
			// Refused even though both pairs agree: the rule is about a source
			// appearing twice, not about the two targets disagreeing.
			name:     "duplicate claim value with the same role",
			spec:     "ops=admin,ops=admin",
			mentions: []string{configXVarRoleMap, "ops"},
		},
	} {
		cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
			configXVarRoleMap: tc.spec,
		})))
		if err == nil {
			t.Errorf("%s: ConfigFromEnv accepted %s=%q as %v", tc.name, configXVarRoleMap, tc.spec, cfg.RoleMap)
			continue
		}
		for _, m := range tc.mentions {
			if !strings.Contains(err.Error(), m) {
				t.Errorf("%s: error %q does not mention %q", tc.name, err, m)
			}
		}
		msgs[tc.name] = err.Error()
	}
	// "empty" and "nothing but separators" are the same rule (nothing mapped) and
	// share a message by design; the rest are separate rules.
	delete(msgs, "nothing but separators")
	configXWantDistinct(t, msgs)
}

func TestConfigFromEnvClaimNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		roles       string
		email       string
		nameClaim   string
		wantRoles   string
		wantEmail   string
		wantName    string
		proxyModeIn bool // run the row against the trusted_proxy base instead
	}{
		{
			// The Keycloak shape. A dotted name is honoured verbatim here; that it
			// resolves as a path and never as a flat key is claims.go's business.
			name:      "dotted roles path",
			roles:     "resource_access.console.roles",
			email:     "upn",
			nameClaim: "preferred_username",
			wantRoles: "resource_access.console.roles",
			wantEmail: "upn",
			wantName:  "preferred_username",
		},
		{
			name:      "empty values take the defaults",
			wantRoles: "roles",
			wantEmail: "email",
			wantName:  "name",
		},
		{
			name:      "surrounding whitespace is trimmed",
			roles:     " realm_access.roles ",
			email:     " mail ",
			nameClaim: " cn ",
			wantRoles: "realm_access.roles",
			wantEmail: "mail",
			wantName:  "cn",
		},
		{
			// The three claim names are shared: they are read after the mode
			// branch, not inside it.
			name:        "shared with trusted_proxy mode",
			roles:       "groups",
			email:       "upn",
			nameClaim:   "cn",
			wantRoles:   "groups",
			wantEmail:   "upn",
			wantName:    "cn",
			proxyModeIn: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			over := map[string]string{
				configXVarClaimRoles: tc.roles,
				configXVarClaimEmail: tc.email,
				configXVarClaimName:  tc.nameClaim,
			}
			env := configXOIDC(over)
			if tc.proxyModeIn {
				env = configXCustom(over)
			}
			cfg, err := identity.ConfigFromEnv(configXEnv(env))
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if cfg.RolesClaim != tc.wantRoles {
				t.Errorf("RolesClaim = %q, want %q", cfg.RolesClaim, tc.wantRoles)
			}
			if cfg.EmailClaim != tc.wantEmail {
				t.Errorf("EmailClaim = %q, want %q", cfg.EmailClaim, tc.wantEmail)
			}
			if cfg.NameClaim != tc.wantName {
				t.Errorf("NameClaim = %q, want %q", cfg.NameClaim, tc.wantName)
			}
		})
	}
}

func TestConfigFromEnvCrossModeVariablesIgnored(t *testing.T) {
	t.Parallel()
	// Variables belonging to the mode that is not in force are warned about and
	// ignored, never a boot error: a staged rollout carries both modes'
	// configuration and flips IDENTITY_MODE between them. The warning itself goes
	// to the process-global slog default, which a parallel test must not swap, so
	// what is asserted here is that the values are ignored.

	t.Run("oidc ignores the proxy variables", func(t *testing.T) {
		t.Parallel()
		cfg, err := identity.ConfigFromEnv(configXEnv(configXOIDC(map[string]string{
			configXVarProxyPreset:   "aws-alb",
			configXVarProxyHeader:   "x-goog-iap-jwt-assertion",
			configXVarProxyIssuer:   "https://cloud.google.com/iap",
			configXVarProxyAudience: "/projects/9/global/backendServices/7",
			configXVarProxyKeysURL:  "https://www.gstatic.com/iap/verify/public_key-jwk",
			configXVarProxyAlgs:     "HS256",
		})))
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Mode != identity.ModeOIDC {
			t.Errorf("Mode = %q, want %q", cfg.Mode, identity.ModeOIDC)
		}
		if cfg.AssertionHeader != "" {
			t.Errorf("AssertionHeader = %q, want empty: the proxy header is not this mode's", cfg.AssertionHeader)
		}
		if cfg.Issuer != "https://idp.example" || cfg.Audience != "console" {
			t.Errorf("issuer/audience = %q/%q, want the oidc pair", cfg.Issuer, cfg.Audience)
		}
		// An unusable preset and an off-allowlist algorithm list are both ignored
		// rather than refused — they belong to the other mode — so Mode A keeps
		// its own five-algorithm allowlist rather than picking up IDENTITY_PROXY_ALGS.
		if !slices.Equal(cfg.Algorithms, configXAllAlgorithms) {
			t.Errorf("Algorithms = %v, want Mode A's five %v", cfg.Algorithms, configXAllAlgorithms)
		}
	})

	t.Run("trusted_proxy ignores the oidc variables", func(t *testing.T) {
		t.Parallel()
		cfg, err := identity.ConfigFromEnv(configXEnv(configXCustom(map[string]string{
			configXVarOIDCIssuer:   "ftp://idp.example",
			configXVarOIDCAudience: "someone-else",
			configXVarOIDCJWKSURL:  "http://idp.example/keys",
		})))
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.Issuer != "https://proxy.example" {
			t.Errorf("Issuer = %q, want the proxy issuer", cfg.Issuer)
		}
		if cfg.Audience != "console" {
			t.Errorf("Audience = %q, want the proxy audience", cfg.Audience)
		}
		if cfg.JWKSURL != "https://proxy.example/keys" {
			t.Errorf("JWKSURL = %q, want the proxy key url", cfg.JWKSURL)
		}
	})
}

// TestFromEnvReturnsNilWhenDisabled uses t.Setenv, so it is the one test in this
// file that is deliberately NOT parallel.
func TestFromEnvReturnsNilWhenDisabled(t *testing.T) {
	for _, name := range configXAllVars {
		t.Setenv(name, "")
	}
	// Configuration that would fail startup if it were read at all: an issuer that
	// only a network call could resolve, and a role-map pair with no '='. A nil
	// error is therefore the assertion that neither the rest of the parse nor New
	// — the only code in this package that opens a connection — was reached.
	t.Setenv(configXVarOIDCIssuer, "https://idp.invalid")
	t.Setenv(configXVarRoleMap, "eng")

	for _, mode := range []string{"", "disabled"} {
		t.Setenv(configXVarMode, mode)
		v, err := identity.FromEnv(context.Background())
		if err != nil {
			t.Fatalf("FromEnv(%s=%q): %v", configXVarMode, mode, err)
		}
		// The concrete *Verifier, so nil compares as nil at the consumer: an
		// interface-typed nil would be a non-nil interface holding a nil pointer,
		// and disabled is the one state that must be byte-for-byte the platform
		// without this package.
		if v != nil {
			t.Fatalf("FromEnv(%s=%q) returned a verifier, want a nil *Verifier", configXVarMode, mode)
		}
	}
}
