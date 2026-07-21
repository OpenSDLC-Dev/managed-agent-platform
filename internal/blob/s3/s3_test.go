package s3_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
)

func TestMain(m *testing.M) {
	os.Exit(blobtest.Main(m))
}

func newStore(t *testing.T) blob.Store {
	t.Helper()
	tgt := blobtest.FreshTarget(t)
	s, err := s3.New(context.Background(), s3.Config{
		Endpoint:  tgt.Endpoint,
		AccessKey: tgt.AccessKey,
		SecretKey: tgt.SecretKey,
		Bucket:    tgt.Bucket,
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	return s
}

// TestContract runs the shared blob.Store suite — the definition of done for
// any backend (CLAUDE.md testing conventions).
func TestContract(t *testing.T) {
	blobtest.Run(t, newStore)
}

// TestContractWithMetrics runs the same suite through the metrics decorator:
// the wrapper must be behaviorally invisible.
func TestContractWithMetrics(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		return blob.WithMetrics(newStore(t))
	})
}

func TestNewValidatesConfig(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]s3.Config{
		"MissingEndpoint": {Bucket: "b"},
		"MissingBucket":   {Endpoint: "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s3.New(ctx, cfg); err == nil {
				t.Error("New accepted an incomplete config")
			}
		})
	}
}

func TestNewRejectsUnreachableEndpoint(t *testing.T) {
	// A refused connection must fail construction (the bucket check runs
	// eagerly), not surface later on first use.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s3.New(ctx, s3.Config{
		Endpoint:  "127.0.0.1:9", // discard port: nothing listens
		AccessKey: "x", SecretKey: "xxxxxxxx", Bucket: "b",
	})
	if err == nil {
		t.Fatal("New reached a store on an unreachable endpoint")
	}
}

func TestNewIsIdempotentPerBucket(t *testing.T) {
	// Two constructions against the same bucket: the second finds the bucket
	// the first created (the racing-creators guarantee, serialized form).
	tgt := blobtest.FreshTarget(t)
	cfg := s3.Config{Endpoint: tgt.Endpoint, AccessKey: tgt.AccessKey,
		SecretKey: tgt.SecretKey, Bucket: tgt.Bucket}
	ctx := context.Background()
	first, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	second, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("second New against the same bucket: %v", err)
	}
	// Both handles work against the shared bucket.
	if err := first.Put(ctx, "k", nil, 0, ""); err != nil {
		t.Fatalf("put via first handle: %v", err)
	}
	if _, _, err := second.Get(ctx, "k"); err != nil {
		t.Errorf("get via second handle: %v", err)
	}
}

func TestNewRejectsMalformedEndpoint(t *testing.T) {
	_, err := s3.New(context.Background(), s3.Config{
		Endpoint: "http://scheme-not-allowed:9000", // Config wants host:port
		Bucket:   "b",
	})
	if err == nil {
		t.Fatal("New accepted a malformed endpoint")
	}
}

func TestEmptyKeyOpsSurfaceErrors(t *testing.T) {
	// The contract types keys as non-empty; the backend must turn a violation
	// into an error, and never into blob.ErrNotFound (absence of an object
	// that could not exist is a caller bug, not a miss).
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put with empty key succeeded")
	}
	if _, _, err := s.Get(ctx, ""); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get with empty key = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, ""); err == nil {
		t.Error("delete with empty key succeeded")
	}
}

func TestOpsAgainstRemovedBucketAreErrorsNotAbsence(t *testing.T) {
	// A vanished bucket is an operator incident, not an empty store: every op
	// errors, and Get's error must not read as blob.ErrNotFound.
	tgt := blobtest.FreshTarget(t)
	cfg := s3.Config{Endpoint: tgt.Endpoint, AccessKey: tgt.AccessKey,
		SecretKey: tgt.SecretKey, Bucket: tgt.Bucket}
	ctx := context.Background()
	s, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	// Remove the bucket out from under the store with a raw client — the
	// backend's own wire, not its API.
	raw, err := minio.New(tgt.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(tgt.AccessKey, tgt.SecretKey, ""),
	})
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	if err := raw.RemoveBucket(ctx, tgt.Bucket); err != nil {
		t.Fatalf("remove bucket: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put into a removed bucket succeeded")
	}
	if _, _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get from a removed bucket = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete in a removed bucket succeeded")
	}
}

func TestBadCredentialsSurfaceAsErrors(t *testing.T) {
	// Wrong secret: construction fails at the bucket check with a real error,
	// not blob.ErrNotFound (an auth failure must never read as absence).
	tgt := blobtest.FreshTarget(t)
	_, err := s3.New(context.Background(), s3.Config{
		Endpoint:  tgt.Endpoint,
		AccessKey: tgt.AccessKey,
		SecretKey: "wrong-secret-key",
		Bucket:    tgt.Bucket,
	})
	if err == nil {
		t.Fatal("New succeeded with bad credentials")
	}
	if errors.Is(err, blob.ErrNotFound) {
		t.Error("auth failure mapped to blob.ErrNotFound")
	}
}
