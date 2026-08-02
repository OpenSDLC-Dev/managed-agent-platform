package s3_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
)

// stubStore builds a Store against a stub S3 endpoint that answers the bucket
// check s3.New makes, hands every other request to answer, and reports the
// requests the endpoint saw. It exists because the MinIO-backed contract suite
// cannot produce the conditions this file is about: a real endpoint answers a
// missing object with its own error document, so only a stub can say anything
// else — and MinIO and AWS S3 answer a DELETE of a missing object with 204, so
// only a stub can answer that one at all.
func stubStore(t *testing.T, answer http.HandlerFunc) (blob.Store, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stub-bucket/" { // the bucket check in s3.New
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		answer(w, r)
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
	return s, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// s3Error answers with a well-formed S3 error document, the shape a real
// endpoint sends and the only one whose code is the server's own word.
func s3Error(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+
			code+`</Code><Message>stub</Message></Error>`)
	}
}

// bodiesWithoutAnErrorDocument are the 404 bodies that are not the endpoint's
// own word. minio-go synthesizes NoSuchKey from the status alone whenever the
// body does not decode as an S3 error document, so an operation reading the
// code without demanding the document would report every one of these as
// absence.
var bodiesWithoutAnErrorDocument = map[string]string{
	"Bodyless":  "",
	"ProxyPage": "<html><head><title>404 Not Found</title></head></html>",
	// A document that starts <Error> and then breaks. encoding/xml has already
	// assigned XMLName by then, so this is the shape that would slip through if
	// minio-go did not replace the whole struct when the decode fails — the
	// reason the marker can be trusted at all.
	"TruncatedDocument": "<Error><Code>NoSuchKey</Code>",
}

// notFoundWithBody answers 404 carrying exactly body.
func notFoundWithBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, body)
	}
}

// TestDeleteConvergesWhenTheEndpointReportsNoSuchKey pins blob.Store's
// convergence requirement — a crashed-and-retried delete must converge, not
// flap — against GCS's XML API, which answers a DELETE of a missing object
// with 404 NoSuchKey where S3 and MinIO answer 204. Measured against real GCS;
// see docs/plan/20_gcp-deployment.md, "Ground truth".
func TestDeleteConvergesWhenTheEndpointReportsNoSuchKey(t *testing.T) {
	s, requests := stubStore(t, s3Error(http.StatusNotFound, "NoSuchKey"))
	if err := s.Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("delete of a missing key = %v, want nil", err)
	}
	// Converging is only convergence if the object was actually asked for: a
	// Delete that stopped issuing the request would satisfy the line above.
	if got := requests(); len(got) != 1 || got[0] != "DELETE /stub-bucket/gone" {
		t.Errorf("endpoint saw %v, want exactly [DELETE /stub-bucket/gone]", got)
	}
}

// TestDeleteSurfacesOtherEndpointErrors keeps that mapping narrow: only the
// object's absence is success. A denied delete is still a failure.
func TestDeleteSurfacesOtherEndpointErrors(t *testing.T) {
	s, _ := stubStore(t, s3Error(http.StatusForbidden, "AccessDenied"))
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Fatal("a denied delete reported success")
	}
}

// TestDeleteSurfacesAMissingBucket is the same narrowness at the status the
// mapping does accept: a vanished bucket also answers 404, and its own code is
// what keeps it an error. TestOpsAgainstRemovedBucketAreErrorsNotAbsence
// proves this against a real MinIO; this pins it where the answer is dictated.
func TestDeleteSurfacesAMissingBucket(t *testing.T) {
	s, _ := stubStore(t, s3Error(http.StatusNotFound, "NoSuchBucket"))
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Fatal("a delete into a missing bucket reported success")
	}
}

// TestDeleteRejectsNoSuchKeyOutsideANotFound covers one path by which the code
// can arrive detached from the document: minio-go lets an x-minio-error-code
// response header overwrite the parsed code whatever the response was, so a
// server in that dialect could label an arbitrary failure NoSuchKey. Absence
// answers 404 and nothing else, so the mapping requires both.
func TestDeleteRejectsNoSuchKeyOutsideANotFound(t *testing.T) {
	s, _ := stubStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-minio-error-code", "NoSuchKey")
		w.WriteHeader(http.StatusForbidden)
	})
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Fatal("a NoSuchKey code on a 403 reported success")
	}
}

