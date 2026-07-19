package provider_test

import (
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
