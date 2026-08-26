// Command controlplane serves the wire-compatible Managed Agents REST API:
// resource CRUD backed by Postgres. Configuration is environment-driven:
//
//	CONTROLPLANE_ADDR     listen address (default ":8080")
//	DATABASE_URL          Postgres DSN (required)
//	CONTROLPLANE_API_KEY  bootstrap management API key (required); seeded
//	                      (hashed) into api_keys at startup. Changing it and
//	                      restarting archives the previous bootstrap key. Naming
//	                      a value here makes that key env-var-managed: if the
//	                      value already exists as a console-issued key, the row
//	                      loses its issuer and any expiry it carried.
//	BLOB_BACKEND          object storage for skill archives and files: "s3"
//	                      (default when empty) or "gcs". Empty with no
//	                      BLOB_ENDPOINT deploys without object storage (the
//	                      skills upload/download routes report it); naming a
//	                      backend and omitting its configuration is an error
//	BLOB_ENDPOINT         S3-compatible object storage host:port, required by
//	                      the s3 backend
//	BLOB_ACCESS_KEY / BLOB_SECRET_KEY / BLOB_BUCKET  credentials and bucket,
//	                      required with BLOB_ENDPOINT
//	BLOB_REGION           optional bucket region
//	BLOB_TLS              "true" for https to the endpoint (default plain)
//	BLOB_BUCKET_PRECREATED  "true" when the bucket is provisioned out of band:
//	                      startup neither checks for it nor creates it, so the
//	                      identity needs object permissions only. Needs
//	                      BLOB_REGION set, or the first object request resolves
//	                      the bucket location and needs that privilege anyway
//	                      (BLOB_BACKEND=gcs is that shape unconditionally, and
//	                      takes BLOB_BUCKET alone — no credential and no
//	                      endpoint, since it authenticates with Application
//	                      Default Credentials)
//	SECRETS_BACKEND       secrets cipher for vault credential material
//	                      (docs/plan/12): "openbao", "local", "gcpkms", or
//	                      empty to deploy without one (vault credential
//	                      storage reports it)
//	BAO_ADDR / BAO_TOKEN  OpenBao transit endpoint and token, required with
//	                      SECRETS_BACKEND=openbao
//	BAO_TRANSIT_KEY       transit key name (default "map-secrets")
//	SECRETS_MASTER_KEY    base64 32-byte key, required with SECRETS_BACKEND=local
//	SECRETS_KEY_ID        key id stored beside local-cipher ciphertext
//	                      (default "local-1")
//	GCPKMS_KEY_NAME       Cloud KMS CryptoKey resource name, required with
//	                      SECRETS_BACKEND=gcpkms. No credential accompanies
//	                      it: authentication is Application Default
//	                      Credentials (Workload Identity on GKE)
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
	blobbackend "github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/backend"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/version"
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

	// Object storage for skill archives and uploaded files is optional: without
	// it the platform runs and the storage-backed skill/file routes report the
	// absence.
	blobs, err := blobbackend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if blobs == nil {
		slog.Info("object storage not configured; storage-backed skill and file routes (upload/download) will report the absence")
	}

	// The secrets cipher is optional the same way: constructing it here means a
	// misconfigured or unreachable backend fails the process at startup rather
	// than on the first credential write. The vaults API consumes it to seal
	// credential secrets; without one, the secret-bearing routes report the
	// absence and everything else serves.
	cipher, err := backend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if cipher == nil {
		slog.Info("secrets cipher not configured; vault credential storage will report the absence")
	}

	// Human authentication is optional the same way, and constructing it here is
	// what makes a misconfigured IdP a boot failure rather than a 500 on the
	// first human's first request: New performs discovery and one warming key
	// fetch. Without it (IDENTITY_MODE unset or disabled) the surface is
	// byte-for-byte what it was — no lane, no role check, machine credentials
	// unchanged.
	verifier, err := identity.FromEnv(ctx)
	if err != nil {
		return err
	}
	if verifier == nil {
		slog.Info("identity not configured; human SSO is off and management auth remains x-api-key only")
	} else {
		slog.Info("identity configured", "mode", string(verifier.Mode()))
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewHandler(pool, blobs, cipher, verifier),
		// Slow-client bounds: auth runs inside the handler, so unauthenticated
		// connections must not be able to sit open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	// Memory-version retention (#476): the only background sweep this binary
	// runs. It is hosted here because this process already holds the pool and
	// serves every memory route, and because a deployment whose environments
	// are all self_hosted runs no executor to put it in. Its statement is
	// idempotent, so a second replica costs a duplicate query and never a
	// wrong answer.
	//
	// Joined, for the reason the meter deregistration above is ordered: this
	// defer is registered after `defer pool.Close()`, so LIFO drains the sweep
	// before the pool it borrows from closes. It cancels rather than only
	// waiting, because one exit leaves ctx live — a ListenAndServe failure —
	// and waiting on a loop nothing has told to stop would hang the shutdown
	// this is here to make orderly.
	retentionCtx, stopRetention := context.WithCancel(ctx)
	retentionDone := make(chan struct{})
	go func() { defer close(retentionDone); api.StartMemoryRetention(retentionCtx, pool) }()
	defer func() { stopRetention(); <-retentionDone }()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("controlplane listening", "addr", addr, "version", version.Version)

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
	blobs, err := blobbackend.FromEnv(ctx)
	if err != nil {
		return err
	}
	if blobs == nil {
		return errors.New("the import needs object storage: configure a blob backend (BLOB_ENDPOINT and its BLOB_* companions for S3, or BLOB_BACKEND=gcs with BLOB_BUCKET)")
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
