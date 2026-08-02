package s3

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxErrorBody bounds what endpointWord buffers out of an error response,
// matching the limit minio-go itself puts on decoding one.
const maxErrorBody = 1 << 20

// deleteMarkerDocument is the error document AWS's x-amz-delete-marker header
// stands in for. Its code and message are S3's own for a key that is not
// there, which is exactly what the header reports.
const deleteMarkerDocument = `<?xml version="1.0" encoding="UTF-8"?><Error>` +
	`<Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`

// endpointWord adjusts an error response so that what minio-go decodes from it
// is what the endpoint actually said. absent() reads absence from the
// endpoint's own <Error> document, and there are two places where minio-go's
// unaided reading is not that document:
//
//   - A header can overrule it. minio-go applies x-minio-error-code after a
//     successful decode and not merely as a fallback for one that failed, so a
//     404 whose body says NoSuchBucket and whose header says NoSuchKey would
//     reach absent() as NoSuchKey with the document marker intact — every
//     conjunct satisfied by a response saying, in the endpoint's own document,
//     that the bucket is gone. Where the body decoded, the header is dropped.
//     Where it did not, the header is left alone: it is the only word there is
//     (a HEAD answer has no body to carry one), and it cannot manufacture
//     absence on its own, since a body that did not decode leaves XMLName zero.
//     x-minio-error-desc is deliberately not dropped — it only refines Message,
//     which nothing here classifies on, and MinIO's is often the more specific.
//
//   - AWS sends no document at all when the current version of an object is a
//     delete marker: the GetObject reference's "If the Latest Object Is a
//     Delete Marker" sample response is a 404 with x-amz-delete-marker: true, a
//     text/plain content type and no body. That header is the endpoint's own
//     word that the object is deleted, as authoritative as the document another
//     endpoint would send and the only word a versioned bucket gives after a
//     Delete — so the document it stands in for is written in. The translation
//     cannot launder an ordinary bare 404 into absence, because a proxy's 404
//     does not carry an affirmative AWS delete-marker header.
type endpointWord struct{ next http.RoundTripper }

func (t endpointWord) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	// Only an error response is read for a code, and only these two shapes
	// need adjusting — so an object read's bytes are never buffered here.
	if err != nil || resp.StatusCode < http.StatusBadRequest {
		return resp, err
	}
	if resp.StatusCode == http.StatusNotFound && resp.Header.Get("x-amz-delete-marker") == "true" {
		return withDeleteMarkerDocument(resp), nil
	}
	if resp.Header.Get("x-minio-error-code") != "" {
		return withoutOverridingHeader(resp), nil
	}
	return resp, nil
}

// withDeleteMarkerDocument replaces the answer AWS gives for a delete marker
// with the error document it means.
func withDeleteMarkerDocument(resp *http.Response) *http.Response {
	// AWS sends nothing here, but draining and closing keeps the substitution
	// honest for any endpoint that does send something alongside the header.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(deleteMarkerDocument))
	// The metadata has to follow the body it describes. minio-go reads the
	// body directly and consults neither, but a response handed on with a
	// length and a type belonging to bytes that are no longer there is one
	// nobody else could read correctly either.
	resp.ContentLength = int64(len(deleteMarkerDocument))
	resp.Header.Set("Content-Type", "application/xml")
	resp.Header.Set("Content-Length", strconv.Itoa(len(deleteMarkerDocument)))
	return resp
}

// withoutOverridingHeader drops x-minio-error-code when the body carries the
// endpoint's own error document, and hands the body back unread either way.
func withoutOverridingHeader(resp *http.Response) *http.Response {
	body := resp.Body
	head, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		// Bytes off a transfer that failed are not a document, so nothing here
		// decides anything from them: the failure is re-raised after them and
		// the header left alone. Nothing downstream depends on this — net/http
		// makes a read error sticky (bodyEOFSignal.rerr), and minio-go's
		// executeMethod surfaces it before it classifies anything at all — so
		// this is only the rule stated where it is decided, rather than the
		// last thing standing between a broken transfer and a wrong answer.
		resp.Body = readCloser{io.MultiReader(bytes.NewReader(head), errorReader{err}), body}
		return resp
	}
	resp.Body = readCloser{io.MultiReader(bytes.NewReader(head), body), body}
	// XMLName is what rejects a body that is not an S3 error document at all,
	// such as a proxy's HTML 404 page or a document truncated at the bound
	// above. A document that decoded is the endpoint's word even when it names
	// no code: leaving the header to supply one would be the override itself.
	var doc struct {
		XMLName xml.Name `xml:"Error"`
	}
	if xml.Unmarshal(head, &doc) == nil {
		resp.Header.Del("x-minio-error-code")
	}
	return resp
}

// readCloser reads from one place and closes another — the buffered head
// followed by the untouched rest, closing the original body.
type readCloser struct {
	io.Reader
	io.Closer
}

// errorReader yields no bytes and one error.
type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
