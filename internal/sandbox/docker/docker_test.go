package docker_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// testImage satisfies the plan's image contract: /bin/bash at that exact path,
// plus a POSIX userland. The `bash` official image does not — its bash lives in
// /usr/local/bin — which is exactly the kind of assumption the contract pins.
const testImage = "debian:stable-slim"

// The Docker backend against a real daemon. A missing daemon is a hard failure,
// not a skip: a skipped contract test would silently hollow out the gate.
func TestDockerProviderContract(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandboxtest.Harness {
		provider, err := docker.New(docker.Config{})
		if err != nil {
			t.Fatalf("contract tests require Docker: %v", err)
		}
		return sandboxtest.Harness{Provider: provider, Image: testImage}
	})
}
