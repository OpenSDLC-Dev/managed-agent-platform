// Package backend selects an object-storage backend by name, so every binary
// that reaches object storage constructs it from the same config point instead
// of each mapping the environment its own way.
//
// It is a sibling of internal/blob rather than part of it — the shape
// internal/secrets/backend and internal/sandbox/backend already have, and for
// the same structural reason: the seam package holds the interface AND the
// sentinel error backends wrap (blob.ErrNotFound), so it must not import them.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/gcs"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/s3"
)

// FromEnv builds the metrics-wrapped store from the BLOB_* environment — the one
// construction the controlplane, brain and executor binaries share, so their
// notion of "configured" cannot drift. (nil, nil) when object storage is not
// configured: it is optional, and each binary decides what its absence means
// (skills routes report it; the executor skips materialization; the brain grades
// file rubrics from the description alone).
//
// BLOB_BACKEND unset means s3, so every deployment that predates the selector
// keeps working unchanged — internal/sandbox/backend's empty-means-docker arm,
// for the same reason. Which variable then says "configured at all" differs by
// backend and is documented on each arm. Once a backend is selected, missing
// configuration fails rather than degrades (the modeltest rule: opting in makes
// misconfiguration an error, never a silent fallback).
func FromEnv(ctx context.Context) (blob.Store, error) {
	backend := os.Getenv("BLOB_BACKEND")
	switch backend {
	case "", "s3":
		// An unset BLOB_ENDPOINT means two different things, and which one
		// depends on whether a backend was named. Unset backend: this is a
		// deployment that predates the selector saying it has no object storage,
		// exactly as BLOB_ENDPOINT alone always said. Named backend: the operator
		// asked for S3 and left out the one thing S3 cannot be reached without,
		// which is a misconfiguration and must not read as "storage off" — the
		// same rule the gcs arm applies to a missing bucket.
		endpoint := os.Getenv("BLOB_ENDPOINT")
		if endpoint == "" {
			if backend == "" {
				return nil, nil
			}
			return nil, fmt.Errorf("BLOB_BACKEND=s3 needs BLOB_ENDPOINT")
		}
		store, err := s3.New(ctx, s3.Config{
			Endpoint:  endpoint,
			AccessKey: os.Getenv("BLOB_ACCESS_KEY"),
			SecretKey: os.Getenv("BLOB_SECRET_KEY"),
			Bucket:    os.Getenv("BLOB_BUCKET"),
			Region:    os.Getenv("BLOB_REGION"),
			TLS:       os.Getenv("BLOB_TLS") == "true",
			// Read like BLOB_TLS, and safe to: a value this does not recognize
			// leaves the bucket check in place, so the mistake ends in a startup
			// error from the check rather than in a store nobody verified.
			BucketPrecreated: os.Getenv("BLOB_BUCKET_PRECREATED") == "true",
		})
		if err != nil {
			return nil, err
		}
		slog.Info("object storage configured", "backend", "s3",
			"endpoint", endpoint, "bucket", os.Getenv("BLOB_BUCKET"))
		return blob.WithMetrics(store), nil
	case "gcs":
		// No credential variable and no endpoint: authentication is Application
		// Default Credentials, which on GKE is a Workload Identity binding, and
		// the client resolves Google's own endpoint. Selecting this backend is
		// itself the statement that object storage is configured, so a missing
		// bucket is an error rather than the "no object storage" answer the s3
		// arm gives.
		bucket := os.Getenv("BLOB_BUCKET")
		if bucket == "" {
			return nil, fmt.Errorf("BLOB_BACKEND=gcs needs BLOB_BUCKET")
		}
		// The S3-only variables are not read on this arm, and an operator who
		// left them set is describing an endpoint the client will never use.
		// Said out loud rather than rejected: a values file that carries both
		// while switching between them is a reasonable thing to have, and
		// failing it would be this selector deciding a deployment's business.
		// Silence is the one option that is wrong.
		if ignored := setOf("BLOB_ENDPOINT", "BLOB_ACCESS_KEY", "BLOB_SECRET_KEY", "BLOB_REGION"); len(ignored) > 0 {
			slog.Warn("BLOB_BACKEND=gcs ignores the S3-only object storage settings; "+
				"authentication is Application Default Credentials and the endpoint is Google's",
				"ignored", ignored)
		}
		store, err := gcs.New(ctx, gcs.Config{Bucket: bucket})
		if err != nil {
			return nil, err
		}
		slog.Info("object storage configured", "backend", "gcs", "bucket", bucket)
		return blob.WithMetrics(store), nil
	default:
		return nil, fmt.Errorf("unknown BLOB_BACKEND %q (want s3 or gcs)", backend)
	}
}

// setOf returns which of the named variables carry a value — the names only,
// never the values, since two of these are credentials.
func setOf(names ...string) []string {
	var set []string
	for _, name := range names {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}
	return set
}
