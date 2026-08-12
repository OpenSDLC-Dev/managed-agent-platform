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
	if len(cfg.RoleMap) == 0 {
		return nil, errors.New("identity: a role map is required")
	}
	if err := requireHTTPS(cfg.Issuer); err != nil {
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
		roleMap:    cfg.RoleMap,
		now:        now,
		keys:       newKeySet(jwksURL, client, algNames, now),
	}

	// Warm the key set so an unreachable or unusable key source is a boot error.
	keys, err := v.keys.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	v.keys.keys, v.keys.fetchedAt, v.keys.lastTry = keys, now(), now()

	slog.Info("identity configured",
		"mode", string(cfg.Mode), "issuer", cfg.Issuer, "audience", cfg.Audience,
		"jwks_url", jwksURL, "algorithms", algNames, "roles_claim", v.rolesClaim,
		"mapped_values", len(cfg.RoleMap))
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
	// which key to use: selection is ours, indexed by the kid we checked. This
	// call also runs go-jose's critical-header check, so a crit naming anything
	// it does not implement is refused here rather than ignored.
	var std jwt.Claims
	all := map[string]any{}
	if err := tok.Claims(key.pub, &std, &all); err != nil {
		if errors.Is(err, jose.ErrUnsupportedCriticalHeader) {
			return Identity{}, reject("crit header present")
		}
		return Identity{}, reject("signature invalid")
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
		Issuer:      std.Issuer,
		Subject:     std.Subject,
		Email:       stringClaim(all, v.emailClaim),
		DisplayName: stringClaim(all, v.nameClaim),
		// RoleNone is not an error: the principal is authenticated with no
		// authority, and a role-gated route refuses it.
		Role: strongestRole(roleValues(claimAt(all, v.rolesClaim)), v.roleMap),
	}, nil
}
