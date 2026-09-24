// Package blobtest is test support for the blob.Store seam: it starts one
// Dockerized Silo (a MinIO fork, see Image) per test binary and hands out
// per-test targets (endpoint, credentials, fresh bucket name) for backends to
// construct stores against.
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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Image is the pinned object store the harness runs — the same image
// deploy/compose and the helm chart default to, so the contract tests exercise
// what ships; pins_test.go fails when one of the three moves without the
// others.
//
// It is PGSTY Silo, a maintained fork of the MinIO server, not MinIO itself,
// and every one of those three pins changed for the same reason: MinIO took
// its community images off both registries it had published them to. Docker
// Hub stopped serving the `minio` namespace between 2026-09-11 and 2026-09-12
// (#701), and quay.io,
// which the pins moved to then, began refusing `minio/minio` with a 401 on
// 2026-09-24 (#799) — each time a hard failure on any machine without the
// image already cached. Silo keeps the server's wire and operator surface,
// which is what the three pins depend on: the S3 API, the MINIO_ROOT_*
// variables, the /minio/health/* routes, `server /data` as the container's
// arguments, and `mc` in the image. The digest pins the bytes as well as the
// release: the image comes from one third-party publisher, and a re-pushed
// tag would otherwise change the server every fresh machine pulls, unnoticed.
const Image = "pgsty/silo:RELEASE.2026-09-16T00-00-00Z@sha256:635197cb9f36d01bee221d34d1c7d7960f6a95c48b0b6c01d99cd13bdae51a46"

// Root credentials for the throwaway container (the server requires a password
// of at least 8 characters).
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

// readyTimeout bounds one container's readiness wait; the retry in Main with
// a fresh container — not a longer wait — is what heals the dead-port-mapping
// flake this guards against (#265; the full rationale is on pgtest's
// readyTimeout, whose rule this follows).
const readyTimeout = 150 * time.Second

// Main wraps testing.M: it starts the shared Silo container, runs the suite,
// and tears the container down. Use from TestMain: os.Exit(blobtest.Main(m)).
// The start is attempted twice, with a fresh container in between (#265; the
// retry's rationale is on pgtest.Main, whose rule this follows). It opens by
// reaping what an earlier killed run left behind, the defer below being
// unreachable to one (#346; the rationale is in dockertest).
func Main(m *testing.M) int {
	dockertest.SweepStrays("blobtest")
	containerID, err := startReady()
	if err != nil {
		fmt.Fprintf(os.Stderr, "blobtest: %v; retrying\n", err)
		if containerID, err = startReady(); err != nil {
			fmt.Fprintf(os.Stderr, "blobtest: %v\n", err)
			return 1
		}
	}
	defer removeContainer(containerID)
	return m.Run()
}

// startReady runs one Silo container, waits for its object layer to serve
// S3, and sets endpoint. On failure the container is removed and the error
// carries its state and last log lines — the only forensics a dead start
// leaves.
func startReady() (string, error) {
	// No --rm: a container whose entrypoint crashes would be auto-removed by
	// the daemon before containerDiag could read its state and logs — the one
	// forensic a dead start leaves. Removal is wholly removeContainer's job.
	out, err := exec.Command("docker", dockertest.RunArgs("blobtest",
		"-e", "MINIO_ROOT_USER="+RootUser,
		"-e", "MINIO_ROOT_PASSWORD="+RootPassword,
		"-p", "127.0.0.1:0:9000", Image, "server", "/data")...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		return "", fmt.Errorf("contract tests require Docker for Silo: %w", err)
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", errors.New("docker run printed no container ID")
	}
	port, err := hostPort(containerID)
	if err != nil {
		removeContainer(containerID)
		return "", fmt.Errorf("resolve silo port: %w", err)
	}
	candidate := "127.0.0.1:" + port
	if err := waitReady(candidate, containerID, readyTimeout); err != nil {
		diag := containerDiag(containerID)
		removeContainer(containerID)
		return "", fmt.Errorf("silo never became ready: %v (%s)", err, diag)
	}
	endpoint = candidate
	return containerID, nil
}

// removeContainer force-removes with -v to reap the anonymous volume the
// Silo image declares (VOLUME /data, which `server /data` writes to) — the
// container runs without --rm so a crash leaves evidence for containerDiag,
// making removal wholly this function's job (the pgtest rule, where the full
// rationale and the timeout's reason live). A failed removal is reported
// rather than swallowed: on the retry path it means the dead first container
// is still on the machine.
func removeContainer(containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", containerID).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "blobtest: remove container %s: %v: %s\n",
			containerID, err, strings.TrimSpace(string(out)))
	}
}

// containerState asks the daemon for the container's status ("running",
// "exited", ...), empty when the daemon cannot answer. Bounded so a wedged
// daemon cannot hang a failure path.
func containerState(containerID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}}", containerID).Output()
	return strings.TrimSpace(string(out))
}

// containerDiag captures the container's state and last log lines for the
// failure message, distinguishing a crashed container from a live one behind
// a dead port mapping.
func containerDiag(containerID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "3", containerID).CombinedOutput()
	return fmt.Sprintf("container state %s; last log: %s",
		containerState(containerID), strings.TrimSpace(string(logs)))
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
// precedes it, which is the request s3.New actually died on (#208). That was
// measured on MinIO; Silo's binary still carries both the header and the
// error, and this gate holds whichever server answers.
//
// So gate on the call the suite itself makes, against a bucket name no test
// uses. The client is built the way a backend under test builds it — no region
// configured — so the probe covers the same location lookup.
func waitReady(endpoint, containerID string, timeout time.Duration) error {
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(RootUser, RootPassword, ""),
	})
	if err != nil {
		return err
	}
	const poll = 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	// One deadline for the whole gate: minio-go's own retries stop with it
	// rather than outliving the loop that started them.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	// Coarse-cadence liveness check, as in pgtest.waitReady: an exited
	// container can never become ready, so stop waiting on it.
	nextLiveness := time.Now().Add(5 * time.Second)
	for {
		if _, err = client.BucketExists(ctx, "blobtest-probe"); err == nil {
			return nil
		}
		// Give up while the error is still one a request produced. Sleeping
		// into the deadline instead would spend the last attempt on a context
		// the shared deadline has already expired, and Main would report
		// "context deadline exceeded" rather than what the server said — the
		// line that made #208 diagnosable at all.
		if time.Now().Add(poll).After(deadline) {
			return err
		}
		if time.Now().After(nextLiveness) {
			nextLiveness = time.Now().Add(5 * time.Second)
			if state := containerState(containerID); state == "exited" || state == "dead" {
				return fmt.Errorf("%v (container %s before the wait expired)", err, state)
			}
		}
		time.Sleep(poll)
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
