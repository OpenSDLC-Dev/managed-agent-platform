package identity

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Config is one verifier's whole contract.
//
// Build it with ConfigFromEnv, or literally in a test: the gcp-iap preset's real
// gstatic key URL cannot point at a fixture, so an exported Config is what makes
// an IAP-shaped end-to-end test possible at all.
type Config struct {
	Mode     Mode
	Issuer   string // expected iss, compared exactly as configured
	Audience string // must appear in aud; also the expected azp
	JWKSURL  string // set ⇒ discovery is skipped entirely

	AssertionHeader string   // trusted_proxy only; "" in oidc mode
	Algorithms      []string // signature allowlist; empty ⇒ defaultAlgorithms

	RolesClaim string          // default "roles"
	EmailClaim string          // default "email"
	NameClaim  string          // default "name"
	RoleMap    map[string]Role // claim value → role; empty is an error in New

	// HTTPClient replaces the guarded client wholesale. A supplied client gives up
	// everything productionClient carries — the dial-time address guard, the
	// refusal to follow redirects, the proxy-free transport, and the raw
	// header-block cap. Nil selects productionClient, which is what production
	// uses; a test supplies its own to reach an httptest server on loopback.
	HTTPClient *http.Client

	// Now drives token expiry, the key-set TTL and the refresh cooldown from one
	// clock. Nil is time.Now. Must be safe for concurrent use. Exported rather
	// than hidden behind an export_test.go seam because later slices need to drive
	// expiry from outside this package's test binary.
	Now func() time.Time
}

// Package-scope tuning. Constants rather than IDENTITY_* knobs: nobody asked for
// them, and tests drive the behaviour through Config.Now plus export_test.go.
const (
	// keySetTTL bounds how long a key the IdP has REMOVED can still verify. It is
	// why the cache is not "refresh on unknown kid" alone: that design never
	// expires a key, so a revoked signing key would verify forever.
	keySetTTL = 5 * time.Minute
	// refreshCooldown rate-limits fetches. The kid that triggers a refresh is
	// attacker-supplied, so without this a stream of tokens carrying random kids
	// turns this process into an outbound amplifier against the IdP.
	refreshCooldown = 30 * time.Second
	// fetchTimeout bounds one key-set or discovery round trip. It is the shared
	// fetch's own deadline, not the requesting caller's.
	fetchTimeout = 5 * time.Second
	// maxIdPBytes caps a discovery or key-set response body. maxKeys RSA-4096 JWKs
	// with x5c chains sit comfortably under this, so it refuses only a broken or
	// hostile endpoint rather than a working IdP.
	maxIdPBytes = 128 << 10
	// maxHeaderBytesPerResponse bounds the raw header block — the bytes the body
	// cap structurally cannot see.
	maxHeaderBytesPerResponse = 64 << 10

	maxKeys    = 50
	minRSABits = 2048
	maxRSABits = 8192
	// maxRSAExponent is crypto/rsa's own ceiling (checkPub rejects anything
	// larger), applied at parse time so an oversized exponent skips the entry
	// instead of surviving as a silently truncated one.
	maxRSAExponent  = 1<<31 - 1
	clockSkewLeeway = 60 * time.Second
	maxTokenBytes   = 16 << 10 // Entra with many groups reaches roughly 8 KiB
	// maxSubjectBytes is OIDC Core §2's own bound on a subject identifier ("MUST
	// NOT exceed 255 ASCII characters in length"). Unlike the profile fields this
	// one REFUSES rather than truncates — see the check in Verify.
	maxSubjectBytes = 255
	maxRoleValues   = 100
	maxClaimDepth   = 8
	maxLoggedKID    = 64 // attacker-controlled; truncate before logging
	// maxProfileBytes bounds the two descriptive Identity fields. Generous for a
	// real name or address (RFC 5321 caps an email path at 254) and far under the
	// ~12 KiB a claim could otherwise reach inside maxTokenBytes.
	maxProfileBytes = 320
)

// defaultAlgorithms is the settled allowlist. Widening it is a deliberate change
// behind an issue, never a quiet edit: PS256 and EdDSA are absent on purpose.
var defaultAlgorithms = []string{"RS256", "RS512", "ES256", "ES384", "ES512"}

