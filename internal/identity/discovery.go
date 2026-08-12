package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// discoveryPath is the well-known location an OpenID Provider publishes its
// metadata at.
const discoveryPath = "/.well-known/openid-configuration"

// discoveryDoc is the subset of the provider metadata this package reads.
type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// discover fetches the issuer's discovery document and returns its jwks_uri.
//
// The document's issuer must equal the configured issuer exactly — OIDC Discovery
// §4.3's mix-up defence — and the jwks_uri must pass the scheme rule. Both are
// boot errors: a misconfigured identity fails startup rather than open.
//
// The well-known path is appended after trimming a trailing slash from the
// issuer, so https://h and https://h/ both fetch the same document. A trailing
// slash is not a configuration error: a provider whose actual iss ends in one
// must stay configurable, and iss is compared exactly as configured rather than
// normalised, so the comparison stays safe either way.
func discover(ctx context.Context, c *http.Client, issuer string, timeout time.Duration) (string, error) {
	target := strings.TrimSuffix(issuer, "/") + discoveryPath
	var doc discoveryDoc
	if err := getJSON(ctx, c, target, timeout, &doc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	// The published issuer is NOT rendered. It is remote input, bounded only by
	// maxIdPBytes, so quoting it would put up to 128 KiB of an unauthenticated
	// endpoint's bytes into a startup error — and this package's own rule is that
	// a URL reaches a log or an error redacted, because one can be a signed URL
	// whose query string is the credential. Which value was published is not the
	// diagnostic anyway; that the document does not vouch for the configured
	// issuer is, and an operator who wants the published value can fetch the
	// document themselves.
	if doc.Issuer != issuer {
		return "", fmt.Errorf("discovery: %s publishes a different issuer than the configured %s",
			redactURL(target), redactURL(issuer))
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery: %s publishes no jwks_uri", target)
	}
	if err := requireHTTPS(doc.JWKSURI); err != nil {
		return "", fmt.Errorf("discovery: jwks_uri %w", err)
	}
	return doc.JWKSURI, nil
}
