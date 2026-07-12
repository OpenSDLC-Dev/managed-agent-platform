// Package worker is the BYOC (bring-your-own-compute) consumer of the work
// queue: the customer-hosted twin of internal/executor. Where the executor runs
// inside the platform with direct database access, a worker runs in the
// customer's own network and reaches the control plane only over the wire —
// authenticating with its environment key, reading a session's suspended tool
// calls through the session events API, running the built-in toolset in a local
// sandbox, and posting the results back as user.tool_result events. Platform
// executor and BYOC worker are the same pull protocol at two deployment points;
// this is the self_hosted one.
//
// This package is the tool-exec driver only (slice 8, PR C2a): given a session
// id, run its outstanding tools once. The lease loop that polls the work queue,
// acknowledges, heartbeats, and drives this driver — plus the cmd/worker binary
// that wires it to configuration — is a later increment.
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
