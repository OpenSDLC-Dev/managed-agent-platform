package store_test

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

	"github.com/jackc/pgx/v5"
)

// The store contract tests run against a real Postgres started in Docker —
// psql is not installed locally, and the CI runner has Docker. A missing
// Docker daemon is a hard failure, not a skip: skipped store tests would
// silently hollow out the coverage gate.

const pgImage = "postgres:16-alpine"

var adminDSN string

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	// stdout carries exactly the container ID; image-pull noise goes to
	// stderr (surfaced via ExitError on failure).
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", pgImage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		fmt.Fprintf(os.Stderr, "store tests require Docker for Postgres: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "resolve postgres port: %v\n", err)
		return 1
	}
	adminDSN = fmt.Sprintf("postgres://postgres:test@127.0.0.1:%s/postgres", port)
	if err := waitReady(adminDSN, 120*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "postgres never became ready: %v\n", err)
		return 1
	}
	return m.Run()
}

func hostPort(containerID string) (string, error) {
	out, err := exec.Command("docker", "port", containerID, "5432/tcp").Output()
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

func waitReady(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		// Connect completes the full startup/auth handshake; success is
		// readiness, no extra ping needed.
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_ = conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

var dbCounter atomic.Int64

// freshDB creates a new empty database in the shared container and returns
// its DSN, so every test migrates from a clean slate.
func freshDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("store_test_%d", dbCounter.Add(1))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	return strings.TrimSuffix(adminDSN, "/postgres") + "/" + name
}
