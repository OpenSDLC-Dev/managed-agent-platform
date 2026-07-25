package docker_test

import (
	"testing"

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
		return sandboxtest.Harness{Provider: provider, Image: testImage, Gate: gateFixture}
	})
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