// Google's two spellings of one issuer identity. Only the first is configurable
// — the second is not a URL, so requireIssuerURL refuses it — and only
// acceptedIssuers reads them; see its doc comment for why the pair exists.
const (
	googleIssuer       = "https://accounts.google.com"
	googleLegacyIssuer = "accounts.google.com"
)

// Environment variable names — exactly the plan's table and nothing invented.
// Mode A has no algorithms override: the five-algorithm default is its whole
// contract.
const (
	envMode          = "IDENTITY_MODE"
	envOIDCIssuer    = "IDENTITY_OIDC_ISSUER"
	envOIDCAudience  = "IDENTITY_OIDC_AUDIENCE"
	envOIDCJWKSURL   = "IDENTITY_OIDC_JWKS_URL"
	envProxyPreset   = "IDENTITY_PROXY_PRESET"
	envProxyHeader   = "IDENTITY_PROXY_HEADER"
	envProxyIssuer   = "IDENTITY_PROXY_ISSUER"
	envProxyAudience = "IDENTITY_PROXY_AUDIENCE"
	envProxyKeysURL  = "IDENTITY_PROXY_KEYS_URL"
	envProxyAlgs     = "IDENTITY_PROXY_ALGS"
	envClaimRoles    = "IDENTITY_CLAIM_ROLES"
	envClaimEmail    = "IDENTITY_CLAIM_EMAIL"
	envClaimName     = "IDENTITY_CLAIM_NAME"
	envRoleMap       = "IDENTITY_ROLE_MAP"
)

