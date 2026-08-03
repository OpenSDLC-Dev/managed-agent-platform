// Package secretstest is test support for the secrets.Cipher seam: the shared
// contract suite (contract.go) plus a Dockerized OpenBao dev-mode container
// started once per test binary, with the transit engine mounted and per-test
// key names handed out. Production code must never import it. A missing
// Docker daemon is a hard failure, not a skip: skipped contract tests would
// silently hollow out the coverage gate (the pgtest rule).
package secretstest

import (
	"bytes"
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

// Image is the pinned OpenBao the harness runs — the same release
// deploy/compose and the helm chart default to, so the contract tests
// exercise what ships.
const Image = "openbao/openbao:2.6.1"

// RootToken is the dev-mode root token for the throwaway container.
const RootToken = "secretstest-root"

var (
	addr       string
	keyCounter atomic.Int64
)

// readyTimeout bounds one container's readiness wait; the retry in Main with
// a fresh container — not a longer wait — is what heals the dead-port-mapping
// flake this guards against (#265; the full rationale is on pgtest's
// readyTimeout, whose rule this follows).
const readyTimeout = 150 * time.Second

// Main wraps testing.M: it starts the shared OpenBao dev container, mounts
// the transit engine, runs the suite, and tears the container down. Use from
// TestMain: os.Exit(secretstest.Main(m)). The start is attempted twice, with
// a fresh container in between (#265).
func Main(m *testing.M) int {
	containerID, err := startReady()
	if err != nil {
		fmt.Fprintf(os.Stderr, "secretstest: %v; retrying with a fresh container\n", err)
		if containerID, err = startReady(); err != nil {
			fmt.Fprintf(os.Stderr, "secretstest: %v\n", err)
			return 1
		}
	}
	defer removeContainer(containerID)

	if err := mountTransit(addr); err != nil {
		fmt.Fprintf(os.Stderr, "mount transit engine: %v\n", err)
		return 1
	}
	return m.Run()
}

// startReady runs one OpenBao dev container, waits for its health endpoint,
// and sets addr. On failure the container is removed and the error carries
// its state and last log lines — the only forensics a dead start leaves.
func startReady() (string, error) {
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "BAO_DEV_ROOT_TOKEN_ID="+RootToken,
		"-p", "127.0.0.1:0:8200", Image).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		return "", fmt.Errorf("contract tests require Docker for OpenBao: %w", err)
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", errors.New("docker run printed no container ID")
	}
	port, err := hostPort(containerID)
	if err != nil {
		removeContainer(containerID)
		return "", fmt.Errorf("resolve openbao port: %w", err)
	}
	candidate := "http://127.0.0.1:" + port
	if err := waitReady(candidate, readyTimeout); err != nil {
		diag := containerDiag(containerID)
		removeContainer(containerID)
		return "", fmt.Errorf("openbao never became ready: %v (%s)", err, diag)
	}
	addr = candidate
	return containerID, nil
}

// removeContainer force-removes with -v for the same reason as blobtest:
// --rm's auto-remove does not fire on a force-remove, so without -v every
// test binary would leak the image's anonymous volume (the pgtest rule).
func removeContainer(containerID string) {
	_ = exec.Command("docker", "rm", "-f", "-v", containerID).Run()
}

// containerDiag captures the container's state and last log lines for the
// failure message, distinguishing a crashed container from a live one behind
// a dead port mapping.
func containerDiag(containerID string) string {
	state, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", containerID).Output()
	logs, _ := exec.Command("docker", "logs", "--tail", "3", containerID).CombinedOutput()
	return fmt.Sprintf("container state %s; last log: %s",
		strings.TrimSpace(string(state)), strings.TrimSpace(string(logs)))
}

// Addr returns the dev container's base URL (http://host:port).
func Addr(t *testing.T) string {
	t.Helper()
	if addr == "" {
		t.Fatal("secretstest.Main did not run; wire it into TestMain")
	}
	return addr
}

// FreshKey returns a transit key name unique to this call. The backend under
// test is expected to create the key itself.
func FreshKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("secretstest-k%d", keyCounter.Add(1))
}

func hostPort(containerID string) (string, error) {
	out, err := exec.Command("docker", "port", containerID, "8200/tcp").Output()
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

func waitReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := client.Get(addr + "/v1/sys/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK { // dev mode: initialized + unsealed
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

func mountTransit(addr string) error {
	req, err := http.NewRequest(http.MethodPost, addr+"/v1/sys/mounts/transit",
		bytes.NewReader([]byte(`{"type":"transit"}`)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", RootToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mount transit: status %d", resp.StatusCode)
	}
	return nil
}
