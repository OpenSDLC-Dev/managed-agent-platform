package worker

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// NewClient builds the SDK client a worker uses to reach the control plane's
// session API. The worker authenticates with its environment key as a Bearer
// token — the wire's worker credential, scoped to one environment's work queue
// and distinct from the management x-api-key. The control plane routes a
// session-events request to its environment-key lane only when a Bearer is
// present and no x-api-key is; WithoutEnvironmentDefaults guarantees the latter
// by keeping the SDK from autoloading an ambient ANTHROPIC_API_KEY (which it
// would otherwise send as x-api-key) underneath the explicit options.
//
// baseURL points at the control plane (e.g. an on-prem deployment's URL), never
// a hard-coded api.anthropic.com — a worker talks to the platform it belongs to.
func NewClient(baseURL, envKey string) sdk.Client {
	return sdk.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(baseURL),
		option.WithAuthToken(envKey),
	)
}

// sessionsTokenFromSecret decodes a work item's secret — base64url, padding
// optional, of JSON {"sessions_token": …}: the envelope the control plane
// renders (internal/worktoken.Secret) and the reference worker decodes
// (anthropic-sdk-go v1.66.0 lib/environments/worker.go
// sessionsTokenFromSecret) — to the token, or "" for no secret or one it
// cannot read. Never logged: the caller logs that it was unreadable, not
// what it held.
func sessionsTokenFromSecret(secret string) string {
	if secret == "" {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return ""
	}
	var payload struct {
		SessionsToken string `json:"sessions_token"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return payload.SessionsToken
}

// sessionsTokenOptions makes one call ride the item's sessions token in place
// of the client's environment key: the memory routes admit the token alone
// (plan 36 decision 15), and everything else the worker calls still takes
// the key, so the token is applied per call rather than to a second client.
func sessionsTokenOptions(token string) []option.RequestOption {
	return []option.RequestOption{option.WithAuthToken(token)}
}
