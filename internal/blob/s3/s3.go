// Package s3 is the S3-compatible blob.Store backend, on minio-go so one
// implementation speaks to MinIO, AWS S3, Ceph RGW, or anything else with the
// S3 wire protocol — never a MinIO-specific API (an operator must be able to
// swap the vendor without touching this package).
package s3

import (
	"context"
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
	Region    string // some S3 endpoints require it on bucket create; required by BucketPrecreated
	TLS       bool

	// BucketPrecreated asserts that the bucket already exists, so New neither
	// checks for it nor creates it. It is what lets a deployment identity hold
	// object permissions only: the bucket check is a bucket-level read
	// (storage.buckets.get on GCS, granted by roles/storage.legacyBucketReader)
	// that a pre-provisioned deployment can only ever answer "yes", so
	// demanding it widens the identity for nothing (#241, and
	// docs/plan/20_gcp-deployment.md Decision 11).
	//
	// It requires Region, because the construction check is not the only
	// bucket-level call: a client with no region resolves the bucket's location
	// before the first object request and caches it (bucket-cache.go), so the
	// mode would trade a bucket call at startup for one at first use. New
	// rejects the pair rather than half-keeping the promise. The requirement is
	// deliberately blunt — minio-go does derive a region from an AWS regional
	// endpoint's own hostname, so a few endpoints would have been safe without
	// one — because "set the region" is a rule an operator can follow and
	// "set it unless your endpoint spells it out" is not. The region must also be the *right* one: minio-go recovers from
	// a wrong region only while its own is empty (api.go's
	// AuthorizationHeaderMalformed retry), so a mistyped one now fails the
	// first object request instead of self-correcting.
	//
	// One endpoint is outside the promise. Against an Amazon endpoint with an
	// S3 Express directory bucket, minio-go mints session credentials for every
	// object request — a bucket-root GET ?session (api.go, create-session.go) —
	// which needs s3express:CreateSession on the bucket. That call is the
	// client's, not this package's, and it is made with or without this
	// setting; the mode simply cannot remove it.
	//
	// The check is also the one call New makes, so this is a trade: with it
	// skipped, an unreachable endpoint, a wrong credential, a wrong region or a
	// misspelled bucket surfaces on first use rather than at startup. That is
	// the right way round for a deployment whose bucket is provisioned out of
	// band, and the wrong one for the bundled MinIO, which starts empty — hence
	// a per-deployment setting rather than a change of default.
	BucketPrecreated bool
}

// Store implements blob.Store against one S3 bucket.
type Store struct {
	client *minio.Client
	bucket string
}

// New connects and ensures the bucket exists, so every later operation can
// assume it (the bundled MinIO starts empty; creating the bucket here keeps
// deployment free of a separate bootstrap step). Idempotent across processes:
// two racing creators both succeed. Config.BucketPrecreated takes the operator's
// word for that instead, and New then issues no request at all — see the field
// for what that does and does not promise about the object work that follows.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint and bucket are required")
	}
	if cfg.BucketPrecreated && cfg.Region == "" {
		return nil, errors.New("s3: a pre-created bucket needs an explicit region, " +
			"or the first object request resolves the bucket's location — the " +
			"bucket-level call the mode exists to stop making")
	}
	// minio-go's own default transport, wrapped so an error response reaches
	// the library saying what the endpoint meant (see endpointWord). It has to
	// be built here rather than left nil, because supplying a transport is the
	// only way in — and it has to be minio-go's rather than http's, whose
	// transparent gzip would rewrite an object's bytes and lose its length.
	transport, err := minio.DefaultTransport(cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("s3: transport: %w", err)
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:    cfg.TLS,
		Region:    cfg.Region,
		Transport: endpointWord{next: transport},
	})
	if err != nil {
		return nil, fmt.Errorf("s3: client: %w", err)
	}
	if !cfg.BucketPrecreated {
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
// detached from the document it came with.
//
// What reaches this check is the endpoint's word rather than minio-go's
// reading of it because endpointWord repairs the two places those differ: a
// response header that overrules a parsed document, and the one absence AWS
// reports by header instead of by document at all.
func absent(err error) bool {
	res := minio.ToErrorResponse(err)
	return res.Code == "NoSuchKey" &&
		res.StatusCode == http.StatusNotFound &&
		res.XMLName.Local == "Error"
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
