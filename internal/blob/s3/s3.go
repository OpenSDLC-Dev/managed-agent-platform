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
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.TLS,
		Region: cfg.Region,
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

// noSuchKey reports the endpoint saying the object is not there. The status is
// required alongside the code because minio-go lets an x-minio-error-code
// response header overwrite the parsed code whatever the response was, and
// absence answers 404 and nothing else.
//
// This is as far as a reader can get: Get learns of absence from a HEAD, whose
// 404 carries no body by definition, so minio-go always synthesizes the code
// from the status there and no stronger proof exists to ask for (#244).
func noSuchKey(err error) bool {
	res := minio.ToErrorResponse(err)
	return res.Code == "NoSuchKey" && res.StatusCode == http.StatusNotFound
}

// absentOnDelete reports a DELETE answered with the endpoint's own NoSuchKey
// error document. A deleter can demand that document where a reader cannot,
// and must: minio-go synthesizes NoSuchKey from any 404 it fails to parse, so
// without it a misrouting proxy or an authorization layer concealing a denial
// would be read as "already gone" and the caller told its data was deleted.
// encoding/xml fills XMLName only on a successful decode, and every branch
// minio-go synthesizes leaves it zero.
//
// Measured against real GCS with this minio-go release: its DELETE of a
// missing object answers 404 with a parsed <Error> document (XMLName "Error",
// no Key), while the same object's HEAD answers 404 with no body at all
// (XMLName empty, Key filled) — which is why the two paths ask for different
// proof rather than sharing one check.
func absentOnDelete(err error) bool {
	return noSuchKey(err) && minio.ToErrorResponse(err).XMLName.Local == "Error"
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
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("s3: get %s: %w", key, err)
	}
	// GetObject is lazy — the request happens on first read. Stat forces it
	// so a missing key is blob.ErrNotFound here, per the Store contract.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if noSuchKey(err) {
			return nil, 0, fmt.Errorf("s3: get %s: %w", key, blob.ErrNotFound)
		}
		return nil, 0, fmt.Errorf("s3: stat %s: %w", key, err)
	}
	return obj, info.Size, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	// AWS S3 and MinIO make DeleteObject idempotent themselves — a missing key
	// answers 204 — but GCS's XML API answers 404 NoSuchKey, so the same
	// absence has to be mapped here for the contract's crashed-and-retried
	// delete to converge on every S3-compatible endpoint rather than flap.
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !absentOnDelete(err) {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}
