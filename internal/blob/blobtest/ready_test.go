package blobtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serverNotInitialized is what MinIO answers S3 requests with while its object
// layer is still coming up — the 503 that failed the suite in CI (#208), under
// the code the pinned release actually sends.
const serverNotInitialized = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>XMinioServerNotInitialized</Code>` +
	`<Message>Server not initialized yet, please try again.</Message></Error>`

// noSuchBucket is what it answers the probe's bucket-location lookup with once
// the object layer serves: the probe names a bucket nothing creates, and
// minio-go turns that 404 into a plain "no, and no error" — one round trip,
// with no HEAD after it (traced against the pinned image).
const noSuchBucket = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>NoSuchBucket</Code>` +
	`<Message>The specified bucket does not exist</Message></Error>`

// stubMinIO is a container mid-startup: the readiness endpoint answers 200 from
// the very first request — MinIO's own handler does, marking a nil object layer
// only with the header it sets alongside that 200 — while the S3 API refuses
// the first refusals requests the way a server without an object layer does.
type stubMinIO struct {
	endpoint  string
	s3Calls   atomic.Int32 // every S3 request the stub saw
	locations atomic.Int32 // how many of them were bucket-location lookups
	refusals  atomic.Int32
}

func newStubMinIO(t *testing.T, refusals int32) *stubMinIO {
	t.Helper()
	s := &stubMinIO{}
	s.refusals.Store(refusals)
	mux := http.NewServeMux()
	mux.HandleFunc("/minio/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if s.refusals.Load() > 0 {
			w.Header().Set("x-minio-server-status", "offline")
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.s3Calls.Add(1)
		if r.URL.Query().Has("location") {
			s.locations.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		if s.refusals.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, serverNotInitialized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, noSuchBucket)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.endpoint = strings.TrimPrefix(srv.URL, "http://")
	return s
}

// TestWaitReadyWaitsForTheObjectLayer is the regression for #208: a 200 from
// the readiness endpoint is not the readiness the suite's first call needs, so
// the gate has to keep going until an S3 request itself is answered.
func TestWaitReadyWaitsForTheObjectLayer(t *testing.T) {
	stub := newStubMinIO(t, 2)
	// The stub has no container behind it; an empty ID leaves the liveness
	// check inert (an unanswerable inspect is not a terminal state).
	if err := waitReady(stub.endpoint, "", 30*time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if got := stub.s3Calls.Load(); got < 3 {
		t.Errorf("gate returned after %d S3 requests; it passed the readiness "+
			"endpoint without waiting for the object layer", got)
	}
	// It has to be the request the suite's own first call makes: a client
	// built with a region configured skips the bucket-location lookup, which
	// is the one #208 died on.
	if stub.locations.Load() == 0 {
		t.Error("the probe never made a bucket-location lookup")
	}
}

// TestWaitReadyFailsWhileTheObjectLayerIsDown pins the other half: a server
// that answers its readiness endpoint but never its S3 API is a failed gate,
// not a green one that hands the suite a container it cannot use.
func TestWaitReadyFailsWhileTheObjectLayerIsDown(t *testing.T) {
	stub := newStubMinIO(t, 1<<30)
	// Three attempts' worth of budget rather than two: the last one starts a
	// full poll interval before the deadline, so the request that has to carry
	// the refusal back is not racing the context that would replace it with a
	// bare timeout. This test exists to end a flake, not to become one.
	err := waitReady(stub.endpoint, "", 600*time.Millisecond)
	if err == nil {
		t.Fatal("waitReady passed a server whose object layer never came up")
	}
	// And it has to fail on what the server said: #208 was actionable because
	// the harness printed the refusal, which a bare expired deadline hides.
	if !strings.Contains(err.Error(), "Server not initialized") {
		t.Errorf("waitReady failed with %v, want the server's own refusal", err)
	}
}
