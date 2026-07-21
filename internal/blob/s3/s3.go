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
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, 0, fmt.Errorf("s3: get %s: %w", key, blob.ErrNotFound)
		}
		return nil, 0, fmt.Errorf("s3: stat %s: %w", key, err)
	}
	return obj, info.Size, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	// S3 DeleteObject is idempotent — a missing key succeeds — which is
	// exactly the contract's convergence requirement.
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}
