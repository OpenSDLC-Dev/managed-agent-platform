// Package blobtest is test support for the blob.Store seam: it starts one
// Dockerized MinIO per test binary and hands out per-test targets (endpoint,
// credentials, fresh bucket name) for backends to construct stores against.
// Production code must never import it. A missing Docker daemon is a hard
// failure, not a skip: skipped contract tests would silently hollow out the
// coverage gate (the pgtest rule).
package blobtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Image is the pinned MinIO the harness runs — the same release deploy/compose
// and the helm chart default to, so the contract tests exercise what ships.
const Image = "minio/minio:RELEASE.2025-09-07T16-13-09Z"

// Root credentials for the throwaway container (MinIO requires a password of
// at least 8 characters).
const (
	RootUser     = "blobtest"
	RootPassword = "blobtest-secret"
)

var (
	endpoint      string
	bucketCounter atomic.Int64
)

// Target is one test's connection coordinates: the shared container's
// endpoint and credentials plus a bucket name no other test uses. The backend
// under test is expected to create the bucket itself.
type Target struct {
	Endpoint  string // host:port, plain HTTP
	AccessKey string
	SecretKey string
	Bucket    string
}

// Main wraps testing.M: it starts the shared MinIO container, runs the suite,
// and tears the container down. Use from TestMain: os.Exit(blobtest.Main(m)).
func Main(m *testing.M) int {
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "MINIO_ROOT_USER="+RootUser,
		"-e", "MINIO_ROOT_PASSWORD="+RootPassword,
		"-p", "127.0.0.1:0:9000", Image, "server", "/data").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		fmt.Fprintf(os.Stderr, "contract tests require Docker for MinIO: %v\n", err)
		return 1
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		fmt.Fprintln(os.Stderr, "docker run printed no container ID")
		return 1
	}
	// -v reaps the anonymous volume the MinIO image declares (VOLUME /data,
	// which `server /data` writes to). The --rm above does not cover it:
	// auto-remove only fires when the container exits on its own, not when this
	// force-removes it mid-run, so without -v every test binary leaks one volume
	// per run (the pgtest rule).
	defer func() { _ = exec.Command("docker", "rm", "-f", "-v", containerID).Run() }()

	port, err := hostPort(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve minio port: %v\n", err)
		return 1
	}
	endpoint = "127.0.0.1:" + port
	if err := waitReady(endpoint, 120*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "minio never became ready: %v\n", err)
		return 1
	}
	return m.Run()
}

func hostPort(containerID string) (string, error) {
	out, err := exec.Command("docker", "port", containerID, "9000/tcp").Output()
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

// waitReady blocks until the container answers an S3 request, which is a
// strictly stronger signal than its readiness endpoint gives: MinIO's
// /minio/health/ready handler returns 200 even when the object layer is nil,
// reporting that state only in an x-minio-server-status header it sets
// alongside the 200. A suite admitted in that window fails its first call with
// "Server not initialized yet, please try again", instantly — minio-go retries
// that 503 on the bucket HEAD but not on the bucket-location lookup that
// precedes it, which is the request s3.New actually died on (#208).
//
// So gate on the call the suite itself makes, against a bucket name no test
// uses. The client is built the way a backend under test builds it — no region
// configured — so the probe covers the same location lookup.
func waitReady(endpoint string, timeout time.Duration) error {
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(RootUser, RootPassword, ""),
	})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	// One deadline for the whole gate: minio-go's own retries stop with it
	// rather than outliving the loop that started them.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		if _, err = client.BucketExists(ctx, "blobtest-probe"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// FreshTarget returns the shared container's coordinates with a bucket name
// unique to this call.
func FreshTarget(t *testing.T) Target {
	t.Helper()
	if endpoint == "" {
		t.Fatal("blobtest.Main did not run; wire it into TestMain")
	}
	return Target{
		Endpoint:  endpoint,
		AccessKey: RootUser,
		SecretKey: RootPassword,
		Bucket:    fmt.Sprintf("blobtest-%d", bucketCounter.Add(1)),
	}
}
