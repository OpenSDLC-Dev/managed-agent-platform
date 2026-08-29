package gcs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/gcs"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/gcs/gcstest"
)

func TestMain(m *testing.M) {
	os.Exit(gcstest.Main(m))
}

func newStore(t *testing.T) blob.Store {
	t.Helper()
	client, bucket := gcstest.FreshBucket(t)
	s, err := gcs.New(context.Background(), gcs.Config{Bucket: bucket, Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	return s
}

// TestContract runs the shared blob.Store suite — the definition of done for
// any backend (CLAUDE.md testing conventions).
func TestContract(t *testing.T) {
	blobtest.Run(t, newStore)
}

// TestContractWithMetrics runs the same suite through the metrics decorator:
// the wrapper must be behaviorally invisible.
func TestContractWithMetrics(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		return blob.WithMetrics(newStore(t))
	})
}

func TestNewRequiresBucket(t *testing.T) {
	if _, err := gcs.New(context.Background(), gcs.Config{}); err == nil {
		t.Error("New accepted a config with no bucket")
	}
}

// TestNewIssuesNoRequest is this backend's whole permission story: it holds
// object permissions and nothing bucket-level, which it can only do by never
// asking about the bucket. The endpoint here refuses everything, so the only way
// New can succeed is by asking nothing.
func TestNewIssuesNoRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	if _, err := gcs.New(context.Background(), gcs.Config{Bucket: "b", Client: client}); err != nil {
		t.Fatalf("New against an endpoint that refuses everything: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("endpoint saw %v, want no request at all", seen)
	}
}

func TestPutShortReaderErrors(t *testing.T) {
	// The contract promises "exactly size bytes": a reader that runs dry before
	// size must be an error, never a silently truncated object.
	s := newStore(t)
	ctx := context.Background()
	err := s.Put(ctx, "short", strings.NewReader("abc"), 10, "")
	if err == nil {
		t.Fatal("put with a short reader succeeded")
	}
	// Not just "some error": the message has to name the shortfall, or a
	// transport failure reported here would be indistinguishable from it — the
	// exact confusion the bare-sentinel comparison in Put exists to prevent.
	if !strings.Contains(err.Error(), "reader gave 3 bytes, want 10") {
		t.Errorf("short-reader error = %v, want it to name the byte counts", err)
	}
	// ...and it must not have committed anything either.
	if _, _, err := s.Get(ctx, "short"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get after a short put = %v, want blob.ErrNotFound", err)
	}
}

// TestPutTransportFailureIsNotReportedAsAShortReader is the regression guard for
// the one comparison in Put that cannot be got right by inspection. io.CopyN
// yields the BARE io.EOF, and only when the source ran dry — but the writer
// yields errors that WRAP it: past the 16 MiB chunk size the upload runs while
// CopyN is still writing, so a chunk PUT that dies mid-flight surfaces through
// Write as *url.Error{Err: io.EOF}. Under errors.Is that network failure was
// swallowed and reported as "reader gave N bytes, want M" — the caller's stream
// blamed for the network's fault, with the real error gone. Nothing else in this
// package stages a chunked upload, so without this test the comparison could be
// reverted and every other test would stay green.
//
// The endpoint accepts the resumable session, then drains the chunk and drops the
// connection — which is what produces the wrapped EOF.
func TestPutTransportFailureIsNotReportedAsAShortReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Routed on the path, not the method: the chunk upload is a POST too, so
		// keying on the method would answer it with a fresh session instead of
		// dropping it, and the test would pass for the wrong reason.
		if !strings.Contains(r.URL.Path, "/upload/session") {
			// Hand back an upload session pointing at ourselves.
			w.Header().Set("Location", "http://"+r.Host+"/upload/session")
			w.WriteHeader(http.StatusOK)
			return
		}
		// The chunk: read what the client is sending, then kill the connection
		// under it rather than answering. That is what reaches Put as
		// *url.Error{Err: io.EOF} — the shape the comparison has to survive.
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithEndpoint(srv.URL+"/storage/v1/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	s, err := gcs.New(ctx, gcs.Config{Bucket: "b", Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	// Past the 16 MiB default chunk size, so the upload starts before the copy
	// finishes and the failure reaches Put through Write.
	const size = 17 << 20
	err = s.Put(ctx, "big", strings.NewReader(strings.Repeat("x", size)), size, "")
	if err == nil {
		t.Fatal("put against an endpoint that drops the connection succeeded")
	}
	if strings.Contains(err.Error(), "reader gave") {
		t.Errorf("transport failure reported as a short reader: %v", err)
	}
}

