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

// FilesKey is the object-storage key a new Files-API file's bytes are written
// at — the `files/{file_id}` namespace this package's doc reserves. It lives
// here, not in a feature package (unlike skills' own BlobKey), because it has
// no home package: the api registry, the executor's outputs harvest and the
// dream runner all write files objects.
//
// It names where a writer puts bytes, never where a reader finds them. Since
// #578 a files row records its key in object_key, and a session's copy of an
// upload records the upload's, so every reader reads the column; migration 0046
// backfills it, and fills it for a previous build's INSERT, with this same
// layout.
func FilesKey(id string) string { return "files/" + id }

// SessionCheckpointKey is the object-storage key for a session's workspace
// checkpoint — the `workspace/{session_id}/checkpoint.tar.gz` layout plan 24
// fixes. It lives here for FilesKey's reason: the executor writes it (the
// idle-TTL capture), the executor's reaper and the API's session delete both
// remove it, and one definition keeps the writer and its removers from
// drifting.
func SessionCheckpointKey(sessionID string) string {
	return "workspace/" + sessionID + "/checkpoint.tar.gz"
}

// Store is the object-storage contract. Keys are opaque non-empty strings;
// "/" separators are conventional namespacing, not directories.
type Store interface {
	// Put stores exactly size bytes from r at key, overwriting any existing
	// object: a reader with fewer bytes than size is an error, and bytes
	// beyond size are never read. contentType is stored as object metadata
	// for HTTP consumers.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get returns the object's bytes and size. A missing key is ErrNotFound
	// from Get itself, never deferred to the first Read. The caller closes
	// the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)

	// Delete removes the object at key. Deleting a missing key is not an
	// error: a crashed-and-retried delete must converge, not flap.
	Delete(ctx context.Context, key string) error
}
