// Package blobtest is test support for the blob.Store seam: it starts one
// Dockerized MinIO per test binary and hands out per-test targets (endpoint,
// credentials, fresh bucket name) for backends to construct stores against.
// Production code must never import it. A missing Docker daemon is a hard
// failure, not a skip: skipped contract tests would silently hollow out the
// coverage gate (the pgtest rule).
package blobtest

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	defer func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() }()

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

func waitReady(endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get("http://" + endpoint + "/minio/health/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("health status %d", resp.StatusCode)
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
