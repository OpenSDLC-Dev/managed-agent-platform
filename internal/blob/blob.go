// Package blob is the platform's object-storage seam: opaque bytes at string
// keys, behind the one interface every backend must satisfy (CLAUDE.md:
// backend variability lives behind an interface with one shared contract
// suite — internal/blob/blobtest). The first consumer is the skills registry
// (docs/plan/06_skills.md), which stores canonical skill-version archives at
// `skills/{skill_id}/{version}.zip`; the key namespace deliberately leaves
// room for later surfaces (the deferred Files API) to share the store.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound reports a Get of a key that has no object. Implementations wrap
// it so callers can errors.Is across backends.
var ErrNotFound = errors.New("blob: object not found")

// Store is the object-storage contract. Keys are opaque non-empty strings;
// "/" separators are conventional namespacing, not directories.
type Store interface {
	// Put stores exactly size bytes from r at key, overwriting any existing
	// object. contentType is stored as object metadata for HTTP consumers.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get returns the object's bytes and size. A missing key is ErrNotFound
	// from Get itself, never deferred to the first Read. The caller closes
	// the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)

	// Delete removes the object at key. Deleting a missing key is not an
	// error: a crashed-and-retried delete must converge, not flap.
	Delete(ctx context.Context, key string) error
}
