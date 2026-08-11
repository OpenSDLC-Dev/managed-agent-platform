// Package gcstest is test support for the GCS backend: one Dockerized
// fake-gcs-server per test binary, per-test fresh buckets, and the gate for the
// opt-in tier that calls real Cloud Storage. It is blobtest's counterpart for a
// backend blobtest's MinIO cannot serve — the shared contract suite itself
// stays in blobtest, and this only supplies targets to run it against.
// Production code must never import this package. A missing Docker daemon is a
// hard failure, not a skip: skipped contract tests would silently hollow out
// the coverage gate (the pgtest rule).
package gcstest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Image is the pinned fake-gcs-server the harness runs. Pinned for the same
// reason blobtest pins its MinIO: a suite whose fixture floats is a suite that
// can change its mind about what the contract means.
const Image = "fsouza/fake-gcs-server:1.52.2"

// Project is the project id the fake wants on a bucket create. The fake
// authenticates nothing, so this is a label, not an identity.
const Project = "gcstest"

var (
	endpoint      string
	bucketCounter atomic.Int64
)

// Main wraps testing.M: it starts the shared fake, runs the suite, and tears the
// container down. Use from TestMain: os.Exit(gcstest.Main(m)). It opens by
// reaping what an earlier killed run left behind: the defer below is
// unreachable to one, and --rm only removes a container that has exited, which
// an abandoned fake never does (#346; the rationale is in dockertest).
func Main(m *testing.M) int {
	dockertest.SweepStrays("gcstest")
	out, err := exec.Command("docker", dockertest.RunArgs("gcstest", "--rm",
		"-p", "127.0.0.1:0:4443", Image,
		"-scheme", "http", "-port", "4443")...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		fmt.Fprintf(os.Stderr, "contract tests require Docker for fake-gcs-server: %v\n", err)
		return 1
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		fmt.Fprintln(os.Stderr, "docker run printed no container ID")
		return 1
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", "-v", containerID).Run() }()

	port, err := hostPort(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve fake-gcs port: %v\n", err)
		return 1
	}
	endpoint = "127.0.0.1:" + port
	if err := waitReady(endpoint, 120*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "fake-gcs-server never became ready: %v\n", err)
		return 1
	}
	return m.Run()
}

func hostPort(containerID string) (string, error) {
	out, err := exec.Command("docker", "port", containerID, "4443/tcp").Output()
	if err != nil {
		return "", err
	}
	first := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	idx := strings.LastIndex(first, ":")
	if idx < 0 {
		return "", fmt.Errorf("unexpected docker port output %q", out)
	}
	return first[idx+1:], nil
}

// waitReady gates on the call the suite itself makes first — a bucket create —
// rather than on the container being up, so a fake that is listening but not yet
// serving cannot admit the suite (blobtest's #208 lesson, restated).
func waitReady(endpoint string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := newClient(ctx, endpoint)
	if err != nil {
		return err
	}
	defer client.Close()
	const poll = 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		err = client.Bucket("gcstest-probe").Create(ctx, Project, nil)
		if err == nil || bucketExists(err) {
			// Serving is what "ready" means, and a 409 says the fake answered
			// about state it already holds — which is what a create whose reply
			// was lost leaves behind. Without this the retry would then poll a
			// working fake until the timeout. (v1.56.0 has no ErrBucketExist, so
			// the status code is the only signal.)
			return nil
		}
		if time.Now().Add(poll).After(deadline) {
			return err
		}
		time.Sleep(poll)
	}
}

// bucketExists reports whether the error is the endpoint saying the bucket is
// already there.
func bucketExists(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusConflict
}

