package backend_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/backend"
)

// The happy paths that need a live service are deliberately absent. The s3 arm's
// is the blob/s3 suite's, against a real MinIO; the gcs arm's needs Application
// Default Credentials and lives in the live tier (internal/blob/gcs). What is
// here is everything the selector decides on its own.

func TestUnconfiguredIsNoStore(t *testing.T) {
	// Object storage is optional, and each binary decides what its absence
	// means — so an unset environment must answer "no store", not an error.
	store, err := backend.FromEnv(context.Background())
	if err != nil {
		t.Fatalf("FromEnv with nothing set: %v", err)
	}
	if store != nil {
		t.Error("FromEnv built a store from an empty environment")
	}
}

func TestExplicitS3WithoutEndpointIsAnError(t *testing.T) {
	// Naming a backend is asking for it. An unset BLOB_ENDPOINT means "no object
	// storage" only for a deployment that named nothing — asking for S3 and
	// omitting the one thing S3 cannot be reached without is a misconfiguration,
	// and silently running without storage is the failure this rule prevents.
	t.Setenv("BLOB_BACKEND", "s3")
	store, err := backend.FromEnv(context.Background())
	if err == nil {
		t.Fatal("FromEnv accepted BLOB_BACKEND=s3 with no endpoint")
	}
	if !strings.Contains(err.Error(), "BLOB_ENDPOINT") {
		t.Errorf("error %q does not name the missing variable", err)
	}
	if store != nil {
		t.Error("FromEnv returned a store alongside the error")
	}
}

func TestUnknownBackendIsAnError(t *testing.T) {
	t.Setenv("BLOB_BACKEND", "azure")
	_, err := backend.FromEnv(context.Background())
	if err == nil {
		t.Fatal("FromEnv accepted an unknown backend")
	}
	// The value the operator typed has to appear, or the message cannot be
	// acted on.
	if !strings.Contains(err.Error(), "azure") {
		t.Errorf("error %q does not name the rejected value", err)
	}
}

func TestGCSNeedsABucket(t *testing.T) {
	// Selecting a backend is itself the statement that object storage is
	// configured, so a missing bucket fails rather than degrading to no store.
	t.Setenv("BLOB_BACKEND", "gcs")
	store, err := backend.FromEnv(context.Background())
	if err == nil {
		t.Fatal("FromEnv accepted BLOB_BACKEND=gcs with no bucket")
	}
	if !strings.Contains(err.Error(), "BLOB_BUCKET") {
		t.Errorf("error %q does not name the missing variable", err)
	}
	if store != nil {
		t.Error("FromEnv returned a store alongside the error")
	}
}

// TestS3ReadsBucketPrecreated covers the activation path a deployment uses for
// the pre-created-bucket mode (#241): the whole feature is one environment read,
// and without a test here it could be deleted with every other test still green.
// The endpoint is the discard port, so construction can only succeed by making
// no request — which is what the mode promises.
func TestS3ReadsBucketPrecreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Setenv("BLOB_ENDPOINT", "127.0.0.1:9")
	t.Setenv("BLOB_BUCKET", "map-blob")
	t.Setenv("BLOB_REGION", "us-east-1")
	t.Setenv("BLOB_BUCKET_PRECREATED", "true")
	store, err := backend.FromEnv(ctx)
	if err != nil {
		t.Fatalf("FromEnv in the pre-created mode: %v", err)
	}
	if store == nil {
		t.Fatal("FromEnv returned no store")
	}
}

// TestS3WithoutBucketPrecreatedChecksTheBucket is the other half: the same
// environment minus the one variable must still reach the endpoint, or the test
// above would pass for a reason that has nothing to do with the mode.
func TestS3WithoutBucketPrecreatedChecksTheBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Setenv("BLOB_ENDPOINT", "127.0.0.1:9")
	t.Setenv("BLOB_BUCKET", "map-blob")
	t.Setenv("BLOB_REGION", "us-east-1")
	if _, err := backend.FromEnv(ctx); err == nil {
		t.Fatal("FromEnv reached a store on the discard port without the mode")
	}
}
