// Command worker runs the BYOC (bring-your-own-compute) sandbox worker: it
// polls the control plane's self_hosted work queue over HTTP, runs the built-in
// toolset inside per-session Docker containers on the customer's own compute,
// and posts the user.tool_result events back over the session API. It is the
// customer-hosted twin of the executor — no inbound network access into the
// customer's environment is required, and it reaches the control plane only
// through the wire, authenticating with an environment key. One session at a
// time; run as many worker processes as needed.
//
// Configuration is environment-driven:
//
//	ANTHROPIC_BASE_URL           control-plane URL (required) — never
//	                             api.anthropic.com; the platform this worker serves
//	ANTHROPIC_ENVIRONMENT_ID     the environment whose work queue to poll (required)
//	ANTHROPIC_ENVIRONMENT_KEY    the environment key, sent as Authorization: Bearer
//	                             (required)
//	ANTHROPIC_WORKER_ID          worker identity for the control plane's poll
//	                             metrics (default "<hostname>-<random>")
//	WORKER_IMAGE                 sandbox base image (default "debian:stable-slim")
//	WORKER_WORKDIR               working directory inside the sandbox (default
//	                             "/workspace")
//	SANDBOX_BACKEND              "docker" (default) or "k8s"
//	DOCKER_HOST                  Docker daemon address for the docker backend
//	                             (falls back to the well-known socket)
//	SANDBOX_K8S_KUBECONFIG       kubeconfig path for the k8s backend; empty,
//	                             together with an empty SANDBOX_K8S_CONTEXT, uses
//	                             in-cluster config, then the default loading rules
//	SANDBOX_K8S_CONTEXT          kubeconfig context for the k8s backend
//	SANDBOX_K8S_NAMESPACE        namespace for sandbox pods (default "default")
//	SANDBOX_K8S_NETSETUP_IMAGE   image carrying `ip` for the limited-networking
//	                             init container (default "busybox")
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional OTLP/gRPC collector endpoint
//	OTEL_EXPORTER_OTLP_INSECURE  "true" to export without TLS (default TLS)
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !telemetry.Run(ctx, telemetry.Config{
		ServiceName: "worker",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	}, run) {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	envKey := os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
	switch {
	case baseURL == "":
		return errors.New("ANTHROPIC_BASE_URL is required")
	case envID == "":
		return errors.New("ANTHROPIC_ENVIRONMENT_ID is required")
	case envKey == "":
		return errors.New("ANTHROPIC_ENVIRONMENT_KEY is required")
	}

	provider, err := backend.New(backend.Config{
		Backend:          os.Getenv("SANDBOX_BACKEND"),
		DockerHost:       os.Getenv("DOCKER_HOST"),
		K8sKubeconfig:    os.Getenv("SANDBOX_K8S_KUBECONFIG"),
		K8sContext:       os.Getenv("SANDBOX_K8S_CONTEXT"),
		K8sNamespace:     os.Getenv("SANDBOX_K8S_NAMESPACE"),
		K8sNetSetupImage: os.Getenv("SANDBOX_K8S_NETSETUP_IMAGE"),
	})
	if err != nil {
		return err
	}

	cfg := worker.Config{
		EnvironmentID: envID,
		WorkerID:      os.Getenv("ANTHROPIC_WORKER_ID"),
		Image:         os.Getenv("WORKER_IMAGE"),
		Workdir:       os.Getenv("WORKER_WORKDIR"),
	}
	client := worker.NewClient(baseURL, envKey)

	slog.Info("worker running", "environment", envID)
	return worker.NewWorker(client, provider, cfg).Run(ctx)
}