// newClient points the Google client at the fake. Two options carry the whole
// hookup and both are load-bearing: WithoutAuthentication because the fake
// issues no credentials, and WithJSONReads because reads otherwise go to the
// XML media host derived from the bucket name rather than to the endpoint —
// against a container on an ephemeral port, every read would 404. (The
// STORAGE_EMULATOR_HOST route fixes reads too, but only when the fake's
// -public-host matches the published address, which a random host port cannot
// promise.)
//
// WithJSONReads is therefore a gap as well as a fix, and it is named rather than
// hidden: production builds its client with no such option, so its reads take the
// XML path this harness cannot exercise. Everything the contract asserts about
// read SEMANTICS holds either way, but the read TRANSPORT the fake exercises is
// not the one production uses. The live tier (TestLiveContract, gated by LiveEnv)
// is that gap's only cover — it runs the same shared suite against real Cloud
// Storage through the production client, options and all.
func newClient(ctx context.Context, endpoint string) (*storage.Client, error) {
	return storage.NewClient(ctx,
		option.WithEndpoint("http://"+endpoint+"/storage/v1/"),
		option.WithoutAuthentication(),
		storage.WithJSONReads())
}

// FreshBucket creates a bucket no other test uses and returns a client for it.
// The client is closed when the test ends; the backend under test is expected to
// take it as its injected client rather than build its own.
func FreshBucket(t *testing.T) (*storage.Client, string) {
	t.Helper()
	if endpoint == "" {
		t.Fatal("gcstest.Main did not run; wire it into TestMain")
	}
	ctx := context.Background()
	client, err := newClient(ctx, endpoint)
	if err != nil {
		t.Fatalf("fake-gcs client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	bucket := fmt.Sprintf("gcstest-%d", bucketCounter.Add(1))
	if err := client.Bucket(bucket).Create(ctx, Project, nil); err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	return client, bucket
}

// LiveEnv is the consent variable for the tier that calls real Cloud Storage. It
// is read from the environment only — never from .env, so consent can never come
// from disk.
const LiveEnv = "RUN_LIVE_GCS_TESTS"

// BucketEnv names the bucket the live tier uses, read from the environment or
// the repo-root .env. Exactly this one key ever reaches the file. It is not a
// credential — authentication is Application Default Credentials — and it rides
// .env only because that is where this repo keeps live-tier configuration.
const BucketEnv = "GCS_BUCKET"

// LiveBucket gates a live test and returns the bucket name. Not opted in: the
// test skips and the file is never opened. Opted in with the bucket missing: the
// test FAILS — a tier that skips itself when its configuration rots is not a
// safety net.
func LiveBucket(t *testing.T) string {
	t.Helper()
	bucket, skip, err := liveBucket(resolve)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return bucket
}

// liveBucket is the rule LiveBucket applies, split out for its own tests.
func liveBucket(getenv func(string) string) (bucket, skip string, err error) {
	if getenv(LiveEnv) == "" {
		return "", fmt.Sprintf("%s is not set: skipping the live GCS tier (Cloud Storage is not called)", LiveEnv), nil
	}
	bucket = getenv(BucketEnv)
	if bucket == "" {
		return "", "", fmt.Errorf("%s opted into the live GCS tier but %s is unset: "+
			"set it in the environment or the repo-root .env", LiveEnv, BucketEnv)
	}
	return bucket, "", nil
}

// resolve reads one key for the production gate.
func resolve(key string) string { return lookup(os.LookupEnv, dotEnv, key) }

// lookup resolves a key from the environment, falling back to the repo-root
// .env for the bucket name only. The environment always wins — including when it
// sets the key to the empty string, which is an answer and not an invitation for
// the file to supply one.
func lookup(lookupEnv func(string) (string, bool), file func() map[string]string, key string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	if key != BucketEnv {
		return ""
	}
	return file()[key]
}

// dotEnv parses the repo-root .env once, on first use. The values stay here
// rather than being pushed into the process environment: an os.Setenv would
// outlive the test that triggered it. A missing file is not an error — the
// environment may carry everything.
var dotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(repoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDotEnv(f)
})

// repoRoot derives the checkout root from this file's compile-time path, so a
// worktree reaches its own .env rather than the main checkout's.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

func parseDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(key) != BucketEnv {
			continue
		}
		out[BucketEnv] = parseValue(value)
	}
	return out
}

// parseValue takes the value side of one .env line, exactly as modeltest,
// webtooltest and gcpkmstest read the same file: a quoted value is whatever the
// quotes enclose, so a '#' inside them is content; an unquoted one runs to a '#'
// that follows whitespace and keeps one that does not.
func parseValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if i := commentStart(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func commentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
