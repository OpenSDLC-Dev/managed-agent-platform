package sandboxtest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// GateFixture is what a gate-declaring backend's Harness.Gate returns: the
// GateSpec its gated rows provision with, plus the egress targets and the one
// credential the fixture's stand-in controlplane serves for them.
type GateFixture struct {
	// Spec is the gate the provisioned sandbox pairs with.
	Spec *sandbox.GateSpec
	// AllowedAddr is the host:port — reachable from the gate container — of an
	// origin whose /echo endpoint reflects the Authorization header and body it
	// received. Its host is the single entry on the policy's allowed_hosts.
	AllowedAddr string
	// DeniedHost is a host the policy does not admit.
	DeniedHost string
	// Placeholder and Secret are the fixture's one env-var credential: the gate
	// substitutes Placeholder → Secret on plain-HTTP egress to AllowedAddr, and
	// must pass Placeholder through a CONNECT tunnel untouched.
	Placeholder string
	Secret      string
}

// gateRows asserts the gated meaning of `limited`: only allowed_hosts, and only
// through the gate. One sandbox carries the whole story — the probes build on
// each other (the refusal asserts are meaningful only once the allowed host is
// known reachable, since a gate serves deny-all before its first config fetch).
func gateRows(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Run("GatedLimitedEgressOnlyThroughGate", func(t *testing.T) {
		h := newHarness(t)
		fx := h.Gate(t)
		allowedHost, allowedPort, err := net.SplitHostPort(fx.AllowedAddr)
		if err != nil {
			t.Fatalf("GateFixture.AllowedAddr %q: %v", fx.AllowedAddr, err)
		}
		sid := domain.NewID("sesn")
		ctx := context.Background()
		sb, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID: sid, Image: h.Image, Workdir: workdir,
			Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{allowedHost}},
			Gate:       fx.Spec,
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := sb.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})

		// An allowed host is reachable through the proxy the provider injected.
		// Polled: the gate serves deny-all (403) until its first config fetch
		// lands, so the row waits for the policy, then holds it to account.
		plainReq := `GET http://` + fx.AllowedAddr + `/echo HTTP/1.1\r\n` +
			`Host: ` + fx.AllowedAddr + `\r\nAuthorization: ` + fx.Placeholder +
			`\r\nConnection: close\r\n\r\n`
		var out string
		for deadline := time.Now().Add(45 * time.Second); ; {
			out = proxyExchange(t, sb, plainReq)
			if strings.Contains(out, "HTTP/1.1 200") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("allowed host never became reachable through the gate; last response:\n%s", out)
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Plain HTTP is where the platform holds the request plaintext: the
		// origin's echo shows the substituted secret, never the placeholder.
		if !strings.Contains(out, "authorization="+fx.Secret) {
			t.Errorf("plain-HTTP echo did not show the substituted secret:\n%s", out)
		}
		if strings.Contains(out, fx.Placeholder) {
			t.Errorf("plain-HTTP egress delivered the literal placeholder:\n%s", out)
		}

		// A CONNECT tunnel is opaque — admitted on the target host but never
		// rewritten — so the same request through the tunnel reaches the origin
		// with its placeholder intact (the documented #166 gap). The inner
		// request rides pipelined behind the CONNECT; the gate forwards bytes
		// buffered past the CONNECT line.
		tunneled := proxyExchange(t, sb,
			`CONNECT `+fx.AllowedAddr+` HTTP/1.1\r\nHost: `+fx.AllowedAddr+`\r\n\r\n`+
				`GET /echo HTTP/1.1\r\nHost: `+fx.AllowedAddr+`\r\nAuthorization: `+fx.Placeholder+
				`\r\nConnection: close\r\n\r\n`)
		if !strings.Contains(tunneled, "authorization="+fx.Placeholder) {
			t.Errorf("CONNECT tunnel did not deliver the literal placeholder:\n%s", tunneled)
		}
		if strings.Contains(tunneled, fx.Secret) {
			t.Errorf("CONNECT tunnel substituted inside an opaque stream:\n%s", tunneled)
		}

		// A host outside allowed_hosts is refused on both proxy paths. The
		// Connection: close on the CONNECT keeps the refusal prompt — an
		// admitted CONNECT hijacks before connection management ever sees it.
		refusedPlain := proxyExchange(t, sb,
			`GET http://`+fx.DeniedHost+`/ HTTP/1.1\r\nHost: `+fx.DeniedHost+
				`\r\nConnection: close\r\n\r\n`)
		if !strings.Contains(refusedPlain, "HTTP/1.1 403") {
			t.Errorf("plain-HTTP request to a non-allowed host was not refused:\n%s", refusedPlain)
		}
		refusedTunnel := proxyExchange(t, sb,
			`CONNECT `+fx.DeniedHost+`:443 HTTP/1.1\r\nHost: `+fx.DeniedHost+
				`:443\r\nConnection: close\r\n\r\n`)
		if !strings.Contains(refusedTunnel, "HTTP/1.1 403") {
			t.Errorf("CONNECT to a non-allowed host was not refused:\n%s", refusedTunnel)
		}

		// Egress that bypasses the proxy is dropped by the gate's owner-match
		// firewall. The target is the same origin the gate just reached, so a
		// pass here can only mean the firewall blocked the sandbox's own dial —
		// not an unreachable fixture.
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: `timeout 3 bash -c 'exec 3<>"/dev/tcp/` + allowedHost + `/` + allowedPort + `"' 2>/dev/null && echo OPEN || echo BLOCKED`,
			Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("direct-dial probe: %v", err)
		}
		if !strings.Contains(res.Stdout, "BLOCKED") {
			t.Errorf("direct egress bypassing the proxy was not blocked: %q", res.Stdout)
		}
	})
}

// proxyExchange writes one raw request — \r\n escapes rendered by printf %b —
// through the proxy address the provider injected as HTTP_PROXY, and returns
// everything read back until the peer closes (bounded so a regression that
// keeps the connection open fails instead of hanging).
func proxyExchange(t *testing.T, sb sandbox.Sandbox, rawEscaped string) string {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: `hp=${HTTP_PROXY#http://}; hp=${hp%/}
exec 3<>"/dev/tcp/${hp%%:*}/${hp##*:}" || { echo PROXY-UNREACHABLE; exit 9; }
printf '%b' '` + rawEscaped + `' >&3
timeout 15 cat <&3`,
		Timeout: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("proxy exchange: %v", err)
	}
	return res.Stdout
}
