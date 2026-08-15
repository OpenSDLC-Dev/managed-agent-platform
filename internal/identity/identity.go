// Package identity is the platform's human-authentication boundary: it verifies
// the credential a human operator presents and reduces it to a principal holding
// one of three roles. Machine credentials never come here — the management
// x-api-key and the BYOC environment key keep exactly the semantics they have
// always had, which is what keeps every documented CLI and SDK flow unchanged
// (docs/plan/31_console-sso-rbac.md).
//
// # Dependency
//
// The verifier is built directly on github.com/go-jose/go-jose/v4. Two other
// routes were evaluated and declined:
//
//   - github.com/coreos/go-oidc. Its oidc.NewRemoteKeySet caches until an unknown
//     kid forces a refetch and offers no TTL, so a key the IdP has REMOVED keeps
//     verifying forever — which the plan's own Architecture text forbids. And
//     oidc.Config carries a single ClientID, so multi-audience plus azp needs
//     SkipClientIDCheck and a hand-written audience policy anyway. Adopting it
//     would mean writing the security core regardless, plus a new module.
//   - A hand-rolled compact-JWS and JWK parser. go-jose is already linked into
//     cmd/controlplane (go list -deps prints it, and its /cipher package, through
//     internal/blob/gcs → cloud.google.com/go/storage → grpc/xds → go-spiffe), so
//     hand-rolling removes nothing from the binary and adds ~250 statements of
//     security-critical parsing.
//
// go-jose earns its place structurally rather than conveniently: the signature
// allowlist is a REQUIRED parameter of jwt.ParseSigned, so alg:none and HS256 die
// inside the parser before any key lookup — stronger than a table we could forget
// to consult.
//
// What go-jose does not do, and this package therefore does:
//
//   - require exp — ValidateWithLeeway skips it when absent (jwt/validation.go);
//   - require a non-empty sub;
//   - anything about azp;
//   - refuse b64, by either route. go-jose lists b64 in its own supportedCritical
//     set (shared.go), so a crit naming it passes; and computeAuthData (jws.go)
//     honours a bare b64 with no reference to crit, verifying over the raw
//     payload. RFC 7797 §7 says a JWT MUST NOT use b64 at all. Verify refuses any
//     crit and any b64, before the key lookup;
//   - fetch, cache, or bound the lifetime of a key set, and OIDC discovery;
//   - bound an RSA modulus, require an odd exponent, reject an even modulus, or
//     bound the exponent above (jwk.go rsaPublicKey builds a bare rsa.PublicKey,
//     and decodes e as int(big.Int.Int64()), which silently truncates);
//   - parse key_ops at all (rawJSONWebKey has no such field);
//   - enforce a JWK's declared alg against the JWS header;
//   - decode a JWK Set per entry: jose.JSONWebKeySet has no set-level
//     UnmarshalJSON and JSONWebKey.UnmarshalJSON fails on any kty it cannot
//     build, so one unusable entry the IdP is entitled to publish would fail the
//     whole set. See parseKeySet.
//
// Do NOT add a len(tok.Headers) != 1 guard: compact parsing makes multi-signature
// unreachable, so the branch is dead code the coverage gate would then carry.
//
// # Uniform rejection
//
// Verify returns exactly one error type, whose Error() is a constant string. The
// detail lives behind Reason(), which the caller logs beside a request id and
// never renders. An oracle distinguishing expired from bad-signature from
// wrong-audience must take deliberate code rather than be one careless wrap away.
//
// One timing side channel is accepted and stated rather than missed: an unknown
// kid within the refresh cooldown answers fast, and past it costs a network round
// trip, so the two are distinguishable. What leaks is whether a kid is in the
// current key set — and the key set is a public document. Signature verification
// uses constant-time stdlib primitives, and every claim comparison is against a
// public configured value.
//
// # Logs and errors
//
// No log line and no error carries a token byte, and no URL reaches either
// without going through redactURL first — a key-set URL may be a signed URL
// whose query string IS the credential. That covers the userinfo, the query, the
// fragment and the opaque form; the scheme, host and path survive on purpose,
// because which endpoint failed is the diagnostic. An error from the transport
// or from url.Parse is reduced to its CAUSE rather than wrapped, since both
// quote the URL verbatim in their own message.
//
// The one attacker-supplied value logged on purpose is the kid, truncated, at
// Debug: it answers which key a provider rotated to, and it is not a credential.
// TestLogsCarryNoCredentials reads the actual output.
//
// # kid is required
//
// A token with no kid, and a JWK with no kid, are both refused. Key selection is
// this package's, never go-jose's, and it is indexed by kid; every provider in
// the compatibility set (Casdoor, Keycloak, Entra ID, Cognito, accounts.google.com
// and Google Cloud IAP) emits one. An OP that does not is a GitHub issue, not a
// silent fallback whose behaviour would change with the size of the key set.
package identity

