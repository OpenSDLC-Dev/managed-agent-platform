package s3

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The rules endpointWord applies are invisible from outside the package: the
// header it drops only ever changes ErrorResponse.Code, which absent() reads
// through a 404-and-document check that a headerless response fails anyway, so
// a store-level test cannot tell "the header was kept" from "the header was
// dropped". They are pinned here instead, the way alreadyOwned is.

// answer is a RoundTripper that returns one canned response.
type answer struct{ resp *http.Response }

func (a answer) RoundTrip(*http.Request) (*http.Response, error) { return a.resp, nil }

// tracked is a response body that records whether it was closed.
type tracked struct {
	io.Reader
	closed bool
}

func (b *tracked) Close() error { b.closed = true; return nil }

// roundTrip sends one canned response through endpointWord.
func roundTrip(t *testing.T, status int, header map[string]string, body io.ReadCloser) *http.Response {
	t.Helper()
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	resp, err := endpointWord{next: answer{&http.Response{StatusCode: status, Header: h, Body: body}}}.
		RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	return resp
}

const (
	minioHeader      = "x-minio-error-code"
	bucketDocument   = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code></Error>`
	codelessDocument = `<?xml version="1.0" encoding="UTF-8"?><Error><Message>no code here</Message></Error>`
)

// TestHeaderDropsWhenTheBodyIsAnErrorDocument is the rule that closes the
// second half of #244: minio-go applies x-minio-error-code after a successful
// decode, so a document saying NoSuchBucket would otherwise reach absent() as
// NoSuchKey and converge a delete into a vanished bucket.
func TestHeaderDropsWhenTheBodyIsAnErrorDocument(t *testing.T) {
	body := &tracked{Reader: strings.NewReader(bucketDocument)}
	resp := roundTrip(t, http.StatusNotFound, map[string]string{minioHeader: "NoSuchKey"}, body)
	if got := resp.Header.Get(minioHeader); got != "" {
		t.Errorf("header = %q, want it dropped", got)
	}
	// Dropping the header must not cost minio-go the body it decodes.
	read, _ := io.ReadAll(resp.Body)
	if string(read) != bucketDocument {
		t.Errorf("body read back as %q, want the document unchanged", read)
	}
	_ = resp.Body.Close()
	if !body.closed {
		t.Error("closing the handed-back body did not close the original")
	}
}

// TestHeaderDropsForADocumentNamingNoCode keeps the rule at the document
// rather than at the code it happens to carry. A decoded <Error> is the
// endpoint's word even when it names nothing: letting the header supply the
// code there would be the override this exists to stop, and it would satisfy
// every conjunct of absent() — the body decodes, so XMLName is set.
func TestHeaderDropsForADocumentNamingNoCode(t *testing.T) {
	resp := roundTrip(t, http.StatusNotFound, map[string]string{minioHeader: "NoSuchKey"},
		io.NopCloser(strings.NewReader(codelessDocument)))
	if got := resp.Header.Get(minioHeader); got != "" {
		t.Errorf("header = %q, want it dropped", got)
	}
}

// TestHeaderStaysWithoutAnErrorDocument is the other half: where the body is
// not the endpoint's document, the header is the only word there is and keeps
// working — a HEAD answer has no body to carry one. It cannot manufacture
// absence by itself, since a body that did not decode leaves XMLName zero.
func TestHeaderStaysWithoutAnErrorDocument(t *testing.T) {
	// A document past the buffering bound decodes no better than a truncated
	// one, and must not be silently trusted on the strength of its opening.
	oversized := `<Error><Code>NoSuchKey</Code><Message>` +
		strings.Repeat("x", maxErrorBody) + `</Message></Error>`
	for name, body := range map[string]string{
		"Bodyless":          "",
		"ProxyPage":         "<html><head><title>404 Not Found</title></head></html>",
		"TruncatedDocument": "<Error><Code>NoSuchKey</Code>",
		"PastTheBound":      oversized,
	} {
		t.Run(name, func(t *testing.T) {
			resp := roundTrip(t, http.StatusNotFound, map[string]string{minioHeader: "NoSuchKey"},
				io.NopCloser(strings.NewReader(body)))
			if got := resp.Header.Get(minioHeader); got != "NoSuchKey" {
				t.Errorf("header = %q, want it kept", got)
			}
			read, _ := io.ReadAll(resp.Body)
			if string(read) != body {
				t.Errorf("body read back as %d bytes, want the original %d", len(read), len(body))
			}
		})
	}
}

// TestABodyThatFailsPartWayDecidesNothing pins the rule that bytes off a
// failed transfer are not the endpoint's document, however complete they look.
// The stub's failure repeats, which is what a RoundTripper actually meets:
// net/http hands back a bodyEOFSignal whose read error is sticky, so the
// replayed body fails the same way the second time round. That makes the
// header the observable half of the rule — dropping it would be this wrapper
// concluding, from a transfer that broke, that the endpoint had spoken.
func TestABodyThatFailsPartWayDecidesNothing(t *testing.T) {
	broke := errors.New("unexpected EOF")
	resp := roundTrip(t, http.StatusNotFound, map[string]string{minioHeader: "NoSuchKey"},
		&tracked{Reader: io.MultiReader(strings.NewReader(bucketDocument), errorReader{broke})})
	if got := resp.Header.Get(minioHeader); got != "NoSuchKey" {
		t.Errorf("header = %q, want it kept — nothing decoded", got)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, broke) {
		t.Errorf("reading the handed-back body = %v, want the original failure", err)
	}
}

// TestASuccessIsNotBuffered keeps the wrapper off the read path: an object's
// bytes must stream, not be read into memory on their way past.
func TestASuccessIsNotBuffered(t *testing.T) {
	body := &tracked{Reader: strings.NewReader("object bytes")}
	resp := roundTrip(t, http.StatusOK, map[string]string{minioHeader: "NoSuchKey"}, body)
	if resp.Body != body {
		t.Error("a successful response's body was replaced")
	}
	if got := resp.Header.Get(minioHeader); got != "NoSuchKey" {
		t.Errorf("header = %q, want a success left alone", got)
	}
}

// TestADeleteMarkerBecomesTheDocumentItStandsFor covers AWS's answer for a key
// whose current version is a delete marker: a 404 carrying
// x-amz-delete-marker: true and no body at all (the GetObject reference's
// sample response). That is the ordinary state of a deleted key on a versioned
// bucket, so without translating the header into the document it stands for,
// every read of one would fail blob.Store's ErrNotFound contract.
func TestADeleteMarkerBecomesTheDocumentItStandsFor(t *testing.T) {
	body := &tracked{Reader: strings.NewReader("")}
	resp := roundTrip(t, http.StatusNotFound, map[string]string{"x-amz-delete-marker": "true"}, body)
	read, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(read), "<Code>NoSuchKey</Code>") {
		t.Errorf("body = %q, want the error document the header stands for", read)
	}
	if !body.closed {
		t.Error("the substituted-for body was not closed")
	}
}

// TestADeleteMarkerHeaderIsNotHonouredOutsideA404 keeps the translation at the
// one answer AWS documents it for, rather than letting the header name any
// failure absence.
func TestADeleteMarkerHeaderIsNotHonouredOutsideA404(t *testing.T) {
	const denied = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code></Error>`
	resp := roundTrip(t, http.StatusForbidden, map[string]string{"x-amz-delete-marker": "true"},
		io.NopCloser(strings.NewReader(denied)))
	read, _ := io.ReadAll(resp.Body)
	if string(read) != denied {
		t.Errorf("body = %q, want a 403 left saying what it said", read)
	}
}
