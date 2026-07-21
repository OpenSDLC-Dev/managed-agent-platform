package s3

import (
	"context"
	"log/slog"
	"os"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// FromEnv builds the metrics-wrapped store from the BLOB_* environment — the
// one construction the controlplane and executor binaries share, so their
// notion of "configured" cannot drift. (nil, nil) when BLOB_ENDPOINT is
// unset: object storage is optional, and each binary decides what its
// absence means (skills routes report it; the executor skips
// materialization).
func FromEnv(ctx context.Context) (blob.Store, error) {
	endpoint := os.Getenv("BLOB_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	store, err := New(ctx, Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("BLOB_ACCESS_KEY"),
		SecretKey: os.Getenv("BLOB_SECRET_KEY"),
		Bucket:    os.Getenv("BLOB_BUCKET"),
		Region:    os.Getenv("BLOB_REGION"),
		TLS:       os.Getenv("BLOB_TLS") == "true",
	})
	if err != nil {
		return nil, err
	}
	slog.Info("object storage configured", "endpoint", endpoint, "bucket", os.Getenv("BLOB_BUCKET"))
	return blob.WithMetrics(store), nil
}