func TestPutIgnoresBytesBeyondSize(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "trimmed", strings.NewReader("abcdefghij"), 3, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, size, err := s.Get(ctx, "trimmed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	if size != 3 {
		t.Errorf("stored size = %d, want 3", size)
	}
	// The content, not just its length: storing any three bytes would satisfy a
	// size check, and "ignores bytes beyond size" is a claim about WHICH three.
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("stored %q, want the first three bytes %q", got, "abc")
	}
}

func TestEmptyKeyOpsSurfaceErrors(t *testing.T) {
	// The contract types keys as non-empty; the backend must turn a violation
	// into an error, and never into blob.ErrNotFound (absence of an object that
	// could not exist is a caller bug, not a miss). Get and Delete are refused
	// locally by the client's own name validation ("storage: object name is
	// empty", before any request); Put is not — the writer path skips that
	// validation, so the empty name goes to the endpoint and comes back a 400.
	// Either way it is an error and not the absence sentinel, which is what this
	// asserts; the difference is one wasted round trip, not a written object.
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put with empty key succeeded")
	}
	if _, _, err := s.Get(ctx, ""); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get with empty key = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, ""); err == nil {
		t.Error("delete with empty key succeeded")
	}
}

// TestMissingBucketIsAnErrorNotAbsence is the point of the absence probe. GCS
// gives the read path one sentinel for two states, so without the probe a
// mistyped bucket name would read as an empty store and the platform would
// report every skill archive and every file as simply not there. The S3 backend
// keeps a vanished bucket an error (TestOpsAgainstRemovedBucketAreErrorsNotAbsence);
// this holds the same line without a bucket-level permission.
func TestMissingBucketIsAnErrorNotAbsence(t *testing.T) {
	client, _ := gcstest.FreshBucket(t)
	ctx := context.Background()
	s, err := gcs.New(ctx, gcs.Config{Bucket: "gcstest-never-created", Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if _, _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get from a missing bucket = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete in a missing bucket reported success")
	}
	if err := s.Put(ctx, "k", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put into a missing bucket succeeded")
	}
}

// TestDeniedProbeIsAnErrorNotAbsence is the other half of the absence rule, and
// the half the fake cannot stage: only an AFFIRMATIVE listing may buy absence.
// TestMissingBucketIsAnErrorNotAbsence exercises the probe with the one error the
// rule's most likely wrong implementation propagates anyway — the 404 the client
// types as ErrBucketNotExist — so it alone does not pin "an open question is an
// error". Make absence special-case that sentinel and swallow everything else
// (the shape someone writes when they set out to catch a mistyped bucket and stop
// there) and MissingBucket stays green while this one goes red. This stages the
// case that separates them: the read answers 404 (absence, as far as the client
// can tell) while the LIST is refused 403, which is what a deployment holding
// object permissions but no list permission looks like. Reporting ErrNotFound
// there would tell the caller the data is gone on the strength of a question
// nobody answered.
//
// (Deleting the probe outright is caught by both tests, which is why the narrower
// mutant above is the one that matters.)
func TestDeniedProbeIsAnErrorNotAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The listing is a GET on the bucket's object collection; the read is a
		// GET on one object below it. Distinguish on the query, which only the
		// list carries.
		if strings.Contains(r.URL.Path, "/o/") && !strings.HasSuffix(r.URL.Path, "/o/") {
			http.Error(w, `{"error":{"code":404,"message":"No such object"}}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":{"code":403,"message":"does not have storage.objects.list access"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithEndpoint(srv.URL+"/storage/v1/"),
		option.WithoutAuthentication(), storage.WithJSONReads())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	s, err := gcs.New(ctx, gcs.Config{Bucket: "denied", Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if _, _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get with the probe denied = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete with the probe denied reported success")
	}
}

// TestPutPersistsContentType pins what the Store interface never reads back —
// content-type exists for HTTP consumers — with the endpoint's own metadata, or
// the impl could drop it and every other test would stay green.
func TestPutPersistsContentType(t *testing.T) {
	client, bucket := gcstest.FreshBucket(t)
	ctx := context.Background()
	s, err := gcs.New(ctx, gcs.Config{Bucket: bucket, Client: client})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if err := s.Put(ctx, "typed", strings.NewReader("zip!"), 4, "application/zip"); err != nil {
		t.Fatalf("put: %v", err)
	}
	attrs, err := client.Bucket(bucket).Object("typed").Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.ContentType != "application/zip" {
		t.Errorf("stored content-type = %q, want %q", attrs.ContentType, "application/zip")
	}
}

// TestFailedOverwriteLeavesThePreviousObject is the atomicity the writer's
// commit-on-Close gives us: a put that errors mid-stream must not have replaced
// what was there.
func TestFailedOverwriteLeavesThePreviousObject(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "k", strings.NewReader("the original"), 12, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader("short"), 99, ""); err == nil {
		t.Fatal("put with a short reader succeeded")
	}
	rc, size, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get after the failed overwrite: %v", err)
	}
	defer rc.Close()
	if size != 12 {
		t.Errorf("size = %d, want the original 12", size)
	}
	// Same reason as above: the original CONTENT has to survive, not merely an
	// object of the original length.
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the original" {
		t.Errorf("stored %q, want the untouched %q", got, "the original")
	}
}

// randomTag is the collision-proof half of a live-tier key prefix.
func randomTag(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// maxBucketName is GCS's limit for an ordinary (non-dotted) bucket name.
const maxBucketName = 63

// missingBucketName derives a name that is unique, certainly absent, and short
// enough to be legal — truncating the configured base rather than the tag, since
// the tag is what makes the name unlikely to belong to anyone. A trailing '-'
// left by the truncation is trimmed: a bucket name may not end in one, and an
// illegal name would fail validation instead of reaching the absence path.
func missingBucketName(base, tag string) string {
	const suffix = "-missing-"
	if room := maxBucketName - len(suffix) - len(tag); len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return base + suffix + tag
}

// TestMissingBucketNameStaysLegal runs in the default tier, because the rule it
// checks is the live tier's and the live tier is opt-in: without this, a base
// name long enough to overflow the limit would be found only by whoever happened
// to configure one, and would surface as a test that passes for the wrong reason
// rather than as a failure.
func TestMissingBucketNameStaysLegal(t *testing.T) {
	const tag = "0123456789abcdef"
	for name, base := range map[string]string{
		"Short":             "map-blob",
		"AtTheLimit":        strings.Repeat("b", maxBucketName),
		"OverTheLimit":      strings.Repeat("b", maxBucketName*2),
		"TruncatesToADash":  strings.Repeat("b", maxBucketName-len("-missing-")-len(tag)-1) + "-x",
		"RealisticLongName": "your-longer-project-name-map-blob-probe2",
	} {
		t.Run(name, func(t *testing.T) {
			got := missingBucketName(base, tag)
			if len(got) > maxBucketName {
				t.Errorf("%q is %d chars, over %d", got, len(got), maxBucketName)
			}
			if !strings.HasSuffix(got, tag) {
				t.Errorf("%q dropped the uniqueness tag", got)
			}
			if strings.Contains(got, "--") {
				t.Errorf("%q has an empty label from the truncation", got)
			}
			// This row exists to show that a realistic project-derived base
			// overflows, so it has to actually overflow. Sanitising the old
			// fixture for #514 shortened it by one character to exactly the
			// room available, which left the case passing on the branch it was
			// written to avoid — silently, since the three checks above and
			// statement coverage were all unaffected.
			if name == "RealisticLongName" && got == base+"-missing-"+tag {
				t.Errorf("%q (%d chars) no longer truncates, so this case stopped being the one it is named for",
					base, len(base))
			}
		})
	}
}

// liveClient builds the client the live tier runs against — the production one,
// with no options, which is the point of the tier.
//
// It refuses to proceed with STORAGE_EMULATOR_HOST set. storage.NewClient
// honours that variable, so a shell that still had it exported from some other
// tool would send this whole tier to a fake and report green — a live tier that
// can silently not be live is worse than no live tier, because the gap it exists
// to cover (production's XML read transport) would look covered.
func liveClient(t *testing.T, ctx context.Context) *storage.Client {
	t.Helper()
	if host := os.Getenv("STORAGE_EMULATOR_HOST"); host != "" {
		t.Fatalf("STORAGE_EMULATOR_HOST=%s is set: the live tier would run against an emulator "+
			"and report success without calling Cloud Storage; unset it or do not opt in", host)
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("live client (Application Default Credentials): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestLiveMissingBucketIsAnErrorNotAbsence carries the Ground-truth claim plan 22
// rests on — that a vanished bucket stays an error — into the tier that can
// actually check it. The hermetic version runs against the fake, whose 404 shapes
// are its own; this asks real Cloud Storage, which is the only thing that keeps
// the claim true as the client version moves. It creates nothing: the bucket name
// is one that does not exist, and every operation on it is expected to fail.
//
// The name has to stay a VALID one, which is why it is bounded rather than
// concatenated. GCS caps a bucket name at 63 characters, `GCS_BUCKET` is not
// length-limited, and an over-long name fails client-side validation — an error,
// and not `ErrNotFound`, so both assertions below would pass without the
// missing-bucket path ever being exercised. That is the "green for the wrong
// reason" this test exists to avoid, so the length is asserted too.
func TestLiveMissingBucketIsAnErrorNotAbsence(t *testing.T) {
	bucket := missingBucketName(gcstest.LiveBucket(t), randomTag(t))
	if len(bucket) > maxBucketName {
		t.Fatalf("derived bucket %q is %d chars, over GCS's %d — the test would pass on a "+
			"validation error rather than on absence", bucket, len(bucket), maxBucketName)
	}
	ctx := context.Background()
	s, err := gcs.New(ctx, gcs.Config{Bucket: bucket, Client: liveClient(t, ctx)})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	if _, _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("get from a missing bucket = %v, want a non-ErrNotFound error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete in a missing bucket reported success")
	}
}

// liveStore namespaces the shared suite's fixed keys and removes what it wrote.
// The suite names keys like "k" and "large" because every other backend hands it
// a throwaway bucket; the live tier runs against one real bucket an operator
// owns, so without this two runs would overwrite each other and every run would
// leave objects behind.
//
// It embeds the CONCRETE *gcs.Store, not blob.Store: prefixing is only complete
// while Put/Get/Delete are the whole interface, and embedding the interface would
// silently promote any method added later — forwarding a raw key to the
// operator's real bucket, unprefixed and unregistered for cleanup. With the
// concrete type, a new interface method fails to compile here until it is
// wrapped.
type liveStore struct {
	*gcs.Store
	prefix  string
	mu      sync.Mutex
	written map[string]bool
}

func (s *liveStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	s.mu.Lock()
	s.written[key] = true
	s.mu.Unlock()
	return s.Store.Put(ctx, s.prefix+key, r, size, contentType)
}

func (s *liveStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return s.Store.Get(ctx, s.prefix+key)
}

func (s *liveStore) Delete(ctx context.Context, key string) error {
	return s.Store.Delete(ctx, s.prefix+key)
}

// TestLiveContract runs the same shared suite against real Cloud Storage, under
// Application Default Credentials — the tier that keeps the hermetic fake
// honest. Opt in with RUN_LIVE_GCS_TESTS=1 and a GCS_BUCKET the credentials can
// reach; without it the suite skips and Cloud Storage is never called.
func TestLiveContract(t *testing.T) {
	bucket := gcstest.LiveBucket(t)
	ctx := context.Background()
	client := liveClient(t, ctx)

	var run atomic.Int64
	blobtest.Run(t, func(t *testing.T) blob.Store {
		t.Helper()
		s, err := gcs.New(ctx, gcs.Config{Bucket: bucket, Client: client})
		if err != nil {
			t.Fatalf("gcs.New: %v", err)
		}
		// The prefix has to be unique against every other run, not just against
		// the others in this process — it is the only thing standing between the
		// suite's fixed key names ("k", "large") and an operator's real objects,
		// and the suite overwrites and deletes what it addresses. A clock plus a
		// process-local counter cannot promise that across two concurrent runs,
		// so 8 random bytes carry it and the counter only keeps the subtests of
		// one run apart.
		live := &liveStore{Store: s, written: map[string]bool{},
			prefix: fmt.Sprintf("gcstest-live/%d-%s-%d/", time.Now().UnixNano(), randomTag(t), run.Add(1))}
		t.Cleanup(func() {
			live.mu.Lock()
			defer live.mu.Unlock()
			for key := range live.written {
				if err := s.Delete(context.Background(), live.prefix+key); err != nil {
					t.Errorf("cleanup %s: %v", live.prefix+key, err)
				}
			}
		})
		return live
	})
}
