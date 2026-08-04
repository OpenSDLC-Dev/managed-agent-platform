package gcs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/gcs"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/gcs/gcstest"
)

func TestMain(m *testing.M) {
	os.Exit(gcstest.Main(m))
}

func newStore(t *testing.T) blob.Store {
	t.Helper()
	client, bucket := gcstest.FreshBucket(t)
	s, err := gcs.New(context.Background(), gcs.Config{Bucket: bucket, Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
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

func TestNewRequiresBucket(t *testing.T) {
	if _, err := gcs.New(context.Background(), gcs.Config{}); err == nil {
		t.Error("New accepted a config with no bucket")
	}
}

// TestNewIssuesNoRequest is this backend's whole permission story: it holds
// object permissions and nothing bucket-level, which it can only do by never
// asking about the bucket. The endpoint here refuses everything, so the only way
// New can succeed is by asking nothing.
func TestNewIssuesNoRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	if _, err := gcs.New(context.Background(), gcs.Config{Bucket: "b", Client: client}); err != nil {
		t.Fatalf("New against an endpoint that refuses everything: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("endpoint saw %v, want no request at all", seen)
	}
}

func TestPutShortReaderErrors(t *testing.T) {
	// The contract promises "exactly size bytes": a reader that runs dry before
	// size must be an error, never a silently truncated object.
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "short", strings.NewReader("abc"), 10, ""); err == nil {
		t.Fatal("put with a short reader succeeded")
	}
	// ...and it must not have committed anything either.
	if _, _, err := s.Get(ctx, "short"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get after a short put = %v, want blob.ErrNotFound", err)
	}
}

func TestPutIgnoresBytesBeyondSize(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "trimmed", strings.NewReader("abcdefghij"), 3, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, size, err := s.Get(ctx, "trimmed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	if size != 3 {
		t.Errorf("stored size = %d, want 3", size)
	}
}

func TestEmptyKeyOpsSurfaceErrors(t *testing.T) {
	// The contract types keys as non-empty; the backend must turn a violation
	// into an error, and never into blob.ErrNotFound (absence of an object that
	// could not exist is a caller bug, not a miss). The client rejects the name
	// itself — measured against real Cloud Storage, which answers all three with
	// "storage: object name is empty" rather than its absence sentinel.
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

// TestMissingBucketIsAnErrorNotAbsence is the point of the absence probe. GCS
// gives the read path one sentinel for two states, so without the probe a
// mistyped bucket name would read as an empty store and the platform would
// report every skill archive and every file as simply not there. The S3 backend
// keeps a vanished bucket an error (TestOpsAgainstRemovedBucketAreErrorsNotAbsence);
// this holds the same line without a bucket-level permission.
func TestMissingBucketIsAnErrorNotAbsence(t *testing.T) {
	client, _ := gcstest.FreshBucket(t)
	ctx := context.Background()
	s, err := gcs.New(ctx, gcs.Config{Bucket: "gcstest-never-created", Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if _, _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get from a missing bucket = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete in a missing bucket reported success")
	}
	if err := s.Put(ctx, "k", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put into a missing bucket succeeded")
	}
}

// TestPutPersistsContentType pins what the Store interface never reads back —
// content-type exists for HTTP consumers — with the endpoint's own metadata, or
// the impl could drop it and every other test would stay green.
func TestPutPersistsContentType(t *testing.T) {
	client, bucket := gcstest.FreshBucket(t)
	ctx := context.Background()
	s, err := gcs.New(ctx, gcs.Config{Bucket: bucket, Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if err := s.Put(ctx, "typed", strings.NewReader("zip!"), 4, "application/zip"); err != nil {
		t.Fatalf("put: %v", err)
	}
	attrs, err := client.Bucket(bucket).Object("typed").Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.ContentType != "application/zip" {
		t.Errorf("stored content-type = %q, want %q", attrs.ContentType, "application/zip")
	}
}

// TestFailedOverwriteLeavesThePreviousObject is the atomicity the writer's
// commit-on-Close gives us: a put that errors mid-stream must not have replaced
// what was there.
func TestFailedOverwriteLeavesThePreviousObject(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "k", strings.NewReader("the original"), 12, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader("short"), 99, ""); err == nil {
		t.Fatal("put with a short reader succeeded")
	}
	rc, size, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get after the failed overwrite: %v", err)
	}
	defer rc.Close()
	if size != 12 {
		t.Errorf("size = %d, want the original 12", size)
	}
}

// liveStore namespaces the shared suite's fixed keys and removes what it wrote.
// The suite names keys like "k" and "large" because every other backend hands it
// a throwaway bucket; the live tier runs against one real bucket an operator
// owns, so without this two runs would overwrite each other and every run would
// leave objects behind.
type liveStore struct {
	blob.Store
	prefix  string
	mu      sync.Mutex
	written map[string]bool
}

func (s *liveStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	s.mu.Lock()
	s.written[key] = true
	s.mu.Unlock()
	return s.Store.Put(ctx, s.prefix+key, r, size, contentType)
}

func (s *liveStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return s.Store.Get(ctx, s.prefix+key)
}

func (s *liveStore) Delete(ctx context.Context, key string) error {
	return s.Store.Delete(ctx, s.prefix+key)
}

// TestLiveContract runs the same shared suite against real Cloud Storage, under
// Application Default Credentials — the tier that keeps the hermetic fake
// honest. Opt in with RUN_LIVE_GCS_TESTS=1 and a GCS_BUCKET the credentials can
// reach; without it the suite skips and Cloud Storage is never called.
func TestLiveContract(t *testing.T) {
	bucket := gcstest.LiveBucket(t)
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("live client (Application Default Credentials): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var run atomic.Int64
	blobtest.Run(t, func(t *testing.T) blob.Store {
		t.Helper()
		s, err := gcs.New(ctx, gcs.Config{Bucket: bucket, Client: client})
		if err != nil {
			t.Fatalf("gcs.New: %v", err)
		}
		live := &liveStore{Store: s, written: map[string]bool{},
			prefix: fmt.Sprintf("gcstest-live/%d-%d/", time.Now().UnixNano(), run.Add(1))}
		t.Cleanup(func() {
			live.mu.Lock()
			defer live.mu.Unlock()
			for key := range live.written {
				if err := s.Delete(context.Background(), live.prefix+key); err != nil {
					t.Errorf("cleanup %s: %v", live.prefix+key, err)
				}
			}
		})
		return live
	})
}
