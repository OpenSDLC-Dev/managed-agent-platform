// Package gaterun is the per-session egress gate's runtime. On startup it applies
// the owner-match firewall, verifies it, then drops privileges, and thereafter
// serves internal/gate's forward proxy fed by a periodic fetch of the session's
// config from the control plane's internal gate-config endpoint
// (docs/plan/12_vaults-credentials.md slice 4).
//
// The OS-touching adapters — the real iptables application and the privilege
// drop — live in cmd/gate behind the Firewall and PrivDropper seams declared
// here, so this package stays unit-testable off a real container. Everything
// with logic lives here and is tested: config conversion, the hot-swap handler,
// the fetch/swap loop, the firewall rule set, and its post-apply verification.
package gaterun

import (
	"net/http"
	"sync/atomic"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"

// Convert builds a gate.Config from a fetched gateconfig.Config: the environment
// networking policy and the agent's MCP server endpoints pass through unchanged
// (the policy decides what to do with the hosts — gate.newPolicy), and each resolved credential
// becomes an egress.Credential — its allowed_hosts a HostSet, its unrestricted
// arm carried through so the secret substitutes for any host. OnUnreachable is
// left unset — the seam is diagnostic-only, and credential_host_unreachable_error
// is a config-conflict event the controlplane emits when rendering this config,
// not something the gate reports. Everything else takes gate.New's defaults,
// the address floor under every dial but an operator-vouched one included — the gate offers no seam
// for its dialer, so this conversion cannot lose it.
func Convert(cfg *gateconfig.Config) gate.Config {
	creds := make([]egress.Credential, 0, len(cfg.Credentials))
	for _, c := range cfg.Credentials {
		creds = append(creds, egress.Credential{
			Placeholder:  c.Placeholder,
			Secret:       c.Secret,
			Hosts:        egress.NewHostSet(c.Networking.AllowedHosts),
			Unrestricted: c.Networking.Type == domain.NetUnrestricted,
			Header:       c.InjectionLocation.Header,
			Body:         c.InjectionLocation.Body,
		})
	}
	return gate.Config{
		Networking:         cfg.Networking,
		MCPServerEndpoints: cfg.MCPServerEndpoints,
		Credentials:        creds,
	}
}

// SwappableHandler is the stable http.Handler the gate's single http.Server
// serves for its whole life: every request is forwarded to the current
// *gate.Gate, whose live policy and credentials Update replaces in place as the
// session's config changes (plan 70 slice 3). Updating in place preserves one
// shared Limiter and lets Gate.Update immediately sever active CONNECT tunnels
// and cancel active plain-HTTP requests whose destination or credentials were
// withdrawn while leaving unaffected traffic uninterrupted. Before the first
// successful fetch the handler serves whatever gate it was constructed with —
// the caller passes a fail-closed gate (admits nothing, no credentials) so an
// unconfigured gate denies rather than leaks.
type SwappableHandler struct {
	current atomic.Pointer[gate.Gate]
	limiter *gate.Limiter
}

// NewSwappableHandler returns a handler that serves initial until Update or
// Swap changes it. initial must be non-nil. The initial gate's Limiter is
// retained on the handler so policy refreshes never reset active resource
// budgets.
func NewSwappableHandler(initial *gate.Gate) *SwappableHandler {
	h := &SwappableHandler{limiter: initial.Limiter()}
	h.current.Store(initial)
	return h
}

// Limiter returns the shared resource limiter carried across policy updates.
func (h *SwappableHandler) Limiter() *gate.Limiter { return h.limiter }

// Update applies cfg's live networking policy, MCP server endpoints, and
// credentials to the current gate in place, withdrawing superseded active work
// while keeping unaffected tunnels and requests alive (plan 70 slice 3).
func (h *SwappableHandler) Update(cfg gate.Config) {
	h.current.Load().Update(cfg)
}

// Swap installs g as the gate every subsequent request uses and retires the
// superseded gate to deny-all so handlers on the old gate cannot begin new
// outbound activity or keep stale tunnels open.
func (h *SwappableHandler) Swap(g *gate.Gate) {
	old := h.current.Swap(g)
	if old != nil && old != g {
		old.Update(gate.Config{})
	}
}

// ServeHTTP forwards to the current gate under an egress span. The span is made
// the active span for the forwarded request (so it wraps the whole proxied
// request or CONNECT tunnel and can parent any spans the gate emits), carrying
// the method and target host — never a credential, substitution happens inside
// the gate. The gate forwards through a plain transport that does not inject a
// traceparent, so making the span active does not leak our trace context to the
// third-party origin; that boundary is deliberate.
func (h *SwappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(
		r.Context(), "egress_request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(r.Method),
			semconv.ServerAddressKey.String(r.Host),
		))
	defer span.End()
	h.current.Load().ServeHTTP(w, r.WithContext(ctx))
}