// TestAbsenceRejectsAHeaderContradictingItsErrorDocument covers the other such
// path — the one the status cannot catch. minio-go applies x-minio-error-code
// *after* a successful decode, so a 404 whose body says NoSuchBucket and whose
// header says NoSuchKey arrives as NoSuchKey with the document marker intact,
// satisfying every conjunct of the absence check with a response that says, in
// the endpoint's own document, that the bucket is gone. Keeping that document
// authoritative over the header is what makes the check hold (#244).
func TestAbsenceRejectsAHeaderContradictingItsErrorDocument(t *testing.T) {
	answer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-minio-error-code", "NoSuchKey")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
			`<Error><Code>NoSuchBucket</Code><Message>stub</Message></Error>`)
	}
	t.Run("Delete", func(t *testing.T) {
		s, _ := stubStore(t, answer)
		if err := s.Delete(context.Background(), "k"); err == nil {
			t.Fatal("a delete into a bucket the endpoint called missing reported success")
		}
	})
	t.Run("Get", func(t *testing.T) {
		s, _ := stubStore(t, answer)
		_, _, err := s.Get(context.Background(), "k")
		if err == nil || errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("get from a bucket the endpoint called missing = %v, want a non-ErrNotFound error", err)
		}
	})
}

// TestDeleteSurfacesA404WithoutAnErrorDocument is what keeps the convergence
// from becoming "any 404 means gone". Without demanding the endpoint's own
// error document, a misrouting proxy or a denial concealed as a bare 404 would
// be reported as a successful delete — the caller told its data is gone while
// it is still there.
func TestDeleteSurfacesA404WithoutAnErrorDocument(t *testing.T) {
	for name, body := range bodiesWithoutAnErrorDocument {
		t.Run(name, func(t *testing.T) {
			s, _ := stubStore(t, notFoundWithBody(body))
			if err := s.Delete(context.Background(), "k"); err == nil {
				t.Fatal("a 404 carrying no S3 error document reported success")
			}
		})
	}
}

// TestGetSurfacesA404WithoutAnErrorDocument is the same demand on the reading
// side, and the point of #244. Get used to force its request with a Stat — a
// HEAD, whose 404 carries no body by definition — so every one of these bodies
// read as absence, and a proxy's bare 404 was reported to the caller as "there
// is no such object". Get now learns absence from the GET it was going to
// issue anyway, whose 404 can carry the document, so it can demand the proof
// Delete demands.
func TestGetSurfacesA404WithoutAnErrorDocument(t *testing.T) {
	for name, body := range bodiesWithoutAnErrorDocument {
		t.Run(name, func(t *testing.T) {
			s, _ := stubStore(t, notFoundWithBody(body))
			_, _, err := s.Get(context.Background(), "k")
			if err == nil || errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("get against a 404 carrying no S3 error document = %v, want a non-ErrNotFound error", err)
			}
		})
	}
}

// TestGetReadsAbsenceFromTheEndpointsErrorDocument is the other half: the
// contract's ErrNotFound still has to reach callers, and it now rests on a
// document rather than on a status. What the endpoint sees is what proves the
// mechanism — one GET, which can carry an error document, and no HEAD, which
// cannot.
func TestGetReadsAbsenceFromTheEndpointsErrorDocument(t *testing.T) {
	s, requests := stubStore(t, s3Error(http.StatusNotFound, "NoSuchKey"))
	if _, _, err := s.Get(context.Background(), "gone"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("get of a missing key = %v, want blob.ErrNotFound", err)
	}
	if got := requests(); len(got) != 1 || got[0] != "GET /stub-bucket/gone" {
		t.Errorf("endpoint saw %v, want exactly [GET /stub-bucket/gone]", got)
	}
}