import (
	"errors"
	"strings"
)

// Role is a platform authority level for an authenticated human.
//
// Deliberately not in internal/domain: that package holds Anthropic-native types
// matching the wire schema (CLAUDE.md principle 1), and the three-role model is a
// declared divergence that never appears on a /v1 path or in a /v1 body.
type Role string

// The three roles, plus the absence of one.
const (
	RoleNone      Role = "" // authenticated, nothing mapped — satisfies nothing
	RoleViewer    Role = "viewer"
	RoleDeveloper Role = "developer"
	RoleAdmin     Role = "admin"
)

// roleRank fixes the strength order. RoleNone is absent from the map, so it
// ranks zero.
var roleRank = map[Role]int{RoleViewer: 1, RoleDeveloper: 2, RoleAdmin: 3}

// AtLeast reports whether r satisfies a route's minimum role, on the fixed order
// admin > developer > viewer.
//
// Fail-closed at both ends: RoleNone satisfies nothing, and a minimum that is not
// one of the three — including "" — is satisfied by nothing, so a typo in a route
// annotation denies rather than admits.
func (r Role) AtLeast(min Role) bool {
	want, ok := roleRank[min]
	if !ok {
		return false
	}
	return roleRank[r] >= want
}

// ParseRole accepts exactly the three role names.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	if _, ok := roleRank[r]; !ok {
		return RoleNone, false
	}
	return r, true
}

// Identity is one verified human principal.
//
// The four strings are exactly the principals-table columns upsertPrincipal
// writes (internal/api); Role is re-derived from the token per request and never
// stored,
// because the IdP stays authoritative. A field that is not here cannot be
// persisted or logged by accident: no raw claims, no token, no expiry.
type Identity struct {
	Issuer      string // the verified iss, equal to the configured issuer
	Subject     string // the verified sub, guaranteed non-empty
	Email       string // "" when the claim is absent or not a string
	DisplayName string // "" likewise
	Role        Role   // RoleNone when no claim value mapped
}

// Mode is the deployment's identity mode.
type Mode string

// The three modes. ModeDisabled is the default and is byte-for-byte the platform
// without this package: no lane exists, and x-api-key remains the only
// management credential.
const (
	ModeDisabled     Mode = "disabled"
	ModeOIDC         Mode = "oidc"
	ModeTrustedProxy Mode = "trusted_proxy"
)

// ProxyPreset names a shipped trusted-proxy configuration.
type ProxyPreset string

// The shipped presets. An aws-alb preset is deliberately absent: ALB's assertion
// is not JWKS-shaped (a per-kid PEM endpoint, signer and issuer in the JWS
// header, and a mandatory expected-signer check), so it needs its own key source
// rather than a table entry.
const (
	PresetGCPIAP ProxyPreset = "gcp-iap"
	PresetCustom ProxyPreset = "custom"
)

// ErrUnauthenticated classes every rejection Verify produces. There is no other
// error path out of Verify, so nothing this package returns from it could render
// as anything but a 401.
var ErrUnauthenticated = errors.New("identity: authentication failed")

// Error is the only error Verify returns.
//
// Error() is a constant string on purpose; the detail is reachable only through
// Reason(), and the caller logs it beside a request id rather than rendering it.
type Error struct{ reason string }

func (e *Error) Error() string  { return "authentication failed" }
func (e *Error) Unwrap() error  { return ErrUnauthenticated }
func (e *Error) Reason() string { return e.reason }

// reject builds the one error type.
//
// It takes a plain string rather than a format and arguments so that
// interpolating a claim value, a token segment, or a kid into a reason is not
// something a caller can do by accident. The reason vocabulary is closed; see the
// package doc.
func reject(reason string) *Error { return &Error{reason: reason} }

// LooksLikeJWT reports the compact-JWS silhouette: exactly three non-empty
// segments separated by two dots, every byte in the base64url alphabet.
//
// This is routing, never security. It is what keeps an sk-map-env01- environment
// key on the worker lane and off this one; everything past it is fully verified.
// It lives here so that "what a JWT looks like" has one definition the API layer's
// lane discrimination inherits instead of re-deriving.
//
// It deliberately does not read an *http.Request: which header a mode owns is
// Mode and AssertionHeader, and header extraction belongs beside the API layer's
// existing bearer-token parsing.
func LooksLikeJWT(s string) bool {
	first := strings.IndexByte(s, '.')
	if first < 1 {
		return false
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, '.')
	if second < 1 {
		return false
	}
	third := rest[second+1:]
	if third == "" || strings.IndexByte(third, '.') >= 0 {
		return false
	}
	return isBase64URL(s[:first]) && isBase64URL(rest[:second]) && isBase64URL(third)
}

// isBase64URL reports whether every byte is in the unpadded base64url alphabet.
func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
