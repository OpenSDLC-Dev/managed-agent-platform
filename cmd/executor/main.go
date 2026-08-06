// Command executor runs the platform-managed sandbox worker: it claims
// tool_exec work from the shared Postgres queue, runs the built-in toolset
// inside per-session Docker containers, and appends the agent.tool_result
// events the brain resumes on. Disposable "hands" — run as many as needed;
// a container dying is one tool-call error, not a lost session.
// Configuration is environment-driven:
//
//	DATABASE_URL             Postgres DSN (required; same database as the
//	                         controlplane and brain). A pool_max_conns below 3
//	                         is refused: a provision and the reaper can pin a
//	                         session-lock connection each while their nested
//	                         queries still need the pool
//	EXECUTOR_IMAGE           sandbox base image (default "debian:stable-slim")
//	EXECUTOR_WORKDIR         working directory inside the sandbox (default
//	                         "/workspace")
//	EXECUTOR_LEASE_TTL       work-item lease, Go duration (default "15m") —
//	                         must comfortably exceed a single tool's timeout
//	EXECUTOR_POLL_INTERVAL   idle queue poll, Go duration (default "500ms")
//	EXECUTOR_REAP_INTERVAL   sandbox reap pass interval, Go duration (default
//	                         "1m"); each pass destroys the sandboxes of
//	                         deleted (tombstone-evidenced), archived, and
//	                         terminated cloud sessions — self_hosted sandboxes
//	                         belong to the BYOC worker and are never touched
//	EXECUTOR_SANDBOX_IDLE_TTL   idle sandbox lifetime, Go duration (default
//	                         "24h"; "0" disables the idle tier). An idle cloud
//	                         session older than this is checkpointed and its
//	                         sandbox reaped — unless it still owes work or an
//	                         unanswered confirmation ask. Requires object
//	                         storage; a blob-less executor disables the tier
//	                         at startup with one log line
//	EXECUTOR_CHECKPOINT_MAX_BYTES   workspace-checkpoint size budget in bytes
//	                         (default 2147483648, 2 GiB); over budget the TTL
//	                         tier reaps without a checkpoint
//	CONTROLPLANE_URL         where a session's egress gate fetches its config;
//	                         set with EXECUTOR_GATE_IMAGE to opt into the gate.
//	                         Unset: no gate runs; a gate-wanting session (limited
//	                         or vault-attached) falls back to the backend's own
//	                         fail-closed networking (Docker limited -> no egress),
//	                         while unrestricted sessions network directly as before
//	EXECUTOR_GATE_IMAGE      the egress-gate container image (built with
//	                         `docker build --target gate`); opts into the gate
//	                         together with CONTROLPLANE_URL
//	SANDBOX_BACKEND          "docker" (default) or "k8s"
//	DOCKER_HOST              Docker daemon address for the docker backend
//	                         (falls back to the well-known socket)
//	SANDBOX_DOCKER_GATE_NETWORK   Docker network a session's egress-gate
//	                         container joins (default "bridge")
//	SANDBOX_K8S_KUBECONFIG   kubeconfig path for the k8s backend; empty, together
//	                         with an empty SANDBOX_K8S_CONTEXT, uses in-cluster
//	                         config, then the default loading rules
//	SANDBOX_K8S_CONTEXT      kubeconfig context for the k8s backend
//	SANDBOX_K8S_NAMESPACE    namespace for sandbox pods (default "default")
//	SANDBOX_K8S_NODE_SELECTOR    node labels every sandbox pod requires, as
//	                         comma-separated key=value; empty places nothing.
//	                         Malformed fails startup
//	SANDBOX_K8S_TOLERATIONS  taints every sandbox pod tolerates, as a JSON array
//	                         of Kubernetes Toleration objects; empty tolerates
//	                         nothing. Malformed fails startup
//	SANDBOX_K8S_IMAGE_PULL_SECRETS   Secrets every sandbox pod pulls images
//	                         with, as comma-separated Secret names; empty adds
//	                         nothing. Malformed fails startup
//	SANDBOX_K8S_NETSETUP_IMAGE   image carrying `ip` for the limited-networking
//	                         init container (default "busybox")
//	BLOB_BACKEND             object storage: "s3" (default when empty) or
//	                         "gcs"; unset with no BLOB_ENDPOINT disables
//	                         skills materialization
//	BLOB_ENDPOINT            S3-compatible object storage host:port, required
//	                         by the s3 backend
//	BLOB_ACCESS_KEY / BLOB_SECRET_KEY / BLOB_BUCKET / BLOB_REGION / BLOB_TLS /
//	BLOB_BUCKET_PRECREATED   the rest of the storage config (as controlplane);
//	                         the gcs backend takes BLOB_BUCKET alone
//	SECRETS_BACKEND          secrets cipher for vault credential material
//	                         (docs/plan/12): "openbao", "local", "gcpkms", or
//	                         empty to run without one; validated at startup for
//	                         deploy parity — egress substitution itself decrypts
//	                         controlplane-side, for the per-session gate
//	BAO_ADDR / BAO_TOKEN / BAO_TRANSIT_KEY / SECRETS_MASTER_KEY / SECRETS_KEY_ID /
//	GCPKMS_KEY_NAME          the rest of the cipher config (as controlplane)
//	TAVILY_API_KEY           web_search backend key; unset leaves the tool
//	                         unconfigured (it answers is_error naming it)
//	JINA_API_KEY             web_fetch backend key; web_fetch needs this OR
//	                         WEBFETCH_BASE_URL, else it answers is_error
//	WEBSEARCH_BASE_URL / WEBFETCH_BASE_URL   Tavily-protocol / Jina-Reader-
//	                         protocol endpoints (default: the public ones)
//	WEBTOOL_ALLOWED_DOMAINS  comma-separated operator allowlist for both web
//	                         tools (bare host, IPv4, or *.wildcard); empty =
//	                         unrestricted
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional OTLP/gRPC collector endpoint
//	OTEL_EXPORTER_OTLP_INSECURE  "true" to export without TLS (default TLS)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	blobbackend "github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/backend"
	secretsbackend "github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

