// Command controlplane serves the wire-compatible Managed Agents REST API:
// resource CRUD backed by Postgres. Configuration is environment-driven:
//
//	CONTROLPLANE_ADDR     listen address (default ":8080")
//	DATABASE_URL          Postgres DSN (required)
//	CONTROLPLANE_API_KEY  bootstrap management API key (required); seeded
//	                      (hashed) into api_keys at startup. Changing it and
//	                      restarting revokes the previous bootstrap key.
//	BLOB_ENDPOINT         S3-compatible object storage host:port for skill
//	                      archives; empty deploys without object storage
//	                      (the skills upload/download routes report it)
//	BLOB_ACCESS_KEY / BLOB_SECRET_KEY / BLOB_BUCKET  credentials and bucket,
//	                      required with BLOB_ENDPOINT
//	BLOB_REGION           optional bucket region
//	BLOB_TLS              "true" for https to the endpoint (default plain)
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional OTLP/gRPC collector endpoint
//	OTEL_EXPORTER_OTLP_INSECURE  "true" to export without TLS (default TLS)
//
// Run-once operator import (docs/plan/06_skills.md slice 3): with
// -import-anthropic-skills pointing at a local checkout of
// github.com/anthropics/skills, the binary imports the -import-skills
// directories as anthropic-source skills (validated exactly like uploads,
// date-based version from the checkout's last commit unless -import-version
// overrides) and exits instead of serving. Needs DATABASE_URL and the BLOB_*
// object storage; CONTROLPLANE_API_KEY is not required in this mode.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

var (
	importCheckout = flag.String("import-anthropic-skills", "",
		"run-once mode: path to a local checkout of github.com/anthropics/skills; import the -import-skills directories, then exit")
	importVersion = flag.String("import-version", "",
		"date version for the import (digits, YYYYMMDD; default: the checkout's last commit date via git)")
	importSkills = flag.String("import-skills", "docx,pdf,pptx,xlsx",
		"comma-separated skill directory names under <checkout>/skills to import")
)

func main() {
	flag.Parse()
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
	if *importCheckout != "" {
		return runImport(ctx, dsn)
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

	// Object storage for skill archives is optional: without it the platform
	// runs and the storage-backed skill routes report the absence.
	blobs, err := s3.FromEnv(ctx)
	if err != nil {
		return err
	}
	if blobs == nil {
		slog.Info("object storage not configured; skills are unavailable")
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewHandler(pool, blobs),
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

// runImport is the run-once operator import: validate + land the named skill
// directories from the checkout, report the summary, exit.
func runImport(ctx context.Context, dsn string) error {
	blobs, err := s3.FromEnv(ctx)
	if err != nil {
		return err
	}
	if blobs == nil {
		return errors.New("the import needs object storage: set BLOB_ENDPOINT (and its BLOB_* companions)")
	}
	version := *importVersion
	if version == "" {
		if version, err = checkoutCommitDate(ctx, *importCheckout); err != nil {
			return fmt.Errorf("resolve the checkout's commit date (pass -import-version to override): %w", err)
		}
	}
	var dirs []string
	for _, name := range strings.Split(*importSkills, ",") {
		if name = strings.TrimSpace(name); name != "" {
			dirs = append(dirs, filepath.Join(*importCheckout, "skills", name))
		}
	}
	if len(dirs) == 0 {
		return errors.New("-import-skills named no directories")
	}
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	sum, err := api.ImportAnthropicSkills(ctx, pool, blobs, dirs, version)
	fmt.Printf("imported %d, skipped %d, failed %d (version %s)\n",
		len(sum.Imported), len(sum.Skipped), len(sum.Failed), version)
	return err
}

// checkoutCommitDate reads the checkout's last commit date as the default
// YYYYMMDD import version.
func checkoutCommitDate(ctx context.Context, checkout string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", checkout,
		"log", "-1", "--format=%cd", "--date=format:%Y%m%d").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
