// Package gcs is the Google Cloud Storage backend, on the native
// cloud.google.com/go/storage client rather than GCS's S3-interop XML API. The
// difference is the credential: interop needs an HMAC key pair — downloaded key
// material a deployment has to store and rotate — where this authenticates with
// Application Default Credentials, which on GKE is a Workload Identity binding
// and no material at all (#240).
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// Config names the bucket. Authentication is Application Default Credentials —
// Workload Identity on GKE — so no credential appears here, and neither does an
// endpoint: the client resolves Google's own (CLAUDE.md principle 4 is about
// not hard-coding a *provider's* endpoint into a protocol adapter, and this
// adapter speaks to exactly one provider).
type Config struct {
	// Bucket is the bucket's name, which must already exist: there is no
	// create-if-missing here, and nothing asks the bucket about itself. The
	// permission-minimal shape #241 made optional for the S3 backend is the only
	// shape this one has, so the deployment identity holds object permissions and
	// nothing bucket-level — deploy/gcp grants roles/storage.objectUser to the
	// processes that write and roles/storage.objectViewer to the one that only
	// reads (#240 slice 2). The cost is that a
	// misspelled bucket or a missing IAM binding surfaces on first use rather
	// than at startup — where it is still named, not swallowed, because absence
	// is checked rather than assumed (see the absence method).
	Bucket string

	// Client overrides the ADC-authenticated client New would build. Tests point
	// it at a fake-GCS container; the caller owns closing it.
	Client *storage.Client
}

// Store implements blob.Store against one GCS bucket.
type Store struct {
	bucket *storage.BucketHandle
}

// New builds the client and returns a store. It makes no request: there is
// nothing to check that would not cost the bucket-level privilege this backend
// exists to do without.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("gcs: bucket is required")
	}
	client := cfg.Client
	if client == nil {
		var err error
		// No credential option: Application Default Credentials, which on GKE is
		// the Workload Identity binding. A deployment without one fails here.
		client, err = storage.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("gcs: build client (Application Default Credentials): %w", err)
		}
	}
	return &Store{bucket: client.Bucket(cfg.Bucket)}, nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	// The writer commits on Close and on nothing else, so returning without
	// closing is already enough to leave no object. The child context is about
	// the other half: an abandoned writer's upload goroutine would otherwise sit
	// on the caller's context, so cancelling releases it (Writer.CloseWithError
	// is deprecated in favour of exactly this). Cancelling after a *successful*
	// Close cannot undo anything — Close waits for the commit to complete.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := s.bucket.Object(key).NewWriter(ctx)
	w.ContentType = contentType
	// CopyN, not Copy: the contract is "exactly size bytes" — a reader with
	// fewer is an error, and bytes beyond size are never read.
	//
	// `err != io.EOF`, deliberately not errors.Is: io.CopyN produces the BARE
	// sentinel and only on the short-source path, while errors.Is would also
	// swallow any error that merely wraps it — and the writer supplies exactly
	// those. An object past the 16 MiB chunk size uploads while CopyN is still
	// writing, so a failed chunk PUT comes back through Write as
	// *url.Error{Err: io.EOF} (the client retries that shape by name, so it is
	// routine, not exotic). Under errors.Is a network fault would be discarded
	// and reported as "reader gave N bytes, want M" — blaming the caller's
	// stream for a transport failure, with the real cause gone.
	n, err := io.CopyN(w, r, size)
	if err != nil && err != io.EOF { //nolint:errorlint // see above: the bare sentinel is the point
		return fmt.Errorf("gcs: put %s: %w", key, err)
	}
	if n != size {
		return fmt.Errorf("gcs: put %s: reader gave %d bytes, want %d", key, n, size)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs: put %s: %w", key, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	r, err := s.bucket.Object(key).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, 0, fmt.Errorf("gcs: get %s: %w", key, s.absence(ctx))
		}
		return nil, 0, fmt.Errorf("gcs: get %s: %w", key, err)
	}
	return r, r.Attrs.Size, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	// A crashed-and-retried delete must converge, so the absence the second one
	// meets is success. The client reports it as its own sentinel, where the S3
	// backend had to read it out of an XML error document.
	err := s.bucket.Object(key).Delete(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs: delete %s: %w", key, err)
	}
	if absent := s.absence(ctx); !errors.Is(absent, blob.ErrNotFound) {
		return fmt.Errorf("gcs: delete %s: %w", key, absent)
	}
	return nil
}

// absence decides what the client's one 404 sentinel meant. ErrObjectNotExist
// covers two states this store must not confuse — the object is not there, and
// the bucket is not there — because on the read path GCS gives the client
// nothing else to go on: measured against real Cloud Storage, a GET for a
// missing object and a GET in a bucket that does not exist both surface as a
// bare ErrObjectNotExist with no googleapi error to inspect. The S3 backend
// keeps a vanished bucket an error rather than an empty store, and so must this
// one, or a mistyped bucket name reads as "the platform has no skills" instead
// of as the incident it is.
//
// One limit this does not remove, stated rather than implied: the probe decides
// what the 404 MEANT, not whether it really came from Cloud Storage. The client
// turns any HTTP 404 into ErrObjectNotExist without inspecting the body
// (storage's http_client.go does it on the read path outright), so a 404
// manufactured somewhere in the path would still read as absence once the probe
// confirms the bucket. The S3 backend does guard that case — it requires the
// endpoint's own <Error> document — and the difference is the endpoint, not the
// care taken: there it is operator-configured, so a misrouting proxy is a
// deployment shape someone can actually have. This Config has no endpoint field
// and no credential, so a deployment has nothing to point elsewhere, and the
// guard would cost a hand-rolled response path against a hop it cannot introduce.
// (Not an absolute: the client honours STORAGE_EMULATOR_HOST, so an environment
// variable can still redirect it — which is a test seam this package's own suite
// uses, and which the live tier refuses outright for exactly that reason.)
//
// The question is settled with a one-object listing rather than a bucket read.
// That is the whole point: listing is storage.objects.list, which every role a
// deployment would plausibly grant here already carries — objectViewer as much as
// objectUser or objectAdmin — so the answer costs no bucket-level privilege, and
// the read-only identity can answer it too. The client turns the listing's own 404 into ErrBucketNotExist,
// which the read path never produces. Only an affirmative answer buys absence: a
// probe that fails for any other reason (a denial, a transport error) is a
// question left open, and an open question is an error rather than a report that
// data is gone. One extra request, on the miss path only.
func (s *Store) absence(ctx context.Context) error {
	it := s.bucket.Objects(ctx, &storage.Query{Projection: storage.ProjectionNoACL})
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return err
	}
	return blob.ErrNotFound
}
