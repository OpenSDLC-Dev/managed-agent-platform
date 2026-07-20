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

// NewRedactor collects the credentials cfg carries.
func NewRedactor(cfg Config) Redactor {
	var r Redactor
	r.add(cfg.APIKey)
	r.addBaseURLSecrets(cfg.BaseURL)
	for name, value := range cfg.Headers {
		if isAuthHeader(name) {
			r.add(value)
		}
	}
	return r
}

// addBaseURLSecrets registers a credential carried in base_url's userinfo. It
// needs no cooperation from the endpoint at all: the Anthropic SDK formats the
// request URL into every API error with String(), not Redacted().
//
// All three renderings are registered, because a password reaches an error in
// whichever one the failure produced: decoded, as url.Parse stores it and as a
// body echo would quote it; re-encoded, as url.URL.String() prints it — a
// password containing '@', '/', '%' or a space, all of which RFC 3986 requires
// be escaped in userinfo, renders nothing like the decoded form; and exactly as
// written in configuration, which is the only form available when base_url does
// not parse at all, itself an error that quotes the string back.
//
// The username is deliberately not registered. It identifies rather than
// authenticates, and masking it would cost a diagnostic to hide nothing.
func (r *Redactor) addBaseURLSecrets(baseURL string) {
	if u, err := url.Parse(baseURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			r.add(pw)
			if _, encoded, found := strings.Cut(u.User.String(), ":"); found {
				r.add(encoded)
			}
		}
	}
	r.add(rawUserinfoPassword(baseURL))
}

// rawUserinfoPassword returns the password as written in a URL's authority,
// found without parsing — url.Parse rejects a malformed URL outright, and the
// error for one quotes the text it could not parse.
func rawUserinfoPassword(baseURL string) string {
	_, rest, ok := strings.Cut(baseURL, "://")
	if !ok {
		return ""
	}
	authority, _, _ := strings.Cut(rest, "/")
	// Userinfo ends at the last "@": an unescaped "@" in a malformed password
	// would otherwise cut the secret short.
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return ""
	}
	_, password, ok := strings.Cut(authority[:at], ":")
	if !ok {
		return ""
	}
	return password
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
	// "apikey" without a separator is Kong's key-auth default and Supabase's
	// convention, and matches none of the substring rules below; "cookie" on a
	// model endpoint carries a session credential, never a diagnostic.
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "apikey", "cookie":
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

// Longest reports the length of the longest registered secret, so a caller that
// quotes only a bounded prefix of a response body can over-read by that much: a
// credential straddling the cap would otherwise be cut in half and survive
// redaction as an unmatched fragment.
func (r Redactor) Longest() int {
	n := 0
	for _, secret := range r.secrets {
		if len(secret) > n {
			n = len(secret)
		}
	}
	return n
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
