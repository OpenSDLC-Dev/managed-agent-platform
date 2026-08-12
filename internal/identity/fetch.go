package identity

import (
	"context"
	"encoding/json"
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", target, err)
	}
	if len(body) > maxIdPBytes {
		return fmt.Errorf("get %s: body exceeds %d bytes", target, maxIdPBytes)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

// requireHTTPS is the scheme rule: https, or http to a loopback host.
//
// It mirrors the reference SDK's own rule for its credential endpoints
// (anthropic-sdk-go internal/auth/https.go). Userinfo in the URL is refused: a
// credential smuggled into a key URL is never a legitimate configuration.
func requireHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q carries credentials in the URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%q is http to a non-loopback host; https is required", raw)
	default:
		return fmt.Errorf("%q has scheme %q; https is required", raw, u.Scheme)
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
