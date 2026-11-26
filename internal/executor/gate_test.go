package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
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

// TestProvisionMigratesExistingUngatedSandboxWithCheckpoint proves the safe
// upgrade path: when an already-running ungated unrestricted sandbox triggers
// ErrGateShapeMismatch on a newly gated provision, provisionSandbox captures
// its workspace, shell state, and outputs into a checkpoint BEFORE reaping it,
// provisions the gated replacement, restores the checkpoint, and marks it
// consumed.
func TestProvisionMigratesExistingUngatedSandboxWithCheckpoint(t *testing.T) {
	sb := &fakeSandbox{}
	prov := &fakeProvider{sb: sb}
	h := newHarnessWith(t, prov, Config{
		GateImage: "gate:1", ControlplaneURL: "http://cp",
	})
	h.prov = prov
	h.prov.owned = []domain.ID{h.sid}
	h.prov.running = map[domain.ID]bool{h.sid: true}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{
			"workspace/work.txt":             "keep-me",
			"workspace/.files_materialized":  "stale-sentinel",
			"workspace/skills/.materialized": "stale-skill-sentinel",
		}, nil),
		sandbox.ShellStateRoot + "/" + h.sid.String(): exportTar(t, map[string]string{
			h.sid.String() + "/cwd": "/workspace/sub",
		}, nil),
		outputsDir: exportTar(t, map[string]string{
			"outputs/report.txt": "deliverable",
		}, nil),
	}

	reapedBeforeSecondProvision := false
	h.prov.provisionHook = func(spec sandbox.Spec) (sandbox.Sandbox, error) {
		if spec.Gate == nil {
			t.Errorf("migration provision missing Gate spec")
		}
		if len(h.prov.reapedSnapshot()) == 0 {
			return nil, sandbox.ErrGateShapeMismatch
		}
		reapedBeforeSecondProvision = true
		return sb, nil
	}

	sess := sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}
	got, err := h.exec.provisionSandbox(context.Background(), h.sid, sess, func() {})
	if err != nil {
		t.Fatalf("provisionSandbox: %v", err)
	}
	if got != sb {
		t.Fatalf("returned sandbox = %v, want %v", got, sb)
	}
	if !reapedBeforeSecondProvision {
		t.Fatal("expected Reap between initial ErrGateShapeMismatch and replacement Provision")
	}
	if !slices.Equal(h.prov.calls, []string{"provision", "reap", "provision"}) {
		t.Errorf("provider calls = %v, want [provision reap provision]", h.prov.calls)
	}
	if state, _ := markerState(t, h); state != "consumed" {
		t.Errorf("checkpoint marker state = %q, want consumed", state)
	}

	// Verify the shipped restore tar carries the captured workspace, shell state,
	// and deliverables, with materialization sentinels stripped so files/skills
	// re-materialize cleanly into the gated sandbox.
	rawTar, ok := sb.files[restoreTarPath]
	if !ok {
		t.Fatalf("restore tar was not shipped to %s", restoreTarPath)
	}
	members := map[string]string{}
	tr := tar.NewReader(bytes.NewReader([]byte(rawTar)))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read restore tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			members[hdr.Name] = string(b)
		}
	}
	wantMembers := map[string]string{
		"workspace/work.txt": "keep-me",
		strings.TrimPrefix(sandbox.ShellStateRoot+"/"+h.sid.String()+"/cwd", "/"): "/workspace/sub",
		"mnt/session/outputs/report.txt":                                          "deliverable",
	}
	for name, want := range wantMembers {
		if members[name] != want {
			t.Errorf("restore member %q = %q, want %q (all=%v)", name, members[name], want, members)
		}
	}
	for _, stripped := range []string{"workspace/.files_materialized", "workspace/skills/.materialized"} {
		if _, present := members[stripped]; present {
			t.Errorf("restore tar retained stripped sentinel %q", stripped)
		}
	}
}

// TestProvisionRefusesGateShapeMigrationWithoutBlobStore verifies fail-closed
// preservation: without an object store to hold a checkpoint, provisionSandbox
// refuses to destroy an intact sandbox on ErrGateShapeMismatch.
func TestProvisionRefusesGateShapeMigrationWithoutBlobStore(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}, provisionErr: sandbox.ErrGateShapeMismatch}
	h := newHarnessWith(t, prov, Config{GateImage: "gate:1", ControlplaneURL: "http://cp"})
	bare := New(h.pool, h.log, h.queue, prov, nil, h.cipher, Config{
		GateImage: "gate:1", ControlplaneURL: "http://cp",
	})

	_, err := bare.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {})
	if !errors.Is(err, sandbox.ErrGateShapeMismatch) {
		t.Fatalf("err = %v, want ErrGateShapeMismatch", err)
	}
	if len(prov.reapedSnapshot()) != 0 {
		t.Errorf("intact sandbox was reaped without an object store: reaped=%v", prov.reapedSnapshot())
	}
	if !slices.Equal(prov.calls, []string{"provision"}) {
		t.Errorf("provider calls = %v, want [provision]", prov.calls)
	}
}

// TestProvisionRefusesGateShapeMigrationWhenCaptureFails proves that unlike
// idle TTL reaping, a gate-shape upgrade never degrades to reap-without-checkpoint:
// both an export error and an over-budget workspace leave the intact sandbox
// untouched with no marker written.
func TestProvisionRefusesGateShapeMigrationWhenCaptureFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, h *harness)
		wantErr  error
		maxBytes int64
	}{
		{
			name: "export error",
			setup: func(_ *testing.T, h *harness) {
				h.prov.exportErr = errors.New("export unavailable")
			},
		},
		{
			name: "over checkpoint budget",
			setup: func(t *testing.T, h *harness) {
				h.prov.exports = map[string][]byte{
					"/workspace": exportTar(t, map[string]string{
						"workspace/big.bin": strings.Repeat("x", 4096),
					}, nil),
				}
			},
			wantErr:  ErrCheckpointTooLarge,
			maxBytes: 128,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{sb: &fakeSandbox{}, provisionErr: sandbox.ErrGateShapeMismatch}
			h := newHarnessWith(t, prov, Config{
				GateImage:          "gate:1",
				ControlplaneURL:    "http://cp",
				CheckpointMaxBytes: tc.maxBytes,
			})
			h.prov = prov
			h.prov.owned = []domain.ID{h.sid}
			tc.setup(t, h)

			_, err := h.exec.provisionSandbox(context.Background(), h.sid,
				sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {})
			if err == nil {
				t.Fatal("provisionSandbox succeeded despite capture failure")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if len(prov.reapedSnapshot()) != 0 {
				t.Errorf("intact sandbox was reaped after failed capture: reaped=%v", prov.reapedSnapshot())
			}
			if state, _ := markerState(t, h); state != "" {
				t.Errorf("checkpoint marker = %q after failed capture, want empty", state)
			}
		})
	}
}
