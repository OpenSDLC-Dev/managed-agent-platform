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
// networking policy passes through unchanged, and each resolved credential
// becomes an egress.Credential — its allowed_hosts a HostSet, its unrestricted
// arm carried through so the secret substitutes for any host. OnUnreachable is
// left unset — the seam is diagnostic-only, and credential_host_unreachable_error
// is a config-conflict event the controlplane emits when rendering this config,
// not something the gate reports; Dial/Transport/MaxBodyBytes take gate.New's
// defaults.
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
		Networking:  cfg.Networking,
		Credentials: creds,
	}
}

// SwappableHandler is the stable http.Handler the gate's single http.Server
// serves for its whole life: every request is forwarded to the current
// *gate.Gate, which the fetch loop replaces atomically as the session's config
// changes. The server never restarts across a swap, and a request already in
// flight keeps the gate it started on. Before the first successful fetch the
// handler serves whatever gate it was constructed with — the caller passes a
// fail-closed gate (admits nothing, no credentials) so an unconfigured gate
// denies rather than leaks.
type SwappableHandler struct {
	current atomic.Pointer[gate.Gate]
}

// NewSwappableHandler returns a handler that serves initial until Swap replaces
// it. initial must be non-nil.
func NewSwappableHandler(initial *gate.Gate) *SwappableHandler {
	h := &SwappableHandler{}
	h.current.Store(initial)
	return h
}

// Swap installs g as the gate every subsequent request uses.
func (h *SwappableHandler) Swap(g *gate.Gate) { h.current.Store(g) }

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
