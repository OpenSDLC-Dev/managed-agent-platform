// Command controlplane serves the wire-compatible Managed Agents REST API:
// resource CRUD backed by Postgres. Configuration is environment-driven:
//
//	CONTROLPLANE_ADDR     listen address (default ":8080")
//	DATABASE_URL          Postgres DSN (required)
//	CONTROLPLANE_API_KEY  bootstrap management API key (required); seeded
//	                      (hashed) into api_keys at startup. Changing it and
//	                      restarting revokes the previous bootstrap key.
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional OTLP/gRPC collector endpoint
//	OTEL_EXPORTER_OTLP_INSECURE  "true" to export without TLS (default TLS)
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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !telemetry.Run(ctx, telemetry.Config{
		ServiceName: "controlplane",
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
	bootKey := os.Getenv("CONTROLPLANE_API_KEY")
	if bootKey == "" {
		return errors.New("CONTROLPLANE_API_KEY is required")
	}
	addr := os.Getenv("CONTROLPLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := api.EnsureAPIKey(ctx, pool, "bootstrap", bootKey); err != nil {
		return err
	}
	// The queue depth/pending/workers_polling gauges sample the /work/stats view
	// this process already serves. telemetry.Run installed the meter provider
	// before run, so the global provider is live here; a disabled telemetry
	// config leaves a no-op provider and the registration is harmless.
	reg, err := queue.New(pool).RegisterMetrics()
	if err != nil {
		return err
	}
	// Deferred after pool.Close above, so it fires first (LIFO): the meter
	// provider's exit flush does a final collection, and the gauge callback must
	// be gone before pool.Close shuts the pool it would query.
	defer func() { _ = reg.Unregister() }()

	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewHandler(pool),
		// Slow-client bounds: auth runs inside the handler, so unauthenticated
		// connections must not be able to sit open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
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