// splitDomains parses the comma-separated WEBTOOL_ALLOWED_DOMAINS value. Empty
// segments are dropped so a trailing comma cannot smuggle a match-nothing
// entry, and every entry must pass the allowed-hosts grammar: an out-of-grammar
// entry silently matches nothing (a typo would read as the operator's fence
// when it is really a hole in it, or a deny-all), so it fails startup instead.
func splitDomains(s string) ([]string, error) {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if err := egress.ValidateHostEntry(d); err != nil {
			return nil, fmt.Errorf("WEBTOOL_ALLOWED_DOMAINS: %w", err)
		}
		out = append(out, d)
	}
	return out, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !telemetry.Run(ctx, telemetry.Config{
		ServiceName: "executor",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	}, run) {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	cfg := executor.Config{
		Image:           os.Getenv("EXECUTOR_IMAGE"),
		Workdir:         os.Getenv("EXECUTOR_WORKDIR"),
		ControlplaneURL: os.Getenv("CONTROLPLANE_URL"),
		GateImage:       os.Getenv("EXECUTOR_GATE_IMAGE"),
		// The same OTLP config telemetry.Run reads for this executor, handed on to
		// each session's gate container so its egress spans reach the collector too.
		OTelEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTelInsecure: os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
		// The web tools' backends (docs/plan/15_web-tools.md): no TAVILY_API_KEY
		// leaves web_search unconfigured, and neither JINA_API_KEY nor
		// WEBFETCH_BASE_URL leaves web_fetch unconfigured (each answers
		// is_error naming what is missing — never a silent default egress);
		// empty base URLs resolve to the public endpoints once a tool is
		// configured. WEBTOOL_ALLOWED_DOMAINS (comma-separated; bare host,
		// IPv4, or *.wildcard) bounds both tools' hosts; empty = unrestricted.
		TavilyAPIKey:     os.Getenv("TAVILY_API_KEY"),
		JinaAPIKey:       os.Getenv("JINA_API_KEY"),
		WebSearchBaseURL: os.Getenv("WEBSEARCH_BASE_URL"),
		WebFetchBaseURL:  os.Getenv("WEBFETCH_BASE_URL"),
	}
	var err error
	if cfg.WebAllowedDomains, err = splitDomains(os.Getenv("WEBTOOL_ALLOWED_DOMAINS")); err != nil {
		return err
	}
	// The sandbox's containment (#65). Malformed configuration fails startup
	// rather than falling back to the defaults: an operator who meant to cap a
	// sandbox must not run believing they had.
	if cfg.Hardening, err = sandbox.HardeningFromEnv(); err != nil {
		return err
	}
	for env, dst := range map[string]*time.Duration{
		"EXECUTOR_LEASE_TTL": &cfg.LeaseTTL, "EXECUTOR_POLL_INTERVAL": &cfg.PollInterval,
		"EXECUTOR_REAP_INTERVAL": &cfg.ReapInterval,
	} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return errors.New(env + " must be a Go duration")
			}
			*dst = d
		}
	}
	// Unset takes the 24h default; an explicit "0" disables the idle tier
	// (Config's zero value), matching the knob's documented semantics.
	cfg.SandboxIdleTTL = 24 * time.Hour
	if v := os.Getenv("EXECUTOR_SANDBOX_IDLE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return errors.New("EXECUTOR_SANDBOX_IDLE_TTL must be a non-negative Go duration (0 disables the idle tier)")
		}
		cfg.SandboxIdleTTL = d
	}
	if v := os.Getenv("EXECUTOR_CHECKPOINT_MAX_BYTES"); v != "" {
		// The upper bound keeps cap+1 arithmetic (restore's decompression
		// budget) far from int64 overflow; 1 PiB is beyond any real spool.
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > 1<<50 {
			return errors.New("EXECUTOR_CHECKPOINT_MAX_BYTES must be a positive byte count of at most 1 PiB")
		}
		cfg.CheckpointMaxBytes = n
	}
	if err := executor.ValidateWorkdir(cfg.Workdir); err != nil {
		return fmt.Errorf("EXECUTOR_WORKDIR: %w", err)
	}

	// The pool opens before the sandbox backend because the backend takes a
	// pool-backed gate-token revoker: Reap revokes a session's token before
	// removing its containers (plan 24).
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The worst case pins two connections at once — the work loop's provision
	// holding its session lock while the reaper holds another session's —
	// and each side still issues transient queries (the gate-token mint and
	// revoke, the under-lock re-reads), so a third connection must exist for
	// those to ever proceed. Refuse less at startup instead of wedging
	// silently under load.
	if pool.Config().MaxConns < 3 {
		return fmt.Errorf("DATABASE_URL pool_max_conns = %d: the executor needs at least 3 connections", pool.Config().MaxConns)
	}

	provider, err := backend.New(backend.Config{
		Backend:             os.Getenv("SANDBOX_BACKEND"),
		GateTokenRevoker:    executor.GateTokenRevoker(pool),
		DockerHost:          os.Getenv("DOCKER_HOST"),
		DockerGateNetwork:   os.Getenv("SANDBOX_DOCKER_GATE_NETWORK"),
		K8sKubeconfig:       os.Getenv("SANDBOX_K8S_KUBECONFIG"),
		K8sContext:          os.Getenv("SANDBOX_K8S_CONTEXT"),
		K8sNamespace:        os.Getenv("SANDBOX_K8S_NAMESPACE"),
		K8sNetSetupImage:    os.Getenv("SANDBOX_K8S_NETSETUP_IMAGE"),
		K8sRuntimeClass:     os.Getenv("SANDBOX_K8S_RUNTIME_CLASS"),
		K8sNodeSelector:     os.Getenv("SANDBOX_K8S_NODE_SELECTOR"),
		K8sTolerations:      os.Getenv("SANDBOX_K8S_TOLERATIONS"),
		K8sImagePullSecrets: os.Getenv("SANDBOX_K8S_IMAGE_PULL_SECRETS"),
	})
	if err != nil {
		return err
	}

	blobs, err := blobbackend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if blobs == nil {
		slog.Info("object storage not configured; skills will not materialize")
	}

	// Constructed for startup validation only (fail fast on a misconfigured or
	// unreachable backend, matching the controlplane's wiring): egress
	// substitution decrypts controlplane-side — the gate-config endpoint —
	// never in the executor.
	cipher, err := secretsbackend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if cipher == nil {
		slog.Info("secrets cipher not configured")
	}

	slog.Info("executor running")
	return executor.New(pool, events.NewLog(pool), queue.New(pool), provider, blobs, cfg).Run(ctx)
}
