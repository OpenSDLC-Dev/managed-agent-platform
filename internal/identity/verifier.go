package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Verifier verifies this deployment's identity credential. Safe for concurrent
// use; built once per process.
type Verifier struct {
	mode       Mode
	header     string
	issuer     string
	audience   string
	algs       []jose.SignatureAlgorithm
	rolesClaim string
	emailClaim string
	nameClaim  string
	roleMap    map[string]Role
	now        func() time.Time
	keys       *keySet
}

// New builds the verifier, performing every network call a misconfiguration
// could fail: OIDC discovery when JWKSURL is unset, then one warming key-set
// fetch. Both are boot errors, so an unreachable issuer, a discovery document
// naming a different issuer, a non-https key URL, or a key set that parses to
// nothing fails the process rather than the first human's first request.
//
// cfg.Mode must not be ModeDisabled — FromEnv owns that case.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if cfg.Mode == ModeDisabled || cfg.Mode == "" {
		return nil, fmt.Errorf("identity: New needs a mode; %s is FromEnv's case", ModeDisabled)
	}
	if cfg.Mode != ModeOIDC && cfg.Mode != ModeTrustedProxy {
		return nil, fmt.Errorf("identity: unknown mode %q", string(cfg.Mode))
	}
	if cfg.Issuer == "" {
		return nil, errors.New("identity: an issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("identity: an audience is required")
	}
	// The header and the mode must agree, for the same reason the role map is
	// checked here: ConfigFromEnv is not the only caller, and Config's own doc
	// invites building one literally. AssertionHeader() == "" is the documented
	// signal for oidc mode, so a trusted_proxy verifier without a header would
	// have the API layer read the header named "" — a lane that can never match —
	// while an oidc verifier WITH one would have it read an attacker-settable
	// request header instead of Authorization.
	switch {
	case cfg.Mode == ModeTrustedProxy && cfg.AssertionHeader == "":
		return nil, fmt.Errorf("identity: mode %s needs an assertion header", ModeTrustedProxy)
	case cfg.Mode == ModeOIDC && cfg.AssertionHeader != "":
		return nil, fmt.Errorf("identity: mode %s takes no assertion header, got %q",
			ModeOIDC, cfg.AssertionHeader)
	}
	if len(cfg.RoleMap) == 0 {
		return nil, errors.New("identity: a role map is required")
	}
	// Validated AND copied. ConfigFromEnv already rejects these shapes, but New is
	// the package's own boundary and a caller can build a Config literally: an
	// empty claim value would grant its role to every token whose roles claim
	// carries an empty string, and keeping the caller's map would leave live
	// authority mutable from outside — a data race against every concurrent
	// Verify, and a silent privilege change.
	roleMap := make(map[string]Role, len(cfg.RoleMap))
	for value, role := range cfg.RoleMap {
		if value == "" {
			return nil, errors.New("identity: the role map has an empty claim value")
		}
		if _, ok := roleRank[role]; !ok {
			return nil, fmt.Errorf("identity: the role map gives %q the unknown role %q", value, string(role))
		}
		roleMap[value] = role
	}
	if err := requireIssuerURL(cfg.Issuer); err != nil {
		return nil, fmt.Errorf("identity: issuer %w", err)
	}
	if cfg.JWKSURL != "" {
		if err := requireHTTPS(cfg.JWKSURL); err != nil {
			return nil, fmt.Errorf("identity: jwks url %w", err)
		}
	}

	client := cfg.HTTPClient
	if client == nil {
		client = productionClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// A clock reading the zero time is refused rather than tolerated. go-jose's
	// jwt.Expected treats a zero Time as "use time.Now()" (jwt/validation.go), so
	// a Config.Now starting at time.Time{} — the natural zero value, and an easy
	// thing for a later slice's fake clock to be — would validate exp, nbf and
	// iat against the real wall clock while the key-set TTL and the cooldown ran
	// against the fake one. Silent, and in the direction where an expired token
	// can verify.
	if now().IsZero() {
		return nil, errors.New("identity: Config.Now returns the zero time")
	}
	algNames := cfg.Algorithms
	if len(algNames) == 0 {
		algNames = defaultAlgorithms
	}
	algs := make([]jose.SignatureAlgorithm, 0, len(algNames))
	for _, a := range algNames {
		if !allowedAlgorithm(a) {
			return nil, fmt.Errorf("identity: algorithm %q is not allowed", a)
		}
		algs = append(algs, jose.SignatureAlgorithm(a))
	}

	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		var err error
		if jwksURL, err = discover(ctx, client, cfg.Issuer, fetchTimeout); err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
	}

	v := &Verifier{
		mode:       cfg.Mode,
		header:     cfg.AssertionHeader,
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		algs:       algs,
		rolesClaim: valueOr(cfg.RolesClaim, "roles"),
		emailClaim: valueOr(cfg.EmailClaim, "email"),
		nameClaim:  valueOr(cfg.NameClaim, "name"),
		roleMap:    roleMap,
		now:        now,
		keys:       newKeySet(jwksURL, client, algNames, now),
	}

	// Warm the key set so an unreachable or unusable key source is a boot error.
	keys, err := v.keys.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	v.keys.install(keys)

	slog.Info("identity configured",
		"mode", string(cfg.Mode), "issuer", cfg.Issuer, "audience", cfg.Audience,
		"jwks_url", redactURL(jwksURL), "algorithms", algNames, "roles_claim", v.rolesClaim,
		"mapped_values", len(roleMap))
	return v, nil
}

