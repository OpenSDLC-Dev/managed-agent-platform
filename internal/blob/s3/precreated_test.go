package s3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
)

// precreatedRegion is what these tests configure. Any value would do for the
// stub, and MinIO accepts this one; what matters is that a region is set at
// all, which is what keeps minio-go from resolving the bucket's location.
const precreatedRegion = "us-east-1"

// rawClient is the endpoint's own wire rather than the backend's API — used to
// pre-create a bucket the store is told about, and to ask afterwards whether the
// store created one it was told not to.
func rawClient(t *testing.T, tgt blobtest.Target) *minio.Client {
	t.Helper()
	c, err := minio.New(tgt.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(tgt.AccessKey, tgt.SecretKey, ""),
	})
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	return c
}

// recordingEndpoint answers every request with answer and reports the requests
// it saw, so a test can assert what the store asked the endpoint for — the only
// way to prove a permission is not needed is to prove the call is not made.
func recordingEndpoint(t *testing.T, answer http.HandlerFunc) (endpoint string, seen func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		req := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			req += "?" + r.URL.RawQuery
		}
		got = append(got, req)
		mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func precreatedConfig(endpoint, bucket string) s3.Config {
	return s3.Config{
		Endpoint:         endpoint,
		AccessKey:        "stub",
		SecretKey:        "stubsecret",
		Bucket:           bucket,
		Region:           precreatedRegion,
		BucketPrecreated: true,
	}
}

// TestPrecreatedBucketIssuesNoBucketRequest is the point of the mode (#241):
// an identity holding object permissions only must be able to construct the
// store, and it cannot if construction asks the endpoint about the bucket. The
// endpoint here refuses everything, so the only way New can succeed is by
// asking for nothing.
func TestPrecreatedBucketIssuesNoBucketRequest(t *testing.T) {
	endpoint, seen := recordingEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	s, err := s3.New(context.Background(), precreatedConfig(endpoint, "stub-bucket"))
	if err != nil {
		t.Fatalf("New against an endpoint that refuses bucket work: %v", err)
	}
	if s == nil {
		t.Fatal("New returned no store")
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("endpoint saw %v, want no request at all", got)
	}
}

// TestPrecreatedBucketObjectWorkIssuesOnlyObjectRequests is the other half of
// the same promise, and the half a skipped construction check does not buy on
// its own: with no region configured minio-go resolves the bucket's location
// before the first object request (bucket-cache.go), which is a bucket-level
// read of exactly the kind this mode exists to stop needing
// (docs/plan/20_gcp-deployment.md Decision 11 measured it as the second of the
// two bucket calls). Requiring a region is what makes "object permissions only"
// true rather than nearly true, and this pins that no bucket-level request
// escapes for ordinary object work.
func TestPrecreatedBucketObjectWorkIssuesOnlyObjectRequests(t *testing.T) {
	endpoint, seen := recordingEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// A body for the read, with the object metadata minio-go insists
			// on parsing off a GET answer.
			w.Header().Set("Content-Length", "2")
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"49f68a5c8493ec2c0bf489821c21fc3b"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("hi"))
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent) // what S3 answers a delete with
			return
		}
		w.Header().Set("ETag", `"49f68a5c8493ec2c0bf489821c21fc3b"`)
		w.WriteHeader(http.StatusOK)
	})
	s, err := s3.New(context.Background(), precreatedConfig(endpoint, "stub-bucket"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "k", strings.NewReader("hi"), 2, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, _, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = rc.Close()
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{"PUT /stub-bucket/k", "GET /stub-bucket/k", "DELETE /stub-bucket/k"}
	got := seen()
	if len(got) != len(want) {
		t.Fatalf("endpoint saw %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint saw %v, want exactly %v", got, want)
		}
	}
}

// TestPrecreatedBucketRequiresARegion keeps that guarantee from resting on the
// operator remembering it: without a region the mode would trade a bucket call
// at startup for a bucket call at first use, which is not what it claims to do.
func TestPrecreatedBucketRequiresARegion(t *testing.T) {
	endpoint, seen := recordingEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, err := s3.New(context.Background(), s3.Config{
		Endpoint:         endpoint,
		AccessKey:        "stub",
		SecretKey:        "stubsecret",
		Bucket:           "stub-bucket",
		BucketPrecreated: true,
	})
	if err == nil {
		t.Fatal("New accepted the pre-created-bucket mode with no region")
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("endpoint saw %v, want the rejection to be local", got)
	}
}

// TestPrecreatedBucketDoesNotCreateTheBucket proves the skip against a real
// endpoint: told the bucket is already there, the store must not make one. The
// bucket name is fresh, so nothing but New could have created it.
func TestPrecreatedBucketDoesNotCreateTheBucket(t *testing.T) {
	tgt := blobtest.FreshTarget(t)
	ctx := context.Background()
	if _, err := s3.New(ctx, s3.Config{
		Endpoint:         tgt.Endpoint,
		AccessKey:        tgt.AccessKey,
		SecretKey:        tgt.SecretKey,
		Bucket:           tgt.Bucket,
		Region:           precreatedRegion,
		BucketPrecreated: true,
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	exists, err := rawClient(t, tgt).BucketExists(ctx, tgt.Bucket)
	if err != nil {
		t.Fatalf("bucket check: %v", err)
	}
	if exists {
		t.Error("New created the bucket it was told was already there")
	}
}

// TestContractWithPrecreatedBucket runs the shared suite in the new mode: the
// mode changes what construction asks of the endpoint, and nothing about what
// the store then does (CLAUDE.md: every backend passes the same suite).
func TestContractWithPrecreatedBucket(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		t.Helper()
		tgt := blobtest.FreshTarget(t)
		ctx := context.Background()
		// The deployment's pre-created bucket, stood in for by the operator the
		// mode assumes provisioned it out of band.
		if err := rawClient(t, tgt).MakeBucket(ctx, tgt.Bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("pre-create bucket: %v", err)
		}
		s, err := s3.New(ctx, s3.Config{
			Endpoint:         tgt.Endpoint,
			AccessKey:        tgt.AccessKey,
			SecretKey:        tgt.SecretKey,
			Bucket:           tgt.Bucket,
			Region:           precreatedRegion,
			BucketPrecreated: true,
		})
		if err != nil {
			t.Fatalf("s3.New: %v", err)
		}
		return s
	})
}

// TestPrecreatedBucketStillValidatesConfig keeps the mode from becoming a way
// to skip the checks that need no endpoint at all.
func TestPrecreatedBucketStillValidatesConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, cfg := range map[string]s3.Config{
		"MissingEndpoint":   {Bucket: "b", Region: precreatedRegion, BucketPrecreated: true},
		"MissingBucket":     {Endpoint: "127.0.0.1:1", Region: precreatedRegion, BucketPrecreated: true},
		"MalformedEndpoint": {Endpoint: "http://scheme-not-allowed:9000", Bucket: "b", Region: precreatedRegion, BucketPrecreated: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s3.New(ctx, cfg); err == nil {
				t.Error("New accepted an unusable config")
			}
		})
	}
}
