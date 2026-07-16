package backend_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

func TestNewDefaultsToDocker(t *testing.T) {
	// docker.New builds its API client without contacting a daemon, so an empty
	// backend resolves to a docker provider with no Docker running. The type
	// assertion pins the routing: k8s.New would also succeed here (config load is
	// pure, no cluster contact), so a nil-error check alone would not catch a
	// default-arm regression that sent "" to the k8s provider.
	p, err := backend.New(backend.Config{})
	if err != nil {
		t.Fatalf("New(default) = %v, want a docker provider", err)
	}
	if _, ok := p.(*docker.Provider); !ok {
		t.Errorf("New(default) = %T, want *docker.Provider", p)
	}
}

func TestNewDockerExplicit(t *testing.T) {
	p, err := backend.New(backend.Config{Backend: "docker", DockerHost: ""})
	if err != nil {
		t.Fatalf("New(docker) = %v", err)
	}
	if _, ok := p.(*docker.Provider); !ok {
		t.Errorf("New(docker) = %T, want *docker.Provider", p)
	}
}

func TestNewUnknownBackend(t *testing.T) {
	_, err := backend.New(backend.Config{Backend: "podman"})
	if err == nil {
		t.Fatal("New(podman) = nil error, want an unknown-backend error")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Errorf("New(podman) error = %q, want it to name the accepted set", err)
	}
}

func TestNewK8sRoutesToK8s(t *testing.T) {
	// An unusable k8s config must surface the k8s provider's own error — proving
	// "k8s" routes to k8s.New — and NOT the unknown-backend error, which would
	// mean the arm was dropped and "k8s" fell through to the default.
	_, err := backend.New(backend.Config{
		Backend:       "k8s",
		K8sKubeconfig: "/definitely/not/a/kubeconfig",
		K8sContext:    "nonexistent",
	})
	if err == nil {
		t.Fatal("New(k8s, unusable config) = nil error, want a config error")
	}
	if strings.Contains(err.Error(), "not one of") {
		t.Errorf("New(k8s) hit the unknown-backend arm instead of k8s.New: %q", err)
	}
}
