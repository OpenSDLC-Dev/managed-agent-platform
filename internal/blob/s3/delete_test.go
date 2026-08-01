package s3_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
)

// stubStore builds a Store against a stub endpoint that answers the bucket
// check s3.New makes and gives every DELETE the supplied status and S3 error
// code. It exists because the MinIO-backed contract suite cannot produce the
// condition these tests are about: MinIO and AWS S3 answer a DELETE of a
// missing object with 204, so only a stub can say otherwise.
func stubStore(t *testing.T, deleteStatus int, deleteCode string) blob.Store {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead: // the bucket check in s3.New
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(deleteStatus)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+
				deleteCode+`</Code><Message>stub</Message></Error>`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	// Region is set so the client signs without a bucket-location round-trip.
	s, err := s3.New(context.Background(), s3.Config{
		Endpoint:  strings.TrimPrefix(srv.URL, "http://"),
		AccessKey: "stub", SecretKey: "stubsecret",
		Bucket: "stub-bucket", Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("s3.New against the stub: %v", err)
	}
	return s
}

// TestDeleteConvergesWhenTheEndpointReportsNoSuchKey pins blob.Store's
// convergence requirement — a crashed-and-retried delete must converge, not
// flap — against GCS's XML API, which answers a DELETE of a missing object
// with 404 NoSuchKey where S3 and MinIO answer 204. Measured against real GCS;
// see docs/plan/20_gcp-deployment.md, "Ground truth".
func TestDeleteConvergesWhenTheEndpointReportsNoSuchKey(t *testing.T) {
	if err := stubStore(t, http.StatusNotFound, "NoSuchKey").Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("delete of a missing key = %v, want nil", err)
	}
}

// TestDeleteSurfacesOtherEndpointErrors keeps that mapping narrow: only the
// object's absence is success. A denied delete is still a failure.
func TestDeleteSurfacesOtherEndpointErrors(t *testing.T) {
	if err := stubStore(t, http.StatusForbidden, "AccessDenied").Delete(context.Background(), "k"); err == nil {
		t.Fatal("a denied delete reported success")
	}
}
