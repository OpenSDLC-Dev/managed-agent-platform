// Package s3 is the S3-compatible blob.Store backend, on minio-go so one
// implementation speaks to MinIO, AWS S3, Ceph RGW, or anything else with the
// S3 wire protocol — never a MinIO-specific API (an operator must be able to
// swap the vendor without touching this package).
package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// Config is everything needed to reach one bucket, supplied by deployment
// configuration (config-driven per CLAUDE.md principle 4 — no default
// endpoint exists).
type Config struct {
	Endpoint  string // host:port, no scheme
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string // optional; some S3 endpoints require it on bucket create
	TLS       bool
}

// Store implements blob.Store against one S3 bucket.
type Store struct {
	client *minio.Client
	bucket string
}

// New connects and ensures the bucket exists, so every later operation can
// assume it (the bundled MinIO starts empty; creating the bucket here keeps
// deployment free of a separate bootstrap step). Idempotent across processes:
// two racing creators both succeed.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint and bucket are required")
	}
	// minio-go's own default transport, wrapped so that an endpoint's error
	// document outranks the x-minio-error-code header (see bodyCodeWins). It
	// has to be built here rather than left nil, because supplying a transport
	// is the only way in.
	transport, err := minio.DefaultTransport(cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("s3: transport: %w", err)
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:    cfg.TLS,
		Region:    cfg.Region,
		Transport: bodyCodeWins{next: transport},
	})
	if err != nil {
		return nil, fmt.Errorf("s3: client: %w", err)
	}
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: check bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region})
		// Losing the create race means another process made it: the goal state.
		if err != nil && !alreadyOwned(err) {
			return nil, fmt.Errorf("s3: create bucket %q: %w", cfg.Bucket, err)
		}
	}
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

func alreadyOwned(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists"
}

// absent reports the endpoint's own word that there is no object at the key:
// its <Error> document, the NoSuchKey code, and the 404 that absence answers.
// Both operations that map absence — a Get that owes callers ErrNotFound and a
// Delete that owes them convergence — ask for exactly this.
//
// All three conjuncts are load-bearing, because ErrorResponse.Code on its own
// is not the endpoint's word. minio-go synthesizes NoSuchKey from any 404
// whose body does not decode as an S3 error document, so without the document
// a misrouting proxy or an authorization layer concealing a denial reads as
// absence: a deleter told its data is gone while it is still there, a reader
// told an object it was refused never existed. XMLName is the marker for a
// synthesized code — minio-go keeps the decoded struct only when the body
// parsed and replaces it wholesale in every decode-failure branch, so a
// synthesized error always carries a zero XMLName. (encoding/xml alone would
// not be enough: it validates the root element and assigns XMLName before
// decoding any children, so a document that starts <Error> and then breaks
// leaves XMLName set. minio-go discarding that struct is what makes the marker
// trustworthy.) The status is required because a code can still arrive
// detached from the document it came with — see bodyCodeWins for the response
// header that does it, and for why the document outranks it.
func absent(err error) bool {
	res := minio.ToErrorResponse(err)
	return res.Code == "NoSuchKey" &&
		res.StatusCode == http.StatusNotFound &&
		res.XMLName.Local == "Error"
}

// maxErrorBody bounds what bodyCodeWins buffers to read a code, matching the
// limit minio-go itself puts on decoding an error body.
const maxErrorBody = 1 << 20

// bodyCodeWins keeps an endpoint's own error document authoritative over the
// x-minio-error-code response header. minio-go applies that header after a
// successful decode and not merely as a fallback for one that failed, so a 404
// whose body says NoSuchBucket and whose header says NoSuchKey reaches absent
// as NoSuchKey with the document marker intact — every conjunct satisfied by a
// response that says, in the endpoint's own document, that the bucket is gone.
//
// Dropping the header only where the body already carries a code leaves the
// dialect working wherever it is the only source, such as a HEAD answer that
// has no body to carry one.
type bodyCodeWins struct{ next http.RoundTripper }

func (t bodyCodeWins) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	// Only an error response can carry an error document, and only a response
	// carrying the header has anything to overrule — so a successful read's
	// object bytes are never buffered here.
	if err != nil || resp.StatusCode < http.StatusBadRequest ||
		resp.Header.Get("x-minio-error-code") == "" {
		return resp, err
	}
	body := resp.Body
	// A read error leaves head short, which then fails to decode below and
	// keeps the header — the same outcome as never having intervened. Handing
	// back what was read, followed by the original body, leaves minio-go to
	// meet the same bytes and the same error.
	head, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), body), body}
	// XMLName is what makes this reject a body that is not an S3 error
	// document at all, such as a proxy's HTML 404 page.
	var doc struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
	}
	if xml.Unmarshal(head, &doc) == nil && doc.Code != "" {
		resp.Header.Del("x-minio-error-code")
	}
	return resp, nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size,
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	// minio.Core is minio-go's low-level API — a plain S3 GET on the wire,
	// nothing MinIO-specific — and it issues the request eagerly, where
	// Client.GetObject is lazy and has to be forced with a Stat. That Stat is
	// a HEAD, and a HEAD answer carries no body, so its 404 could never be the
	// endpoint's own error document and every bare 404 read as absence (#244).
	// The GET here is the one the caller's first read would have issued
	// anyway, so asking for the stronger proof removes a round trip rather
	// than adding one.
	rc, info, _, err := minio.Core{Client: s.client}.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if absent(err) {
			return nil, 0, fmt.Errorf("s3: get %s: %w", key, blob.ErrNotFound)
		}
		return nil, 0, fmt.Errorf("s3: get %s: %w", key, err)
	}
	return rc, info.Size, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	// AWS S3 and MinIO make DeleteObject idempotent themselves — a missing key
	// answers 204 — but GCS's XML API answers 404 NoSuchKey, so the same
	// absence has to be mapped here for the contract's crashed-and-retried
	// delete to converge rather than flap. It converges on an endpoint that
	// answers 204 or says NoSuchKey in its own error document; one that answers
	// a bare 404 instead fails closed and never converges, which is the
	// deliberate trade — a delete that keeps erroring is an operator's problem,
	// a delete that reports success on a misrouted request is a lost object.
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !absent(err) {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}
