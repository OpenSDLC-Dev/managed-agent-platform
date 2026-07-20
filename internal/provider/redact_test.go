package provider_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

func TestRedactorString(t *testing.T) {
	cases := []struct {
		name string
		cfg  provider.Config
		in   string
		want string
	}{
		{
			name: "api key",
			cfg:  provider.Config{APIKey: "sk-secret-123"},
			in:   `{"message":"bad key sk-secret-123"}`,
			want: `{"message":"bad key [redacted]"}`,
		},
		{
			name: "api key echoed inside a bearer header",
			cfg:  provider.Config{APIKey: "sk-secret-123"},
			in:   "rejected Authorization: Bearer sk-secret-123",
			want: "rejected Authorization: Bearer [redacted]",
		},
		{
			name: "base_url userinfo password",
			cfg:  provider.Config{BaseURL: "https://user:pw-secret@gateway.internal"},
			in:   `POST "https://user:pw-secret@gateway.internal/v1/messages": 500`,
			want: `POST "https://user:[redacted]@gateway.internal/v1/messages": 500`,
		},
		{
			// url.Parse stores the password decoded, but url.URL.String() — how
			// the SDK prints the request URL — re-encodes it. Registering only
			// the decoded form matches nothing for a password containing any of
			// the characters userinfo must escape.
			name: "base_url password rendered percent-encoded",
			cfg:  provider.Config{BaseURL: "https://gw:p%40ss-w0rd@gateway.internal"},
			in:   `POST "https://gw:p%40ss-w0rd@gateway.internal/v1/messages": 500`,
			want: `POST "https://gw:[redacted]@gateway.internal/v1/messages": 500`,
		},
		{
			// The same password quoted decoded, as a body echo would show it.
			name: "base_url password echoed decoded",
			cfg:  provider.Config{BaseURL: "https://gw:p%40ss-w0rd@gateway.internal"},
			in:   `{"message":"credential p@ss-w0rd rejected"}`,
			want: `{"message":"credential [redacted] rejected"}`,
		},
		{
			// A password whose configured escapes are not all required in
			// userinfo renders in a third form again: url.Parse decodes %2F,
			// %2B and %3D, String() re-escapes only the "/".
			name: "base_url password re-encoded differently than written",
			cfg:  provider.Config{BaseURL: "https://gw:a%2Fb%2Bc%3Dd@gateway.internal"},
			in:   `POST "https://gw:a%2Fb+c=d@gateway.internal/v1/messages": 500`,
			want: `POST "https://gw:[redacted]@gateway.internal/v1/messages": 500`,
		},
		{
			// url.Parse rejects this outright, so the password is only
			// reachable textually — and an unparsable base_url is itself an
			// error that quotes the string back.
			name: "unparsable base_url still yields its password",
			cfg:  provider.Config{BaseURL: "https://user:pw-secret-999@gw.internal/%zz"},
			in:   `parse "https://user:pw-secret-999@gw.internal/%zz/v1/chat/completions": invalid URL escape "%zz"`,
			want: `parse "https://user:[redacted]@gw.internal/%zz/v1/chat/completions": invalid URL escape "%zz"`,
		},
		{
			// net/http derives an Authorization: Basic header from userinfo, so
			// an auth-echoing endpoint quotes the credential base64-encoded.
			name: "base_url credential echoed as basic auth",
			cfg:  provider.Config{BaseURL: "https://gw-user:pw-secret-999@gateway.internal"},
			in:   "rejected " + base64.StdEncoding.EncodeToString([]byte("gw-user:pw-secret-999")),
			want: "rejected [redacted]",
		},
		{
			// url.Parse reads this as scheme-plus-opaque, so it yields no
			// userinfo at all and only the textual scan can find the password.
			name: "schemeless base_url still yields its password",
			cfg:  provider.Config{BaseURL: "user:pw-secret-777@gw.internal"},
			in:   `Post "user:pw-secret-777@gw.internal/v1/chat/completions": unsupported protocol scheme`,
			want: `Post "user:[redacted]@gw.internal/v1/chat/completions": unsupported protocol scheme`,
		},
		{
			// The authority ends at the query, so nothing here is a credential.
			name: "at-sign in a query is not userinfo",
			cfg:  provider.Config{BaseURL: "https://gateway.internal:8080?x@y"},
			in:   "no capacity at 8080?x on gateway.internal",
			want: "no capacity at 8080?x on gateway.internal",
		},
		{
			// No password means the username is the credential, not an
			// identifier standing beside one.
			name: "userinfo with no password is itself the credential",
			cfg:  provider.Config{BaseURL: "http://token-secret-42@gw.internal"},
			in:   `Post "http://token-secret-42@gw.internal/v1/chat/completions": dial refused`,
			want: `Post "http://[redacted]@gw.internal/v1/chat/completions": dial refused`,
		},
		{
			name: "credential in a base_url query parameter",
			cfg:  provider.Config{BaseURL: "https://gw.internal?api_key=query-secret&pool=alpha"},
			in:   `Get "https://gw.internal?api_key=query-secret&pool=alpha": dial refused`,
			want: `Get "https://gw.internal?api_key=[redacted]&pool=alpha": dial refused`,
		},
		{
			// A custom auth header name matches none of the canonical spellings.
			name: "custom auth header names",
			cfg: provider.Config{Headers: map[string]string{
				"X-Auth": "gw-secret-456", "X-Signature": "sig-789", "X-Credential": "cred-abc",
			}},
			in:   "rejected gw-secret-456 sig-789 cred-abc",
			want: "rejected [redacted] [redacted] [redacted]",
		},
		{
			// Splitting on any space would register "alpha" and blank it out of
			// every diagnostic naming the pool.
			name: "a non-scheme value with a space is not split",
			cfg:  provider.Config{Headers: map[string]string{"X-Route-Key": "pool alpha"}},
			in:   "no capacity in alpha",
			want: "no capacity in alpha",
		},
		{
			// An endpoint quoting the header back normalized drops the extra
			// space a hand-written value carries.
			name: "bearer token registered without surrounding whitespace",
			cfg:  provider.Config{Headers: map[string]string{"Authorization": "Bearer  gw-secret"}},
			in:   "endpoint saw gw-secret",
			want: "endpoint saw [redacted]",
		},
		{
			// A cookie on a model endpoint carries a session credential.
			name: "cookie header value",
			cfg:  provider.Config{Headers: map[string]string{"Cookie": "session=abc-123-def"}},
			in:   "rejected session=abc-123-def",
			want: "rejected [redacted]",
		},
		{
			name: "base_url with no userinfo",
			cfg:  provider.Config{BaseURL: "https://gateway.internal/v1"},
			in:   "no capacity at gateway.internal",
			want: "no capacity at gateway.internal",
		},
		{
			// Kong's key-auth default and Supabase's convention; it matches
			// none of the substring rules.
			name: "apikey header without a separator",
			cfg:  provider.Config{Headers: map[string]string{"apikey": "kong-cred-9"}},
			in:   "key kong-cred-9 rejected",
			want: "key [redacted] rejected",
		},
		{
			name: "auth header value, whole and token alone",
			cfg:  provider.Config{Headers: map[string]string{"Authorization": "Bearer gw-token-xyz"}},
			in:   "sent Bearer gw-token-xyz, endpoint saw gw-token-xyz",
			want: "sent [redacted], endpoint saw [redacted]",
		},
		{
			name: "x-api-key header value",
			cfg:  provider.Config{Headers: map[string]string{"x-api-key": "azure-key-9"}},
			in:   "key azure-key-9 rejected",
			want: "key [redacted] rejected",
		},
		{
			// Redaction must not mangle the diagnostic it protects: a routing
			// tag is not a credential and has to survive.
			name: "routing header value survives",
			cfg:  provider.Config{Headers: map[string]string{"x-gateway-route": "llm-pool-7"}},
			in:   "no capacity in pool llm-pool-7",
			want: "no capacity in pool llm-pool-7",
		},
		{
			// Replacing an empty secret would insert the marker between every
			// character of the message.
			name: "empty config leaves the text alone",
			cfg:  provider.Config{},
			in:   "openai endpoint returned 500 Internal Server Error",
			want: "openai endpoint returned 500 Internal Server Error",
		},
		{
			name: "empty api key with a configured header",
			cfg:  provider.Config{APIKey: "", Headers: map[string]string{"x-api-key": ""}},
			in:   "nothing to redact",
			want: "nothing to redact",
		},
	}
	for _, tc := range cases {
		if got := provider.NewRedactor(tc.cfg).String(tc.in); got != tc.want {
			t.Errorf("%s: String(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestRedactorLongest(t *testing.T) {
	// A caller quoting a bounded prefix of a body over-reads by this much, so
	// it must cover the longest secret, not the first one registered.
	r := provider.NewRedactor(provider.Config{
		APIKey:  "short",
		Headers: map[string]string{"authorization": "Bearer a-much-longer-token"},
	})
	if got, want := r.Longest(), len("Bearer a-much-longer-token"); got != want {
		t.Errorf("Longest() = %d, want %d", got, want)
	}
	if got := provider.NewRedactor(provider.Config{}).Longest(); got != 0 {
		t.Errorf("Longest() with no secrets = %d, want 0", got)
	}
}

func TestRedactorError(t *testing.T) {
	r := provider.NewRedactor(provider.Config{APIKey: "sk-secret-123"})

	if err := r.Error(nil); err != nil {
		t.Errorf("Error(nil) = %v, want nil", err)
	}

	// An error carrying no secret is returned untouched, so no wrapper is paid
	// for on the common path.
	clean := errors.New("upstream returned 503")
	if got := r.Error(clean); got != clean {
		t.Errorf("Error on a clean error returned a new value: %v", got)
	}

	sentinel := errors.New("upstream rejected sk-secret-123 for pool llm-pool-7")
	got := r.Error(sentinel)
	if strings.Contains(got.Error(), "sk-secret-123") {
		t.Errorf("redacted error still quotes the credential: %q", got)
	}
	if !strings.Contains(got.Error(), "llm-pool-7") {
		t.Errorf("redaction destroyed the diagnostic: %q", got)
	}
	// The original stays reachable: %w would have re-rendered the leaking
	// message, so the chain is preserved by Unwrap instead.
	if !errors.Is(got, sentinel) {
		t.Errorf("redacted error lost the wrapped error: %q", got)
	}
	// And wrapping the redacted error the way the brain does must not restore
	// the credential.
	if wrapped := fmt.Errorf("model request: %w", got); strings.Contains(wrapped.Error(), "sk-secret-123") {
		t.Errorf("wrapping the redacted error re-quoted the credential: %q", wrapped)
	}
}
