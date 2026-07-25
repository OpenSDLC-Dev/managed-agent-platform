package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
)

// TestGateSpecGatesLimitedAndVaultSessions exercises the gate-need decision (with
// a configured executor): a limited or vault-attached session is gated (the
// provider that runs the gate injects the proxy env, not this method); an
// unrestricted vault-less session is not.
func TestGateSpecGatesLimitedAndVaultSessions(t *testing.T) {
	e := &Executor{cfg: Config{GateImage: "gate:1", ControlplaneURL: "http://cp"}}

	// Unrestricted, no vaults: no gate.
	if gate := e.gateSpec(sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); gate != nil {
		t.Errorf("ungated session: gate=%v, want nil", gate)
	}

	// Limited networking: gated.
	gate := e.gateSpec(sessionRun{networking: domain.Networking{Type: domain.NetLimited}})
	if gate == nil || gate.Image != "gate:1" || gate.ControlplaneURL != "http://cp" {
		t.Fatalf("limited session gate = %+v, want image gate:1 / cp http://cp", gate)
	}
	if gate.TokenMinter == nil {
		t.Error("gate has no token minter")
	}

	// Vault-attached (even on unrestricted networking): gated.
	if gate := e.gateSpec(sessionRun{
		networking: domain.Networking{Type: domain.NetUnrestricted},
		vaultIDs:   []string{"vlt_x"},
	}); gate == nil {
		t.Fatal("vault-attached session was not gated")
	}
}

// TestGateSpecWithoutConfigReturnsNoGate: an executor with no gate configured
// (K8s, or a Docker deployment that has not opted in) asks for no gate even for a
// session that would want one — the backend then applies its own fail-closed
// networking (Docker `limited` → no egress, K8s → its init-container isolation),
// the pre-gate behavior, rather than faulting the provision. It must not silently
// return a gate the deployment cannot serve.
func TestGateSpecWithoutConfigReturnsNoGate(t *testing.T) {
	for _, cfg := range []Config{
		{},                             // neither set
		{GateImage: "gate:1"},          // no control-plane URL
		{ControlplaneURL: "http://cp"}, // no gate image
	} {
		e := &Executor{cfg: cfg}
		if gate := e.gateSpec(sessionRun{networking: domain.Networking{Type: domain.NetLimited}}); gate != nil {
			t.Errorf("cfg %+v: gateSpec returned a gate the executor cannot serve: %+v", cfg, gate)
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

// TestUnrestrictedSessionProvisionsWithoutGate is the common path: no vaults,
// unrestricted egress, no gate, no proxy env.
func TestUnrestrictedSessionProvisionsWithoutGate(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{GateImage: "gate:1", ControlplaneURL: "http://cp"})
	h.prov = prov

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}
	if spec := h.prov.lastSpec; spec.Gate != nil || spec.Env != nil {
		t.Errorf("ungated session provisioned with gate=%v env=%v", spec.Gate, spec.Env)
	}
}
