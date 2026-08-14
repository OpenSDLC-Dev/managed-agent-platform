package oauthrefresh_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/url"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/oauthrefresh"
)

func params() oauthrefresh.Params {
	return oauthrefresh.Params{
		ClientID:          "client-1",
		TokenEndpoint:     "https://issuer.example/token",
		TokenEndpointAuth: oauthrefresh.AuthNone,
		RefreshToken:      "refresh-1",
	}
}

// form reads the request body back as the token endpoint would parse it.
func form(t *testing.T, p oauthrefresh.Params) url.Values {
	t.Helper()
	req, err := p.NewRequest(context.Background())
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	return values
}

func TestAPublicClientSendsItsIDInTheBody(t *testing.T) {
	got := form(t, params())
	if got.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q", got.Get("grant_type"))
	}
	if got.Get("refresh_token") != "refresh-1" {
		t.Errorf("refresh_token = %q", got.Get("refresh_token"))
	}
	if got.Get("client_id") != "client-1" {
		t.Errorf("client_id = %q", got.Get("client_id"))
	}
	if _, ok := got["client_secret"]; ok {
		t.Error("a public client sent a client_secret")
	}
}

func TestAnUnknownAuthTypeIsTreatedAsAPublicClient(t *testing.T) {
	p := params()
	p.TokenEndpointAuth = "private_key_jwt"
	p.ClientSecret = "s3cret"
	got := form(t, p)
	if got.Get("client_id") != "client-1" {
		t.Errorf("client_id = %q", got.Get("client_id"))
	}
	if _, ok := got["client_secret"]; ok {
		t.Error("an unrecognized arm sent the client_secret")
	}
}

func TestClientSecretPostPutsBothHalvesInTheBody(t *testing.T) {
	p := params()
	p.TokenEndpointAuth = oauthrefresh.AuthPost
	p.ClientSecret = "s3cret"
	got := form(t, p)
	if got.Get("client_id") != "client-1" {
		t.Errorf("client_id = %q", got.Get("client_id"))
	}
	if got.Get("client_secret") != "s3cret" {
		t.Errorf("client_secret = %q", got.Get("client_secret"))
	}
	req, err := p.NewRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h := req.Header.Get("Authorization"); h != "" {
		t.Errorf("client_secret_post sent an Authorization header: %q", h)
	}
}

// RFC 6749 §2.3.1: both halves are form-urlencoded before they are base64'd, so
// a secret carrying a colon or a space cannot be misread as a field boundary.
func TestClientSecretBasicEscapesBothHalvesBeforeEncodingThem(t *testing.T) {
	p := params()
	p.TokenEndpointAuth = oauthrefresh.AuthBasic
	p.ClientID = "client one"
	p.ClientSecret = "s3:cret"

	got := form(t, p)
	if _, ok := got["client_id"]; ok {
		t.Error("client_secret_basic also sent client_id in the body")
	}
	if _, ok := got["client_secret"]; ok {
		t.Error("client_secret_basic also sent the secret in the body")
	}

	req, err := p.NewRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPayload := base64.StdEncoding.EncodeToString(
		[]byte(url.QueryEscape("client one") + ":" + url.QueryEscape("s3:cret")))
	if h := req.Header.Get("Authorization"); h != "Basic "+wantPayload {
		t.Errorf("Authorization = %q, want %q", h, "Basic "+wantPayload)
	}
	// The needle a caller scrubs with has to be exactly what went out, or a
	// token endpoint reflecting the header would leak past the scrubber.
	if n := p.BasicNeedle(); n != wantPayload {
		t.Errorf("BasicNeedle = %q, want %q", n, wantPayload)
	}
}

func TestOnlyTheBasicArmHasANeedle(t *testing.T) {
	for _, arm := range []string{oauthrefresh.AuthNone, oauthrefresh.AuthPost, "private_key_jwt"} {
		p := params()
		p.TokenEndpointAuth = arm
		p.ClientSecret = "s3cret"
		if n := p.BasicNeedle(); n != "" {
			t.Errorf("%s: BasicNeedle = %q, want empty", arm, n)
		}
	}
}

func TestScopeAndResourceRideOnlyWhenSet(t *testing.T) {
	got := form(t, params())
	if _, ok := got["scope"]; ok {
		t.Error("scope sent when unset")
	}
	if _, ok := got["resource"]; ok {
		t.Error("resource sent when unset")
	}

	p := params()
	scope, resource := "mcp:read", "https://mcp.example/"
	p.Scope, p.Resource = &scope, &resource
	got = form(t, p)
	if got.Get("scope") != scope {
		t.Errorf("scope = %q", got.Get("scope"))
	}
	if got.Get("resource") != resource {
		t.Errorf("resource = %q", got.Get("resource"))
	}
}

func TestAnUnusableTokenEndpointIsRefusedBeforeTheDial(t *testing.T) {
	p := params()
	p.TokenEndpoint = "://not a url"
	if _, err := p.NewRequest(context.Background()); err == nil {
		t.Fatal("built a request for an unparseable token endpoint")
	}
}

func TestParseGrantReadsTheRotatedTokensAndTheExpiry(t *testing.T) {
	grant, ok := oauthrefresh.ParseGrant([]byte(
		`{"access_token":"a2","refresh_token":"r2","expires_in":3600,"token_type":"Bearer"}`))
	if !ok {
		t.Fatal("a well-formed grant was not read")
	}
	if grant.AccessToken != "a2" || grant.RefreshToken != "r2" || grant.ExpiresIn != 3600 {
		t.Errorf("grant = %+v", grant)
	}
}

func TestParseGrantRejectsAnAnswerThatIsNotOne(t *testing.T) {
	for name, body := range map[string]string{
		"not json":            `<html>ok</html>`,
		"no access_token":     `{"token_type":"Bearer","expires_in":60}`,
		"empty access_token":  `{"access_token":""}`,
		"access_token a list": `{"access_token":["a2"]}`,
	} {
		if _, ok := oauthrefresh.ParseGrant([]byte(body)); ok {
			t.Errorf("%s: read as a grant", name)
		}
	}
}

// A provider that keeps the refresh token in place answers without one, and the
// caller has to be able to tell that from a rotation.
func TestParseGrantLeavesAnAbsentRefreshTokenEmpty(t *testing.T) {
	grant, ok := oauthrefresh.ParseGrant([]byte(`{"access_token":"a2"}`))
	if !ok {
		t.Fatal("a grant without a refresh token was not read")
	}
	if grant.RefreshToken != "" || grant.ExpiresIn != 0 {
		t.Errorf("grant = %+v", grant)
	}
}
