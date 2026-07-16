// Package backend selects a sandbox provider by name, so the executor and the
// BYOC worker construct Docker or Kubernetes "hands" from the same config point
// instead of hard-coding one. Both binaries are thin glue around this: they map
// their environment to a Config and call New.
package backend

import (
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
)

// Config names the backend and carries each one's settings; only the selected
// backend's fields are read. An empty Backend defaults to docker — the v1
// backend — so a deployment that predates this selection keeps working with no
// new configuration.
type Config struct {
	Backend string // "docker" (default) or "k8s"

	// docker: empty Host falls back to DOCKER_HOST and then the well-known socket.
	DockerHost string

	// k8s: empty Kubeconfig and Context use in-cluster config (the executor
	// running as a Deployment), then the standard kubeconfig loading rules.
	K8sKubeconfig    string
	K8sContext       string
	K8sNamespace     string
	K8sNetSetupImage string
}

// New builds the named sandbox provider, or an error naming the accepted set.
func New(cfg Config) (sandbox.Provider, error) {
	switch cfg.Backend {
	case "", "docker":
		return docker.New(docker.Config{Host: cfg.DockerHost})
	case "k8s":
		return k8s.New(k8s.Config{
			Kubeconfig:    cfg.K8sKubeconfig,
			Context:       cfg.K8sContext,
			Namespace:     cfg.K8sNamespace,
			NetSetupImage: cfg.K8sNetSetupImage,
		})
	default:
		return nil, fmt.Errorf("sandbox backend %q is not one of docker, k8s", cfg.Backend)
	}
}
