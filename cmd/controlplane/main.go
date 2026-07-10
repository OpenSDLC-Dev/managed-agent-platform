// Command controlplane serves the wire-compatible Managed Agents REST API:
// resource CRUD backed by Postgres. Configuration is environment-driven:
//
//	CONTROLPLANE_ADDR     listen address (default ":8080")
//	DATABASE_URL          Postgres DSN (required)
//	CONTROLPLANE_API_KEY  bootstrap management API key (required); seeded
//	                      (hashed) into api_keys at startup
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional OTLP/gRPC collector endpoint
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("controlplane exiting", "err", err)
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
	bootKey := os.Getenv("CONTROLPLANE_API_KEY")
	if bootKey == "" {
		return errors.New("CONTROLPLANE_API_KEY is required")
	}
	addr := os.Getenv("CONTROLPLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	shutdownTelemetry, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "controlplane",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:    true,
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
	if err := api.EnsureAPIKey(ctx, pool, "bootstrap", bootKey); err != nil {
		return err
	}

	srv := &http.Server{Addr: addr, Handler: api.NewHandler(pool)}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("controlplane listening", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
