package provider

import (
	"net/url"
	"strings"
)

// redactedMarker stands in for a credential wherever one would otherwise be
// quoted. It matches what internal/modeltest renders, so both read alike.
const redactedMarker = "[redacted]"

// Redactor removes the credentials one provider was configured with from text
// that quotes an endpoint.
//
// Adapters quote what the endpoint said about a failure, because a status alone
// rarely explains a gateway misconfiguration. But an endpoint that echoes the
// request's auth header into its own diagnostic body — some gateways do on a
// 401 — would otherwise put the credential into an error that becomes a
// session.error event: append-only in Postgres, and re-served to API clients on
// every read.
//
// Matching is by exact value, not by token shape. The adapter holds the secret,
// so it need not guess what a credential looks like — a base_url may point at
// any gateway, proxy, or self-hosted model, whose token format is unknowable.
// Shape-matching "Bearer …" would also have missed the very leak this was
// written for: the Anthropic protocol sends x-api-key, and the observed echo
// was a bare value with no scheme prefix and no header name beside it.
type Redactor struct{ secrets []string }

// NewRedactor collects every credential reachable from cfg.
func NewRedactor(cfg Config) Redactor {
	var r Redactor
	r.add(cfg.APIKey)
	// A credential in base_url's userinfo needs no cooperation from the
	// endpoint at all: the Anthropic SDK formats the request URL into every API
	// error with String(), which keeps the password, rather than Redacted().
	if u, err := url.Parse(cfg.BaseURL); err == nil && u.User != nil {
		pw, _ := u.User.Password()
		r.add(pw)
	}
	for name, value := range cfg.Headers {
		if isAuthHeader(name) {
			r.add(value)
		}
	}
	return r
}

// add registers one secret, plus the token alone when the value carries an auth
// scheme ("Bearer <token>"): an endpoint may echo the whole header or only the
// token, and matching is by substring, so the narrowest form must be registered
// too. An empty secret is skipped — replacing "" would insert the marker
// between every character of the message.
func (r *Redactor) add(secret string) {
	if secret == "" {
		return
	}
	r.secrets = append(r.secrets, secret)
	if _, token, ok := strings.Cut(secret, " "); ok && token != "" {
		r.secrets = append(r.secrets, token)
	}
}

// isAuthHeader reports whether a configured header carries a credential. Only
// those values join the secret set, because Headers also carries routing
// metadata: redacting "x-gateway-route: llm-pool-7" out of "no capacity in pool
// llm-pool-7" would destroy the diagnostic the quoted body exists to provide.
// Headers must be covered at all because they are an auth channel by
// construction — the openai adapter applies them after setting Authorization,
// so an entry can replace the api_key outright.
func isAuthHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "x-api-key", "api-key":
		return true
	}
	return strings.Contains(n, "token") || strings.Contains(n, "secret") ||
		strings.Contains(n, "password") || strings.HasSuffix(n, "-key")
}

// String replaces every configured credential in s. Everything else — the
// status line, the endpoint's error type and message, a request id — survives,
// so the quoted body still explains the failure.
func (r Redactor) String(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redactedMarker)
	}
	return s
}

// Error returns err with its message redacted, keeping the original reachable
// through errors.As and errors.Is. Wrapping with fmt.Errorf("%w") would not do:
// %w re-renders the wrapped message, which is the leak itself. Nothing in this
// repo unwraps a provider error today, but retry logic reading an upstream
// status is the obvious future caller, and it should not have to choose between
// the status and a safe message.
func (r Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := r.String(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
