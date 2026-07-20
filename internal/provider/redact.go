package provider

import (
	"encoding/base64"
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
		if isCredentialName(name) {
			r.add(value)
		}
	}
	return r
}

// addBaseURLSecrets registers a credential carried in base_url's userinfo. It
// needs no cooperation from the endpoint at all: the Anthropic SDK formats the
// request URL into every API error with String(), not Redacted().
//
// Every rendering is registered, because a password reaches an error in
// whichever one the failure produced: decoded, as url.Parse stores it and as a
// body echo would quote it; re-encoded, as url.URL.String() prints it — a
// password containing '@', '/', '%' or a space, all of which RFC 3986 requires
// be escaped in userinfo, renders nothing like the decoded form; base64, as
// net/http sends it in an Authorization: Basic header an endpoint may echo
// back; and exactly as written in configuration, which is the only form
// available when base_url does not parse at all, itself an error that quotes
// the string back.
//
// The username is deliberately not registered. It identifies rather than
// authenticates, and masking it would cost a diagnostic to hide nothing.
func (r *Redactor) addBaseURLSecrets(baseURL string) {
	if u, err := url.Parse(baseURL); err == nil {
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				r.add(pw)
				if _, encoded, found := strings.Cut(u.User.String(), ":"); found {
					r.add(encoded)
				}
				// net/http turns userinfo into an Authorization: Basic header
				// whenever the request carries none — always, under the
				// anthropic protocol, which authenticates with x-api-key
				// instead. An endpoint echoing that header back quotes the
				// credential base64-encoded, which no rendering of the URL
				// matches.
				r.add(base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pw)))
			} else {
				// With no password the username is not an identifier standing
				// beside a credential — it is the credential, the
				// token-as-userinfo convention.
				r.add(u.User.Username())
			}
		}
		// Some gateways take the key as a query parameter instead; a transport
		// error quotes the whole URL, query included.
		for name, values := range u.Query() {
			if isCredentialName(name) {
				for _, value := range values {
					r.add(value)
				}
			}
		}
	}
	r.add(rawUserinfoPassword(baseURL))
}

// rawUserinfoPassword returns the password as written in a URL's authority,
// found without parsing — url.Parse rejects a malformed URL outright, and the
// error for one quotes the text it could not parse.
func rawUserinfoPassword(baseURL string) string {
	// A base_url with no scheme is malformed too, and url.Parse reads it as
	// scheme-plus-opaque rather than userinfo, so it reaches here intact.
	authority := baseURL
	if _, rest, ok := strings.Cut(baseURL, "://"); ok {
		authority = rest
	}
	// The authority ends at whichever delimiter comes first; stopping only at
	// "/" would swallow a query into the candidate secret.
	authority, _, _ = strings.Cut(authority, "/")
	authority, _, _ = strings.Cut(authority, "?")
	authority, _, _ = strings.Cut(authority, "#")
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
// too. The token is trimmed because an endpoint quoting the header back
// normalized would drop whitespace a hand-written value carries.
//
// Only a known scheme splits the value. Splitting on any space would register
// the second word of a value that is not a credential pair at all — a routing
// tag like "pool alpha" would put "alpha" in the secret set and blank it out of
// every diagnostic mentioning the pool.
//
// An empty secret is skipped: replacing "" would insert the marker between
// every character of the message.
func (r *Redactor) add(secret string) {
	if secret == "" {
		return
	}
	r.secrets = append(r.secrets, secret)
	scheme, token, ok := strings.Cut(secret, " ")
	if !ok {
		return
	}
	switch strings.ToLower(scheme) {
	case "bearer", "basic", "digest", "token":
		if token = strings.TrimSpace(token); token != "" {
			r.secrets = append(r.secrets, token)
		}
	}
}

// isCredentialName reports whether a configured header or query parameter of
// this name carries a credential. Only those values join the secret set,
// because Headers also carries routing metadata: redacting
// "x-gateway-route: llm-pool-7" out of "no capacity in pool llm-pool-7" would
// destroy the diagnostic the quoted body exists to provide. Headers must be
// covered at all because they are an auth channel by construction — the openai
// adapter applies them after setting Authorization, so an entry can replace the
// api_key outright.
//
// A "-key" suffix classifies deliberately: it catches "x-tenant-key" at the
// cost of masking an "idempotency-key" that was never secret. Over-redaction
// costs a diagnostic and is noticed; under-redaction writes a credential into
// an append-only log and is not.
func isCredentialName(name string) bool {
	// Underscores so a query parameter ("api_key", "access_token") is judged by
	// the same rules as the header spelling.
	n := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	switch n {
	// Neither matches a rule below: "apikey" carries no separator (Kong's
	// key-auth default, Supabase's convention), and a cookie on a model
	// endpoint carries a session credential, never a diagnostic.
	case "apikey", "cookie":
		return true
	}
	return strings.Contains(n, "auth") || strings.Contains(n, "token") ||
		strings.Contains(n, "secret") || strings.Contains(n, "password") ||
		strings.Contains(n, "credential") || strings.Contains(n, "signature") ||
		strings.HasSuffix(n, "-key")
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
