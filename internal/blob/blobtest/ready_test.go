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
// layer is still coming up — the body that failed the suite in CI (#208).
const serverNotInitialized = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>ServerNotInitialized</Code>` +
	`<Message>Server not initialized yet, please try again.</Message></Error>`

// stubMinIO stands in for a container mid-startup: its readiness endpoint
// answers 200 from the very first request — MinIO's own handler does, reporting
// a nil object layer only in a header it sets alongside the 200 — while the S3
// API refuses the first refusals requests the way a server without an object
// layer does. The counter is every S3 request the stub saw.
func stubMinIO(t *testing.T, refusals int32) (endpoint string, s3Calls *atomic.Int32) {
	t.Helper()
	var calls, remaining atomic.Int32
	remaining.Store(refusals)
	mux := http.NewServeMux()
	mux.HandleFunc("/minio/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if remaining.Add(-1) >= 0 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, serverNotInitialized)
			return
		}
		// Serving: the bucket-location lookup a client with no region
		// configured makes first, then the HEAD itself — 404, because the
		// probe bucket is one nothing creates.
		if r.URL.Query().Has("location") {
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<LocationConstraint>us-east-1</LocationConstraint>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &calls
}

// TestWaitReadyWaitsForTheObjectLayer is the regression for #208: a 200 from
// the readiness endpoint is not the readiness the suite's first call needs, so
// the gate has to keep going until an S3 request itself is answered.
func TestWaitReadyWaitsForTheObjectLayer(t *testing.T) {
	endpoint, s3Calls := stubMinIO(t, 2)
	if err := waitReady(endpoint, 30*time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if got := s3Calls.Load(); got < 3 {
		t.Errorf("gate returned after %d S3 requests; it passed the readiness "+
			"endpoint without waiting for the object layer", got)
	}
}

// TestWaitReadyFailsWhileTheObjectLayerIsDown pins the other half: a server
// that answers its readiness endpoint but never its S3 API is a failed gate,
// not a green one that hands the suite a container it cannot use.
func TestWaitReadyFailsWhileTheObjectLayerIsDown(t *testing.T) {
	endpoint, _ := stubMinIO(t, 1<<30)
	if err := waitReady(endpoint, 300*time.Millisecond); err == nil {
		t.Error("waitReady passed a server whose object layer never came up")
	}
}
