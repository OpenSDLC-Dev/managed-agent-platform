// Package oauthrefresh is the RFC 6749 refresh-token grant, spelled once for
// the two places that perform it: the control plane's mcp_oauth_validate probe
// and the executor's dial-time credential resolution.
//
// It builds the request and reads the grant, and stops there. The HTTP client,
// the response capture and what a failure means all belong to the caller,
// because they genuinely differ — the probe dials through an SSRF-guarded client
// and renders a scrubbed copy of whatever came back, while a dial-time refresh
// captures nothing and needs only the token. What must not differ is the wire:
// which fields a grant carries, which of the three client-authentication arms
// puts the secret where, and how the Basic arm escapes it.
package oauthrefresh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// The token_endpoint_auth arms the reference admits. Anything else is treated as
// a public client: sending the secret to an arm this platform does not
// understand is the one outcome worse than not sending it.
const (
	AuthNone  = "none"
	AuthBasic = "client_secret_basic"
	AuthPost  = "client_secret_post"
)

// Params is one credential's refresh configuration: the non-secret half from the
// stored auth document, and the two secrets from its sealed document.
type Params struct {
	ClientID          string
	TokenEndpoint     string
	TokenEndpointAuth string
	Resource          *string // RFC 8707, when the credential names one
	Scope             *string
	RefreshToken      string
	ClientSecret      string
}

// String, Format and LogValue render Params without any of its secrets. This
// type is shared by callers with different logging habits, and a struct holding
// a refresh token and a client secret prints both under a bare `%v` or a
// structured log of the whole value. Three methods, because none of them covers
// the others: fmt reaches String for %v and its kin but past it for `%#v` and
// for a mismatched verb like %d, which is what Format catches (the reasoning is
// spelled out at internal/modeltest's Config, which redacts a credential the
// same way and for the same reason); and a structured handler reaches LogValue
// before it would marshal the fields.
//
// Three of the fields are secrets, not two: the create-time grammar
// (internal/api, validateEndpointURL) accepts a URL carrying userinfo, so a
// token endpoint can be `https://id:secret@issuer/token` — the reason the
// executor redacts these URLs by value out of everything it stores.
func (p Params) String() string {
	return fmt.Sprintf("oauthrefresh.Params{ClientID:%s TokenEndpoint:%s TokenEndpointAuth:%s "+
		"Resource:%s Scope:%s RefreshToken:[redacted] ClientSecret:[redacted]}",
		p.ClientID, redactURL(p.TokenEndpoint), p.TokenEndpointAuth, derefOr(p.Resource), derefOr(p.Scope))
}

func (p Params) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		io.WriteString(f, "oauthrefresh."+p.String())
		return
	}
	io.WriteString(f, p.String())
}

func (p Params) LogValue() slog.Value {
	return slog.StringValue(p.String())
}

// redactURL renders a stored URL without the userinfo it is allowed to carry. A
// URL that will not parse is not rendered at all: it cannot be stripped, and it
// reached this type from the same place the parseable ones did.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable]"
	}
	if u.User != nil {
		u.User = url.User("[redacted]")
	}
	return u.String()
}

// derefOr renders an optional field, distinguishing an unset one from an empty
// string the credential actually carries.
func derefOr(s *string) string {
	if s == nil {
		return "<unset>"
	}
	return *s
}

// NewRequest builds the token request. It reads nothing back and dials nothing:
// the caller supplies the client, which is where the address guard lives.
func (p Params) NewRequest(ctx context.Context) (*http.Request, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.RefreshToken},
	}
	if p.Scope != nil {
		form.Set("scope", *p.Scope)
	}
	if p.Resource != nil {
		form.Set("resource", *p.Resource)
	}
	switch p.TokenEndpointAuth {
	case AuthBasic:
		// Credentials ride the Authorization header, set below.
	case AuthPost:
		form.Set("client_id", p.ClientID)
		form.Set("client_secret", p.ClientSecret)
	default: // a public client sends its client_id in the body
		form.Set("client_id", p.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if p.TokenEndpointAuth == AuthBasic {
		// RFC 6749 §2.3.1: both halves are form-urlencoded before basic auth.
		req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// BasicNeedle is the base64 composite the client_secret_basic arm sends, and ""
// for the arms that send none. That composite is not any single secret value, so
// a caller that scrubs captured text has to register it on its own — a token
// endpoint reflecting the request's Authorization header would otherwise leak
// the client secret past needles built from the secrets themselves.
func (p Params) BasicNeedle() string {
	if p.TokenEndpointAuth != AuthBasic {
		return ""
	}
	return base64.StdEncoding.EncodeToString(
		[]byte(url.QueryEscape(p.ClientID) + ":" + url.QueryEscape(p.ClientSecret)))
}

// Grant is a token response. RefreshToken is empty when the provider kept the
// one it already issued, and ExpiresIn is zero when it named no lifetime — both
// are the absence of news rather than a rotation to an empty value.
type Grant struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// ParseGrant reads a token response body. ok is false when the body is not JSON
// or carries no access_token: an endpoint can answer 200 with an error document,
// and taking that for a grant would seal an empty token over a working one.
func ParseGrant(raw []byte) (Grant, bool) {
	var grant Grant
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return grant, false
	}
	grant = Grant(body)
	return grant, true
}
