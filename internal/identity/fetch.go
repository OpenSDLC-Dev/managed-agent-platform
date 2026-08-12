package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
)

// productionClient is the guarded client a Config with no HTTPClient selects —
// internal/mcp's DefaultClient reasoning applied to key fetching.
//
// The dial guard is here because the URL that matters is remote-supplied: the
// configured issuer and key URL are operator process configuration, but the
// jwks_uri this package actually fetches arrives INSIDE the discovery document,
// from the remote issuer. A hostile or compromised provider is already game over
// for authentication — but the guard still denies it a blind, credential-free
// SSRF primitive against the control plane's own loopback surfaces and the cloud
// metadata endpoint, and checking the resolved IP at connect time makes DNS
// rebinding ineffective. It is not a defence against a hostile issuer, and it is
// not a substitute for the scheme rule.
//
// The two rules are separate and must not be conflated. requireHTTPS is a SCHEME
// rule with a loopback exception for tests; the guard is a DIAL rule on the
// resolved address, with no exception. The consequence, stated rather than
// discovered: with the guard wired, http-to-loopback URLs are dead in production —
// fail-closed, and exactly what the loopback exception is for. Tests reach
// 127.0.0.1 by supplying Config.HTTPClient, which is the single seam.
//
// RFC 1918 stays permitted by dialguard, so a control plane reaching a sibling
// Casdoor container by service name over a Docker bridge works unchanged.
//
// No Proxy: a proxy moves the dial off the target and onto the proxy, which is
// the guard removed rather than satisfied, and it interposes a cache we do not
// control on our only key source. A deployment whose egress genuinely needs one
// supplies Config.HTTPClient and owns the consequence.
var productionClient = &http.Client{
	Timeout: fetchTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: fetchTimeout,
			Control: dialguard.Control(dialguard.IPAllowed),
		}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytesPerResponse,
	},
}

// getJSON performs one bounded, deadlined GET and decodes the body into v.
//
// context.WithoutCancel then WithTimeout: the trace context rides along (values
// survive) while the caller's cancellation does not. The request that happened to
// lead a key-set flight hanging up must not fail every other caller waiting on
// it; waiters honour their own context while waiting on the flight channel
// instead.
//
// Content-Type is deliberately not enforced: too many providers get it wrong, and
// the body having to parse is the real check.
func getJSON(ctx context.Context, c *http.Client, target string, timeout time.Duration, v any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	safe := redactURL(target)
	resp, err := c.Do(req)
	if err != nil {
		// *url.Error's own message quotes the URL verbatim, and a signed key URL
		// carries its credential in the query string. Wrapping its CAUSE instead
		// drops the quoted URL while keeping the chain intact, so errors.Is still
		// finds context.DeadlineExceeded and the dial guard's refusal underneath.
		cause := err
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			cause = uerr.Err
		}
		return fmt.Errorf("get %s: %w", safe, cause)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", safe, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", safe, err)
	}
	if len(body) > maxIdPBytes {
		return fmt.Errorf("get %s: body exceeds %d bytes", safe, maxIdPBytes)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode %s: %w", safe, err)
	}
	return nil
}

// redactURL renders a URL for a log line or an error with the components that
// carry credentials in practice removed: the userinfo, the query, the fragment,
// and the opaque form. A provider that hands out a signed key-set URL puts its
// token in the query, and both logs and errors travel further than the process.
//
// The scheme, host and PATH survive on purpose, and that is the limit of the
// claim: an operator reading "key set fetch failed" needs to know which endpoint
// it was, and /oauth2/v3/certs versus /.well-known/jwks.json is the whole
// diagnostic. A provider that embeds a secret in a path segment would still have
// it logged — nothing here can tell a secret path from a routing one, and
// blanking the path would cost every legitimate reader the answer to protect a
// shape no provider in the compatibility set uses.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	if u.Opaque != "" {
		// Opaque is everything after "scheme:" when there is no "//" — one blob
		// with no structure to inspect, so none of it is quotable.
		u.Opaque = "redacted"
	}
	u.Fragment = ""
	return u.String()
}

// requireHTTPS is the scheme rule: https, or http to a loopback host.
//
// It mirrors the reference SDK's own rule for its credential endpoints
// (anthropic-sdk-go internal/auth/https.go). Userinfo in the URL is refused: a
// credential smuggled into a key URL is never a legitimate configuration.
func requireHTTPS(raw string) error {
	_, err := parseHTTPSURL(raw)
	return err
}

// requireIssuerURL adds the issuer identifier's own rule to the scheme rule:
// OIDC Discovery §2 says an issuer identifier MUST NOT carry a query or a
// fragment. It is enforced because iss is compared as an exact string — two URLs
// differing only in a query would otherwise be two issuers for one provider, and
// the discovery path appends /.well-known/... to whatever it is given, which a
// query silently breaks.
func requireIssuerURL(raw string) error {
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return err
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%q has a query or fragment; an issuer identifier must have neither", redactURL(raw))
	}
	return nil
}

// parseHTTPSURL is requireHTTPS's shared body, returning the parsed URL so the
// issuer rule can add its own checks without parsing twice.
func parseHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error's message quotes the input verbatim, so the cause is NOT
		// wrapped here: redacting the prefix while wrapping an error that reprints
		// the whole URL would redact nothing. Only the parser's own reason is
		// kept, which is what an operator needs anyway.
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			err = uerr.Err
		}
		return nil, fmt.Errorf("%q is not a URL: %v", redactURL(raw), err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%q has no host", redactURL(raw))
	}
	if u.User != nil {
		// Redacted: the whole point of refusing this URL is that it carries a
		// credential, and quoting it verbatim would copy that credential into
		// the operator's startup log.
		return nil, fmt.Errorf("%q carries credentials in the URL", redactURL(raw))
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return u, nil
		}
		return nil, fmt.Errorf("%q is http to a non-loopback host; https is required", redactURL(raw))
	default:
		return nil, fmt.Errorf("%q has scheme %q; https is required", redactURL(raw), u.Scheme)
	}
}

// isLoopbackHost reports whether host is one of the loopback names the scheme
// rule exempts.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
