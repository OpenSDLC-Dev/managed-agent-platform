// Command brain runs the model-turn orchestration pool: it claims model_turn
// work from the shared Postgres queue, replays session event logs into
// provider requests, and writes the turns back as Anthropic-native events.
// Stateless and horizontally scalable — run as many as needed.
// Configuration is environment-driven:
//
//	DATABASE_URL          Postgres DSN (required; same database as the
//	                      controlplane)
//	MODEL_PROVIDERS_PATH  JSON file mapping model strings to provider
//	                      endpoints (required) — see internal/provider
//	BRAIN_LEASE_TTL       work-item lease, Go duration (default "2m")
//	BRAIN_POLL_INTERVAL   idle queue poll, Go duration (default "250ms")
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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("brain exiting", "err", err)
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
	providersPath := os.Getenv("MODEL_PROVIDERS_PATH")
	if providersPath == "" {
		return errors.New("MODEL_PROVIDERS_PATH is required")
	}
	cfg := brain.Config{}
	for env, dst := range map[string]*time.Duration{
		"BRAIN_LEASE_TTL": &cfg.LeaseTTL, "BRAIN_POLL_INTERVAL": &cfg.PollInterval,
	} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return errors.New(env + " must be a Go duration")
			}
			*dst = d
		}
	}

	routes, err := provider.LoadRoutes(providersPath)
	if err != nil {
		return err
	}
	registry, err := provider.NewRegistry(routes, map[string]provider.Factory{
		"anthropic": anthropic.New,
	})
	if err != nil {
		return err
	}

	shutdownTelemetry, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "brain",
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

	slog.Info("brain running", "providers", providersPath)
	return brain.New(pool, registry, cfg).Run(ctx)
}
