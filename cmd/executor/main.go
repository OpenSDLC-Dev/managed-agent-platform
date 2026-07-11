// Command executor runs the platform-managed sandbox worker: it claims
// tool_exec work from the shared Postgres queue, runs the built-in toolset
// inside per-session Docker containers, and appends the agent.tool_result
// events the brain resumes on. Disposable "hands" — run as many as needed;
// a container dying is one tool-call error, not a lost session.
// Configuration is environment-driven:
//
//	DATABASE_URL             Postgres DSN (required; same database as the
//	                         controlplane and brain)
//	EXECUTOR_IMAGE           sandbox base image (default "debian:stable-slim")
//	EXECUTOR_WORKDIR         working directory inside the sandbox (default
//	                         "/workspace")
//	EXECUTOR_LEASE_TTL       work-item lease, Go duration (default "15m") —
//	                         must comfortably exceed a single tool's timeout
//	EXECUTOR_POLL_INTERVAL   idle queue poll, Go duration (default "500ms")
//	DOCKER_HOST              Docker daemon address (falls back to the
//	                         well-known socket)
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
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("executor exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	cfg := executor.Config{
		Image:   os.Getenv("EXECUTOR_IMAGE"),
		Workdir: os.Getenv("EXECUTOR_WORKDIR"),
	}
	for env, dst := range map[string]*time.Duration{
		"EXECUTOR_LEASE_TTL": &cfg.LeaseTTL, "EXECUTOR_POLL_INTERVAL": &cfg.PollInterval,
	} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return errors.New(env + " must be a Go duration")
			}
			*dst = d
		}
	}

	provider, err := docker.New(docker.Config{Host: os.Getenv("DOCKER_HOST")})
	if err != nil {
		return err
	}

	shutdownTelemetry, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "executor",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	})
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(flushCtx)
	}()

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	slog.Info("executor running")
	return executor.New(pool, events.NewLog(pool), queue.New(pool), provider, cfg).Run(ctx)
}
