package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
)

// TestGateSpecGatesAllSessions exercises the gate decision (with a configured
// executor): every cloud session — unrestricted without vaults, limited, and
// vault-attached — gets a gate spec carrying the deployment's gate image,
// control-plane URL, OTLP config, and token minter, so unrestricted sessions
// also pass through the gate's address floor.
func TestGateSpecGatesAllSessions(t *testing.T) {
	e := &Executor{cfg: Config{
		GateImage: "gate:1", ControlplaneURL: "http://cp",
		OTelEndpoint: "otel:4317", OTelInsecure: true,
	}}

	for _, tc := range []struct {
		name string
		sess sessionRun
	}{
		{
			name: "unrestricted without vaults",
			sess: sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}},
		},
		{
			name: "limited without vaults",
			sess: sessionRun{networking: domain.Networking{Type: domain.NetLimited}},
		},
		{
			name: "unrestricted with vaults",
			sess: sessionRun{
				networking: domain.Networking{Type: domain.NetUnrestricted},
				vaultIDs:   []string{"vlt_x"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := e.gateSpec(tc.sess)
			if gate == nil || gate.Image != "gate:1" || gate.ControlplaneURL != "http://cp" {
				t.Fatalf("%s gate = %+v, want image gate:1 / cp http://cp", tc.name, gate)
			}
			if gate.OTelEndpoint != "otel:4317" || !gate.OTelInsecure {
				t.Errorf("%s gate OTLP = %q insecure=%v, want otel:4317 / true", tc.name, gate.OTelEndpoint, gate.OTelInsecure)
			}
			if gate.TokenMinter == nil {
				t.Errorf("%s gate has no token minter", tc.name)
			}
		})
	}
}

// TestGateSpecWithoutConfigReturnsNoGate: an executor with no gate configured
// (an explicit opt-out) asks for no gate even for sessions that would use one —
// the backend then applies its own ungated networking (Docker `limited` → no
// egress, K8s → its init-container isolation), rather than faulting the
// provision. It must not silently return a gate the deployment cannot serve.
func TestGateSpecWithoutConfigReturnsNoGate(t *testing.T) {
	for _, cfg := range []Config{
		{},                             // neither set
		{GateImage: "gate:1"},          // no control-plane URL
		{ControlplaneURL: "http://cp"}, // no gate image
	} {
		e := &Executor{cfg: cfg}
		for _, netType := range []domain.NetworkingType{domain.NetLimited, domain.NetUnrestricted} {
			if gate := e.gateSpec(sessionRun{networking: domain.Networking{Type: netType}}); gate != nil {
				t.Errorf("cfg %+v (%s): gateSpec returned a gate the executor cannot serve: %+v", cfg, netType, gate)
			}
		}
	}
}

// TestGateTokenMinterGeneratesGatePrefixed proves the minter's in-memory half
// returns a gate-token-shaped value (the DB half is covered by gatetoken's own
// tests and the vault integration test).
func TestGateTokenMinterGeneratesGatePrefixed(t *testing.T) {
	m := gateTokenMinter{}
	tok := m.Generate()
	if !strings.HasPrefix(tok, gatetoken.TokenPrefix) {
		t.Errorf("Generate() = %q, want a %s… token", tok, gatetoken.TokenPrefix)
	}
	if m.Generate() == tok {
		t.Error("Generate() returned the same token twice; it must be fresh each call")
	}
}

// TestVaultSessionProvisionsWithGate wires the whole path — a vault-attached
// session flows through step → provisionAndRun → gateSpec and reaches the
// provider with Spec.Gate and the proxy env set.
func TestVaultSessionProvisionsWithGate(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{GateImage: "gate:1", ControlplaneURL: "http://cp"})
	h.prov = prov
	h.attachVault(t)

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}

	spec := h.prov.lastSpec
	if spec.Gate == nil {
		t.Fatal("vault-attached session provisioned without a gate")
	}
	if spec.Gate.Image != "gate:1" {
		t.Errorf("gate image = %q, want gate:1", spec.Gate.Image)
	}
}

// TestUnrestrictedSessionProvisionsWithGate is the common cloud path: no vaults,
// unrestricted egress, still provisioned with a gate when configured so the
// gate's owner-match firewall and address floor protect every outbound dial.
func TestUnrestrictedSessionProvisionsWithGate(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{GateImage: "gate:1", ControlplaneURL: "http://cp"})
	h.prov = prov

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}
	spec := h.prov.lastSpec
	if spec.Gate == nil {
		t.Fatal("unrestricted session provisioned without a gate")
	}
	if spec.Gate.Image != "gate:1" || spec.Gate.ControlplaneURL != "http://cp" {
		t.Errorf("gate = %+v, want image gate:1 / cp http://cp", spec.Gate)
	}
	if spec.Gate.TokenMinter == nil {
		t.Error("gate has no token minter")
	}
	if spec.Env != nil {
		t.Errorf("unrestricted vault-less session carried unexpected spec.Env=%v", spec.Env)
	}
	if spec.GateTokenRevoker == nil {
		t.Error("spec carries no GateTokenRevoker")
	}
}

// TestUnrestrictedSessionWithoutGateConfigProvisionsUngated verifies explicit
// gate opt-out (empty GateImage): the session provisions without a gate and
// still carries GateTokenRevoker so any previously gated pair's token is
// revoked on dismantle (#197).
func TestUnrestrictedSessionWithoutGateConfigProvisionsUngated(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{})
	h.prov = prov

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}
	if spec := h.prov.lastSpec; spec.Gate != nil || spec.Env != nil {
		t.Errorf("ungated session provisioned with gate=%v env=%v", spec.Gate, spec.Env)
	}
	if h.prov.lastSpec.GateTokenRevoker == nil {
		t.Error("ungated spec carries no GateTokenRevoker — a dismantled gate's token could never be revoked")
	}
}
