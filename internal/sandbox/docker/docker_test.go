package docker_test

import (
	"archive/tar"
	"context"
	"io"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// testImage satisfies the plan's image contract: /bin/bash at that exact path,
// plus a POSIX userland. The `bash` official image does not — its bash lives in
// /usr/local/bin — which is exactly the kind of assumption the contract pins.
const testImage = "debian:stable-slim"

// The Docker backend against a real daemon. A missing daemon is a hard failure,
// not a skip: a skipped contract test would silently hollow out the gate.
// The harness declares gate support, so the gated rows run the real egress-gate
// image against this daemon — the permit path end-to-end.
func TestDockerProviderContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandboxtest.Harness {
		provider, err := docker.New(docker.Config{})
		if err != nil {
			t.Fatalf("contract tests require Docker: %v", err)
		}
		return sandboxtest.Harness{
			Provider: provider, Image: testImage, Gate: gateFixture,
			// The daemon caps a single container's process count (HostConfig.PidsLimit).
			EnforcesPidsLimit: true,
		}
	})
}

// TestExportWorksOnAStoppedContainer is Docker-specific by design: the archive
// endpoint reads a stopped container's filesystem, which is why the checkpoint
// path uses it — the TTL reap must read a sandbox whose process may already be
// dead (plan 24 D7). K8s has no stopped-pod read and degrades instead, so this
// is not a shared contract row.
func TestExportWorksOnAStoppedContainer(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("requires Docker: %v", err)
	}
	ctx := context.Background()
	sid := domain.NewID("sesn")
	sb, err := provider.Provision(ctx, sandbox.Spec{
		SessionID: sid, Image: testImage, Workdir: "/workspace",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = provider.Reap(context.Background(), sid) })
	if err := sb.WriteFile(ctx, "/workspace/survives.txt", []byte("read me stopped")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "kill 1"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("stop container: %v (exit %d)", err, res.ExitCode)
	}
	// The export must run against a STOPPED container, or this test proves
	// nothing the contract rows don't: wait until exec refuses first.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "echo hi"}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container never stopped after kill 1")
		}
		time.Sleep(200 * time.Millisecond)
	}

	rc, err := provider.Export(ctx, sid, "/workspace")
	if err != nil {
		t.Fatalf("export after stop: %v", err)
	}
	defer rc.Close()
	var found bool
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "workspace/survives.txt" {
			b, _ := io.ReadAll(tr)
			if string(b) != "read me stopped" {
				t.Fatalf("content = %q", b)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("survives.txt missing from the stopped-container export")
	}
}

// gateFixture builds the real gate image and stands in for the controlplane
// and the egress origin on one host listener the gate can reach from the
// default bridge network.
func gateFixture(t *testing.T) sandboxtest.GateFixture {
	image := sandboxtest.BuildGateImage(t)
	stub := sandboxtest.StartGateStub(t)
	return sandboxtest.GateFixture{
		Spec: &sandbox.GateSpec{
			Image:           image,
			ControlplaneURL: "http://" + stub.Addr,
			TokenMinter:     stub.Minter(),
		},
		AllowedAddr: stub.Addr,
		DeniedHost:  "denied.invalid",
		Placeholder: stub.Placeholder,
		Secret:      stub.Secret,
	}
}
