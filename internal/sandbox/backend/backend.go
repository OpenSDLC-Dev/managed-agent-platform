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
	// docker: the network a session's egress-gate container joins (the deploy
	// network reaching the control plane and the outside). Empty defaults to
	// "bridge". Unused for ungated sessions.
	DockerGateNetwork string

	// k8s: empty Kubeconfig and Context use in-cluster config (the executor
	// running as a Deployment), then the standard kubeconfig loading rules.
	K8sKubeconfig    string
	K8sContext       string
	K8sNamespace     string
	K8sNetSetupImage string
	// k8s: the RuntimeClass sandbox pods run under (a hardened runtime such as
	// gVisor's runsc). Empty uses the cluster default. Docker has no analogue —
	// a runtime there is a daemon-level choice, not a per-container one.
	K8sRuntimeClass string
	// k8s: where sandbox pods may run — a comma-separated key=value node
	// selector and a JSON array of tolerations. Both are raw here and parsed by
	// the k8s provider, which rejects a malformed value at startup; a dedicated,
	// tainted sandbox node pool needs both halves. Docker has no analogue.
	K8sNodeSelector string
	K8sTolerations  string
	// k8s: the Secrets sandbox pods pull images with, comma-separated — raw here
	// and parsed by the k8s provider like placement. No Docker counterpart is
	// wired: the engine takes per-request client auth (X-Registry-Auth), which
	// the platform's anonymous pull does not send, so a private image there must
	// already be on the host.
	K8sImagePullSecrets string
}

// New builds the named sandbox provider, or an error naming the accepted set.
func New(cfg Config) (sandbox.Provider, error) {
	switch cfg.Backend {
	case "", "docker":
		return docker.New(docker.Config{Host: cfg.DockerHost, GateNetwork: cfg.DockerGateNetwork})
	case "k8s":
		return k8s.New(k8s.Config{
			Kubeconfig:       cfg.K8sKubeconfig,
			Context:          cfg.K8sContext,
			Namespace:        cfg.K8sNamespace,
			NetSetupImage:    cfg.K8sNetSetupImage,
			RuntimeClass:     cfg.K8sRuntimeClass,
			NodeSelector:     cfg.K8sNodeSelector,
			Tolerations:      cfg.K8sTolerations,
			ImagePullSecrets: cfg.K8sImagePullSecrets,
		})
	default:
		return nil, fmt.Errorf("sandbox backend %q is not one of docker, k8s", cfg.Backend)
	}
}
