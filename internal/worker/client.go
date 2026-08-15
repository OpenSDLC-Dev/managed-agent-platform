package worker

import (
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
