// Command modelgateway serves the model gateway (docs/plan/59_model-gateway.md):
// Anthropic Messages for the platform's API keys, routed to the vendor
// deployments the catalogue configures, and the /admin/v1/ API that
// configures it. It is stateless; any number of replicas share one database.
// Configuration is environment-driven:
//
//	MODELGATEWAY_ADDR      listen address (default ":8090")
//	DATABASE_URL           Postgres DSN of the platform's database (required):
//	                       its api_keys check a caller, its modelgateway schema
//	                       holds the catalogue, and the platform's migrations
//	                       run at start as every binary runs them
//	CONTROLPLANE_API_KEY   the bootstrap key the control plane registers
//	                       (required): it reaches the admin API, and while its
//	                       row is active calls any model unless a key policy
//	                       written for it says less
//	BRAIN_API_KEY          the brain's platform key, which the control plane
//	                       registers as "brain" (optional): while its row is
//	                       active it calls any model unless a key policy
//	                       written for it says less, and it never reaches the
//	                       admin API. It must differ from CONTROLPLANE_API_KEY
//	                       and carry no surrounding whitespace
//	                       (apikey.CheckBrainKey)
//	SECRETS_BACKEND        the cipher sealing vendor credentials (required):
//	                       "openbao", "local" or "gcpkms", with the variables
//	                       cmd/controlplane documents for each
//	IDENTITY_*             optional operator sign-in for the admin API, as the
//	                       control plane reads it
//	MODELGATEWAY_RELOAD_INTERVAL  how often each replica rereads the catalogue
//	                       whatever its notifications say, a Go duration
//	                       (default "30s"): a missed notification costs at
//	                       most this
//	MODELGATEWAY_MAX_ATTEMPTS  upstream calls one request may make to each
//	                       deployment before it falls back to the next, all
//	                       before its answer begins (default 3)
//	MODELGATEWAY_USAGE_RETENTION  how long the per-request ledger keeps a
//	                       row, a Go duration (default "2160h", 90 days); a
//	                       sweep on any replica deletes older rows each hour,
//	                       and the daily rollups stay
//	OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_INSECURE  as the other binaries
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	mgstore "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if !telemetry.Run(ctx, telemetry.Config{
		ServiceName: "modelgateway",
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
	addr := os.Getenv("MODELGATEWAY_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	reload := 30 * time.Second
	if v := os.Getenv("MODELGATEWAY_RELOAD_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("MODELGATEWAY_RELOAD_INTERVAL: %q is not a positive duration", v)
		}
		reload = d
	}
	attempts := 0
	if v := os.Getenv("MODELGATEWAY_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("MODELGATEWAY_MAX_ATTEMPTS: %q is not a positive integer", v)
		}
		attempts = n
	}
	retention := 90 * 24 * time.Hour
	if v := os.Getenv("MODELGATEWAY_USAGE_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("MODELGATEWAY_USAGE_RETENTION: %q is not a positive duration", v)
		}
		retention = d
	}

	// Every dependency is checked here, so a bad configuration or an
	// unreachable dependency is a failed start rather than a failed request.
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	cipher, err := backend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if cipher == nil {
		return errors.New("SECRETS_BACKEND is required: the gateway seals and opens vendor credentials")
	}
	verifier, err := identity.FromEnv(ctx)
	if err != nil {
		return err
	}
	if verifier == nil {
		slog.Info("identity not configured; the admin API takes the bootstrap key only")
	}
	cat, err := catalog.New(ctx, pool, reload)
	if err != nil {
		return err
	}
	st := mgstore.New(pool)
	adminAPI, err := admin.New(admin.Config{Store: st, Cipher: cipher, Verifier: verifier, BootstrapKey: bootKey})
	if err != nil {
		return err
	}
	gateway, err := modelgateway.New(modelgateway.Config{Catalog: cat, Store: st, Keys: pool, Cipher: cipher, BootstrapKey: bootKey,
		BrainKey: os.Getenv("BRAIN_API_KEY"), Admin: adminAPI, MaxAttempts: attempts})
	if err != nil {
		return err
	}

	// Joined before the pool closes (defers run last-in first-out), and
	// cancelled rather than only awaited, since a ListenAndServe failure
	// leaves ctx live.
	reloadCtx, stopReload := context.WithCancel(ctx)
	reloadDone := make(chan struct{})
	go func() { defer close(reloadDone); cat.Run(reloadCtx) }()
	defer func() { stopReload(); <-reloadDone }()
	sweepCtx, stopSweep := context.WithCancel(ctx)
	sweepDone := make(chan struct{})
	go func() { defer close(sweepDone); st.RunRetention(sweepCtx, time.Hour, retention) }()
	defer func() { stopSweep(); <-sweepDone }()

	srv := &http.Server{
		Addr:    addr,
		Handler: gateway,
		// Slow-client bounds on the request side only: an answer streams for
		// as long as the model writes, which the stall guard bounds instead.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("modelgateway listening", "addr", addr, "version", version.Version)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
