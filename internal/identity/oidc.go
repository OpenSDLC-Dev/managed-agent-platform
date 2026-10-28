package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// RelyingParty performs authorization-code login with S256 PKCE. Only the control
// plane holds it; shared browser attempts belong to internal/login. Exchange
// returns an Identity, never provider tokens. Safe for concurrent use.
//
// The form exchange stays here to share the existing guarded HTTP client and
// response-body cap, without a second token or transport abstraction.
type RelyingParty struct {
	verifier      *Verifier
	client        *http.Client
	authorization *url.URL
	tokenEndpoint string
	clientID      string
	clientSecret  string
	redirectURL   string
	secretInBody  bool
}

func (*RelyingParty) String() string         { return "identity.RelyingParty{credentials:redacted}" }
func (p *RelyingParty) GoString() string     { return p.String() }
func (p *RelyingParty) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Format redacts method-interceptable fmt verbs on values and pointers. fmt
// bypasses methods for %p on a struct value and for unexported nested value
// fields; those paths are not covered. %p on a pointer prints only its address.
func (p RelyingParty) Format(state fmt.State, _ rune) { io.WriteString(state, p.String()) }

// NewRelyingParty discovers the code-flow endpoints even with a pinned JWKS URL.
// New's verifier-only pinned-JWKS contract remains unchanged. Client ID must equal
// the configured audience, so audience and azp cannot name different clients.
func NewRelyingParty(ctx context.Context, cfg Config, clientID, clientSecret, redirectURL string) (*RelyingParty, error) {
	if cfg.Mode != ModeOIDC || clientID == "" || cfg.Audience != clientID {
		return nil, errors.New("identity: OIDC relying party requires oidc mode and client ID equal to audience")
	}
	if err := requireIssuerURL(cfg.Issuer); err != nil {
		return nil, errors.New("identity: invalid OIDC relying-party issuer")
	}
	if _, err := oidcEndpoint(redirectURL); err != nil {
		return nil, errors.New("identity: invalid OIDC callback URL")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = productionClient
	}
	var doc struct {
		discoveryDoc
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	}
	target := strings.TrimSuffix(cfg.Issuer, "/") + discoveryPath
	if err := getJSON(ctx, client, target, fetchTimeout, &doc); err != nil {
		// Remote JSON and transport errors can contain credentials. Keep the stage,
		// not the response or its error string, even in a startup failure.
		return nil, errors.New("identity: OIDC relying-party discovery failed")
	}
	if err := validateDiscovery(doc.discoveryDoc, cfg.Issuer); err != nil {
		return nil, errors.New("identity: invalid OIDC relying-party discovery")
	}
	authorization, err := oidcEndpoint(doc.AuthorizationEndpoint)
	if err != nil {
		return nil, errors.New("identity: invalid OIDC authorization endpoint")
	}
	if _, err := oidcEndpoint(doc.TokenEndpoint); err != nil {
		return nil, errors.New("identity: invalid OIDC token endpoint")
	}
	secretInBody := false
	if clientSecret != "" && len(doc.TokenAuthMethods) != 0 && !slices.Contains(doc.TokenAuthMethods, "client_secret_basic") {
		if !slices.Contains(doc.TokenAuthMethods, "client_secret_post") {
			return nil, errors.New("identity: unsupported OIDC token endpoint client authentication")
		}
		secretInBody = true
	}
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = doc.JWKSURI
	}
	v, err := New(ctx, cfg)
	if err != nil {
		return nil, errors.New("identity: OIDC relying-party verifier initialization failed")
	}
	return &RelyingParty{verifier: v, client: client, authorization: authorization, tokenEndpoint: doc.TokenEndpoint,
		clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL, secretInBody: secretInBody}, nil
}

// oidcEndpoint adds OAuth's no-fragment rule to the shared HTTPS policy.
func oidcEndpoint(raw string) (*url.URL, error) {
	u, err := parseHTTPSURL(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("invalid OIDC endpoint")
	}
	return u, nil
}

func (p *RelyingParty) AuthorizationURL(state, nonce, verifier string) string {
	u := *p.authorization
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.clientID)
	q.Set("redirect_uri", p.redirectURL)
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	challenge := sha256.Sum256([]byte(verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String()
}

// Exchange does one bounded POST through the same guarded client as discovery
// and JWKS. Provider bodies, parser errors and transport errors are never returned
// or logged. No access, refresh or ID token is retained on the relying party.
func (p *RelyingParty) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.redirectURL}, "code_verifier": {verifier}}
	if p.clientSecret == "" || p.secretInBody {
		form.Set("client_id", p.clientID)
	}
	if p.secretInBody {
		form.Set("client_secret", p.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, reject("code exchange failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.clientSecret != "" && !p.secretInBody {
		req.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(p.clientSecret))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Identity{}, reject("code exchange failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, reject("code exchange failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPBytes+1))
	if err != nil || len(body) > maxIdPBytes {
		return Identity{}, reject("code exchange failed")
	}
	var result struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Error != "" || result.IDToken == "" {
		return Identity{}, reject("code exchange failed")
	}
	return p.verifier.VerifyNonce(ctx, result.IDToken, nonce)
}