// ConfigFromEnv parses and validates the IDENTITY_* variables read through
// getenv. cmd/controlplane passes os.Getenv; a test passes a map lookup, which is
// what turns the startup-validation rules into pure, parallel table rows.
//
// getenv returning "" means absent: os.Getenv cannot distinguish unset from
// set-empty, so neither does this, and an empty value takes the default or the
// missing-value error rather than a third branch unreachable in production.
//
// An unset or "disabled" IDENTITY_MODE yields Config{Mode: ModeDisabled} and a
// nil error, reading no other variable — so a staged rollout can place the
// configuration first and flip the mode second. Every other defect is an error
// the binary must fail startup on.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	mode := Mode(strings.TrimSpace(getenv(envMode)))
	switch mode {
	case "", ModeDisabled:
		return Config{Mode: ModeDisabled}, nil
	case ModeOIDC, ModeTrustedProxy:
	default:
		return Config{}, fmt.Errorf("%s %q is not one of %s, %s, %s",
			envMode, string(mode), ModeDisabled, ModeOIDC, ModeTrustedProxy)
	}

	cfg := Config{Mode: mode}
	var err error
	if mode == ModeOIDC {
		err = configureOIDC(&cfg, getenv)
	} else {
		err = configureProxy(&cfg, getenv)
	}
	if err != nil {
		return Config{}, err
	}

	cfg.RolesClaim = valueOr(getenv(envClaimRoles), "roles")
	cfg.EmailClaim = valueOr(getenv(envClaimEmail), "email")
	cfg.NameClaim = valueOr(getenv(envClaimName), "name")
	if cfg.RoleMap, err = parseRoleMap(getenv(envRoleMap)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// configureOIDC fills the Mode A fields.
func configureOIDC(cfg *Config, getenv func(string) string) error {
	cfg.Issuer = strings.TrimSpace(getenv(envOIDCIssuer))
	if cfg.Issuer == "" {
		return fmt.Errorf("%s=%s needs %s", envMode, ModeOIDC, envOIDCIssuer)
	}
	if err := requireIssuerURL(cfg.Issuer); err != nil {
		return fmt.Errorf("%s: %w", envOIDCIssuer, err)
	}
	if cfg.Audience = strings.TrimSpace(getenv(envOIDCAudience)); cfg.Audience == "" {
		return fmt.Errorf("%s=%s needs %s", envMode, ModeOIDC, envOIDCAudience)
	}
	if cfg.JWKSURL = strings.TrimSpace(getenv(envOIDCJWKSURL)); cfg.JWKSURL != "" {
		if err := requireHTTPS(cfg.JWKSURL); err != nil {
			return fmt.Errorf("%s: %w", envOIDCJWKSURL, err)
		}
	}
	// Mode A has no allowlist override — the five-algorithm default is its whole
	// contract — but the parsed Config still carries it, so a caller reading a
	// Config knows the allowlist without also knowing New's defaulting rule.
	cfg.Algorithms = slices.Clone(defaultAlgorithms)
	warnIgnored(getenv, envProxyPreset, envProxyHeader, envProxyIssuer,
		envProxyAudience, envProxyKeysURL, envProxyAlgs)
	return nil
}

// configureProxy fills the Mode B fields from the named preset.
func configureProxy(cfg *Config, getenv func(string) string) error {
	warnIgnored(getenv, envOIDCIssuer, envOIDCAudience, envOIDCJWKSURL)
	switch ProxyPreset(strings.TrimSpace(getenv(envProxyPreset))) {
	case PresetGCPIAP:
		return configureGCPIAP(cfg, getenv)
	case PresetCustom:
		return configureCustomProxy(cfg, getenv)
	default:
		return fmt.Errorf("%s must be %s or %s", envProxyPreset, PresetGCPIAP, PresetCustom)
	}
}

// configureGCPIAP applies the one shipped preset. Every verification parameter
// comes from the preset, so an attempt to override one is a boot error rather
// than a silently ignored variable: those four names ARE the issuer and the key
// source.
func configureGCPIAP(cfg *Config, getenv func(string) string) error {
	for _, name := range []string{envProxyHeader, envProxyIssuer, envProxyKeysURL, envProxyAlgs} {
		if strings.TrimSpace(getenv(name)) != "" {
			return fmt.Errorf("%s=%s supplies %s; unset it", envProxyPreset, PresetGCPIAP, name)
		}
	}
	aud := strings.TrimSpace(getenv(envProxyAudience))
	if aud == "" {
		return fmt.Errorf("%s=%s needs %s, the IAP backend-service audience", envProxyPreset, PresetGCPIAP, envProxyAudience)
	}
	// The gstatic key set is global across every Google Cloud customer, so this
	// audience is the entire tenant boundary. The shape check is loose on purpose:
	// IAP also mints App Engine audiences (/projects/N/apps/ID), and refusing
	// those would break a valid deployment.
	if !strings.HasPrefix(aud, "/projects/") {
		return fmt.Errorf("%s %q is not an IAP audience; want /projects/...", envProxyAudience, aud)
	}
	cfg.AssertionHeader = gcpIAPPreset.Header
	cfg.Issuer = gcpIAPPreset.Issuer
	cfg.JWKSURL = gcpIAPPreset.KeysURL
	// Cloned: preset.go's promise that its literals are pinned by a test holds
	// only for values a caller cannot reach and edit in place.
	cfg.Algorithms = slices.Clone(gcpIAPPreset.Algorithms)
	cfg.Audience = aud
	return nil
}

// configureCustomProxy fills Mode B from explicit values. Discovery never runs
// here: the key URL is pinned.
func configureCustomProxy(cfg *Config, getenv func(string) string) error {
	cfg.AssertionHeader = strings.TrimSpace(getenv(envProxyHeader))
	if cfg.AssertionHeader == "" {
		return fmt.Errorf("%s=%s needs %s", envProxyPreset, PresetCustom, envProxyHeader)
	}
	// A machine credential's header cannot also be the proxy's assertion header.
	// trusted_proxy mode's whole discipline is that Authorization is NEVER read as
	// a human credential — it is where a worker's environment key arrives — and
	// naming it here would quietly invert that: the API layer would read a raw
	// Authorization value as an assertion, and a worker's Bearer key would be
	// handed to the verifier instead of the key lane. x-api-key is refused for the
	// same reason, one lane further up.
	for _, reserved := range []string{"authorization", "x-api-key"} {
		if strings.EqualFold(cfg.AssertionHeader, reserved) {
			return fmt.Errorf("%s must not be %q; that header carries a machine credential",
				envProxyHeader, cfg.AssertionHeader)
		}
	}
	cfg.Issuer = strings.TrimSpace(getenv(envProxyIssuer))
	if cfg.Issuer == "" {
		return fmt.Errorf("%s=%s needs %s", envProxyPreset, PresetCustom, envProxyIssuer)
	}
	if err := requireIssuerURL(cfg.Issuer); err != nil {
		return fmt.Errorf("%s: %w", envProxyIssuer, err)
	}
	if cfg.Audience = strings.TrimSpace(getenv(envProxyAudience)); cfg.Audience == "" {
		return fmt.Errorf("%s=%s needs %s", envProxyPreset, PresetCustom, envProxyAudience)
	}
	cfg.JWKSURL = strings.TrimSpace(getenv(envProxyKeysURL))
	if cfg.JWKSURL == "" {
		return fmt.Errorf("%s=%s needs %s", envProxyPreset, PresetCustom, envProxyKeysURL)
	}
	if err := requireHTTPS(cfg.JWKSURL); err != nil {
		return fmt.Errorf("%s: %w", envProxyKeysURL, err)
	}
	algs, err := parseAlgorithms(getenv(envProxyAlgs))
	if err != nil {
		return err
	}
	cfg.Algorithms = algs
	return nil
}

// warnIgnored logs the variables set for the mode that is not in force. It is a
// warning rather than a boot error so a staged rollout can carry both modes'
// configuration and flip IDENTITY_MODE between them.
func warnIgnored(getenv func(string) string, names ...string) {
	for _, name := range names {
		if strings.TrimSpace(getenv(name)) != "" {
			slog.Warn("identity: variable ignored in this mode", "variable", name)
		}
	}
}

// valueOr returns the trimmed value, or def when it is empty.
func valueOr(v, def string) string {
	if v = strings.TrimSpace(v); v == "" {
		return def
	}
	return v
}

// parseRoleMap parses IDENTITY_ROLE_MAP: comma-separated value=role pairs,
// whitespace trimmed around each pair and around both sides of each '='.
//
// A claim value therefore cannot itself contain ',' or '='. A directory group
// named in distinguished form (CN=platform-admins,OU=corp) is not configurable
// here; map the short group name the IdP actually places in the roles claim.
// Such a value fails startup rather than mapping something unintended.
//
// Every defect is an error rather than a dropped entry. A duplicate source in
// particular: last-wins would be a silent authority change.
func parseRoleMap(s string) (map[string]Role, error) {
	pairs := strings.Split(s, ",")
	out := make(map[string]Role, len(pairs))
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		value, name, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("%s pair %q has no '='", envRoleMap, pair)
		}
		value, name = strings.TrimSpace(value), strings.TrimSpace(name)
		if value == "" {
			return nil, fmt.Errorf("%s pair %q has an empty claim value", envRoleMap, pair)
		}
		if name == "" {
			return nil, fmt.Errorf("%s pair %q has an empty role", envRoleMap, pair)
		}
		role, ok := ParseRole(name)
		if !ok {
			return nil, fmt.Errorf("%s pair %q names role %q, want %s, %s or %s",
				envRoleMap, pair, name, RoleViewer, RoleDeveloper, RoleAdmin)
		}
		if _, dup := out[value]; dup {
			return nil, fmt.Errorf("%s maps %q twice", envRoleMap, value)
		}
		out[value] = role
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is required and must map at least one claim value", envRoleMap)
	}
	return out, nil
}

// parseAlgorithms parses a comma-separated allowlist against defaultAlgorithms.
// An empty value takes all five; an unknown or empty element is an error.
//
// The default is CLONED rather than returned. defaultAlgorithms is the package's
// settled allowlist and the thing allowedAlgorithm consults, so handing a caller
// its backing array would let one in-place edit of a returned Config redefine
// what every later verifier in the process accepts — HS256 included.
func parseAlgorithms(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return slices.Clone(defaultAlgorithms), nil
	}
	fields := strings.Split(s, ",")
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		alg := strings.TrimSpace(field)
		if alg == "" {
			return nil, fmt.Errorf("%s has an empty algorithm", envProxyAlgs)
		}
		if !allowedAlgorithm(alg) {
			return nil, fmt.Errorf("%s %q is not one of %s", envProxyAlgs, alg,
				strings.Join(defaultAlgorithms, ", "))
		}
		out = append(out, alg)
	}
	return out, nil
}

// allowedAlgorithm reports whether alg is on the settled list.
func allowedAlgorithm(alg string) bool {
	for _, a := range defaultAlgorithms {
		if a == alg {
			return true
		}
	}
	return false
}