// FromEnv builds the verifier from the IDENTITY_* environment — the one
// construction cmd/controlplane uses, so nothing else re-derives what
// "configured" means.
//
// It returns (nil, nil) when IDENTITY_MODE is unset or disabled: identity is
// optional exactly as the cipher and the blob store are, and the caller decides
// what its absence means. The return type is the concrete *Verifier rather than
// an interface precisely so that nil compares as nil at the consumer — an
// interface-typed nil would be a non-nil interface holding a nil pointer, and
// disabled is the one state that must be byte-for-byte today's platform.
func FromEnv(ctx context.Context) (*Verifier, error) {
	cfg, err := ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	if cfg.Mode == ModeDisabled {
		return nil, nil
	}
	return New(ctx, cfg)
}

// Mode reports the deployment's mode, so the API layer's dispatch knows which
// credential to look for.
func (v *Verifier) Mode() Mode { return v.mode }

// AssertionHeader is the request header carrying the proxy's assertion in
// trusted_proxy mode, and "" in oidc mode.
func (v *Verifier) AssertionHeader() string { return v.header }

// Verify authenticates one compact JWT and maps it to an Identity.
//
// Every failure returns *Error, whose Error() is one constant string. The order
// of the steps below is the security property, not an implementation detail.
func (v *Verifier) Verify(ctx context.Context, token string) (Identity, error) {
	if len(token) > maxTokenBytes {
		return Identity{}, reject("token exceeds the size limit")
	}
	// One call covers segment count, base64url strictness and the algorithm
	// allowlist: alg:none and HS256 die here, before any key lookup.
	tok, err := jwt.ParseSigned(token, v.algs)
	if err != nil {
		return Identity{}, reject("not a verifiable JWS")
	}
	hdr := tok.Headers[0]
	// Neither crit nor b64 may appear, and both are refused HERE — before the key
	// lookup, so a junk token cannot even reach the network. go-jose files both
	// under ExtraHeaders because its sanitized() switch names neither.
	//
	// crit: go-jose's own check is weaker than it looks, allowing a crit that
	// names "b64" (shared.go's supportedCritical). Nothing this package needs is
	// negotiated through crit, so "present" is the whole test.
	//
	// b64 is checked separately rather than only through crit because go-jose
	// honours it either way: computeAuthData (jws.go) reads b64 from the
	// protected header with no reference to crit, and verifies over the raw
	// payload when it is false. RFC 7797 §7 says a JWT MUST NOT use b64 at all.
	// No attacker can reach this — the protected header is signed, so adding b64
	// breaks the signature — but a provider minting one would hand us a token
	// other verifiers read differently, and that is a difference to refuse
	// rather than absorb.
	for _, name := range [...]jose.HeaderKey{"crit", "b64"} {
		if _, ok := hdr.ExtraHeaders[name]; ok {
			return Identity{}, reject("crit or b64 header present")
		}
	}
	if hdr.KeyID == "" {
		return Identity{}, reject("no kid")
	}
	key, err := v.keys.get(ctx, hdr.KeyID)
	if err != nil {
		return Identity{}, err
	}
	// A JWK that declares alg:RS512 must not verify an RS256 header.
	if key.alg != "" && key.alg != hdr.Algorithm {
		return Identity{}, reject("key algorithm mismatch")
	}

	// Passing a single key rather than a key set means go-jose never re-derives
	// which key to use: selection is ours, indexed by the kid we checked.
	var std jwt.Claims
	all := map[string]any{}
	if err := tok.Claims(key.pub, &std, &all); err != nil {
		// One call does two jobs — verify, then unmarshal into both destinations —
		// so its error covers a bad signature AND a payload that will not decode
		// (an "aud" that is a number, an "exp" that is a string, a payload that is
		// a JSON array). Reporting the second as "signature invalid" sends an
		// operator hunting a key rotation for what is a provider emitting the
		// wrong claim type, and the reason vocabulary is their whole diagnostic
		// surface. go-jose's sentinel separates the two exactly.
		if errors.Is(err, jose.ErrCryptoFailure) {
			return Identity{}, reject("signature invalid")
		}
		return Identity{}, reject("claims did not decode")
	}

	if std.Issuer != v.issuer {
		return Identity{}, reject("issuer mismatch")
	}
	if !std.Audience.Contains(v.audience) {
		return Identity{}, reject("audience mismatch")
	}
	if len(std.Audience) > 1 && stringClaim(all, "azp") != v.audience {
		return Identity{}, reject("azp mismatch")
	}
	if std.Subject == "" {
		return Identity{}, reject("missing sub")
	}
	if std.Expiry == nil {
		return Identity{}, reject("missing exp")
	}
	// Expected carries only Time: iss, aud and sub stay ours, checked above.
	switch err := std.ValidateWithLeeway(jwt.Expected{Time: v.now()}, clockSkewLeeway); {
	case err == nil:
	case errors.Is(err, jwt.ErrExpired):
		return Identity{}, reject("expired")
	case errors.Is(err, jwt.ErrNotValidYet):
		return Identity{}, reject("not yet valid")
	case errors.Is(err, jwt.ErrIssuedInTheFuture):
		return Identity{}, reject("issued in the future")
	default:
		return Identity{}, reject("malformed claims")
	}

	return Identity{
		Issuer:  std.Issuer,
		Subject: std.Subject,
		// Bounded, because these two are the only fields whose length the token
		// alone decides, and a later slice persists them. Under maxTokenBytes a
		// claim can reach roughly 12 KiB, which is a valid login turning into an
		// insert failure against a bounded column. Truncating keeps the login
		// working and the descriptive value intact; neither field carries any
		// authority, so nothing is decided on the bytes dropped.
		//
		// Note for the slice that persists them: Email is NOT verified here.
		// Requiring email_verified would deny every human on the many providers
		// that never emit it, and this package has no use for the claim beyond
		// display. Anything that MATCHES or LINKS on an email must make its own
		// verification decision — on an IdP with self-service profile attributes,
		// Casdoor included, the user chooses this string.
		Email:       truncate(stringClaim(all, v.emailClaim), maxProfileBytes),
		DisplayName: truncate(stringClaim(all, v.nameClaim), maxProfileBytes),
		// RoleNone is not an error: the principal is authenticated with no
		// authority, and a role-gated route refuses it.
		Role: strongestRole(roleValues(claimAt(all, v.rolesClaim)), v.roleMap),
	}, nil
}
