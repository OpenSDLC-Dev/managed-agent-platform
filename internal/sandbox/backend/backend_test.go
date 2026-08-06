package backend_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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

// sentinelRevoker fails every revoke with a recognizable error, so a Reap that
// reaches it proves the revoker threaded through backend.New — and a Reap that
// does not (the sentinel absent from the error) proves the threading dropped.
type sentinelRevoker struct{ err error }

func (r sentinelRevoker) Revoke(context.Context, domain.ID) error { return r.err }

// TestNewThreadsGateTokenRevoker: Config.GateTokenRevoker must reach both
// backends' providers. Reap revokes before it contacts any daemon or cluster,
// so a sentinel revoke error surfacing from Reap is proof of the threading
// with no Docker daemon or Kubernetes cluster in the loop; a backend arm that
// drops the field leaves the provider revoker-less and Reap fails on the
// endpoint instead, without the sentinel.
func TestNewThreadsGateTokenRevoker(t *testing.T) {
	sentinel := errors.New("revoker-threading-sentinel")

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "http://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c}}]
current-context: c
`), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	for _, tc := range []struct {
		name string
		cfg  backend.Config
	}{
		{"docker", backend.Config{Backend: "docker", DockerHost: "tcp://127.0.0.1:1",
			GateTokenRevoker: sentinelRevoker{err: sentinel}}},
		{"k8s", backend.Config{Backend: "k8s", K8sKubeconfig: kubeconfig,
			GateTokenRevoker: sentinelRevoker{err: sentinel}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := backend.New(tc.cfg)
			if err != nil {
				t.Fatalf("New(%s) = %v", tc.name, err)
			}
			err = p.Reap(context.Background(), domain.NewID("sesn"))
			if !errors.Is(err, sentinel) {
				t.Errorf("Reap error = %v, want the sentinel revoke error — the revoker did not thread through backend.New", err)
			}
		})
	}
}
