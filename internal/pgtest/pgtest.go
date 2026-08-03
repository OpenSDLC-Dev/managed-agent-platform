// Package pgtest is test support: it starts one Dockerized Postgres per test
// binary and hands out fresh databases — migrated pools via NewPool, or bare
// DSNs via FreshDB for suites that exercise store.Open/Migrate themselves.
// Every Postgres-backed suite uses it; production code must never import it.
// A missing Docker daemon is a hard failure, not a skip: skipped contract
// tests would silently hollow out the coverage gate.
package pgtest

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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgImage = "postgres:16-alpine"

var (
	adminDSN  string
	dbCounter atomic.Int64
)

// readyTimeout bounds one container's readiness wait. Longer would not help:
// the flake this guards against (#265) is a published port that stays
// connection-refused forever — observed unhealed at both a 120s and a 300s
// ceiling on a crowded Docker daemon — which more waiting on the same
// container never fixes, where Main's retry with a fresh container (and thus
// a fresh port mapping) does.
const readyTimeout = 150 * time.Second

// Main wraps testing.M: it starts the shared container, runs the suite, and
// tears the container down. Use from TestMain: os.Exit(pgtest.Main(m)).
// The start is attempted twice, with a fresh container in between (#265; see
// readyTimeout).
func Main(m *testing.M) int {
	containerID, err := startReady()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v; retrying with a fresh container\n", err)
		if containerID, err = startReady(); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
			return 1
		}
	}
	defer removeContainer(containerID)
	return m.Run()
}

// startReady runs one Postgres container, waits for its published port to
// accept a connection, and sets adminDSN. On failure the container is removed
// and the error carries its state and last log lines — the only forensics a
// dead start leaves.
func startReady() (string, error) {
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", pgImage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		return "", fmt.Errorf("contract tests require Docker for Postgres: %w", err)
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", errors.New("docker run printed no container ID")
	}
	port, err := hostPort(containerID)
	if err != nil {
		removeContainer(containerID)
		return "", fmt.Errorf("resolve postgres port: %w", err)
	}
	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%s/postgres", port)
	if err := waitReady(dsn, readyTimeout); err != nil {
		diag := containerDiag(containerID)
		removeContainer(containerID)
		return "", fmt.Errorf("postgres never became ready: %v (%s)", err, diag)
	}
	adminDSN = dsn
	return containerID, nil
}

// removeContainer force-removes with -v: the Postgres image declares VOLUME
// /var/lib/postgresql/data, and --rm's auto-remove only fires when the
// container exits on its own — without -v every test binary leaks one
// anonymous volume per run.
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
		// 5s per attempt: under the same load a dial+auth round trip can
		// legitimately outlast 2s, and a too-short attempt keeps cancelling
		// a connection that would have completed.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

// FreshDB creates a new empty database in the shared container and returns
// its DSN, un-migrated, so a suite can exercise store.Open/Migrate itself
// from a clean slate.
func FreshDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("pgtest_%d", dbCounter.Add(1))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	return strings.TrimSuffix(adminDSN, "/postgres") + "/" + name
}

// NewPool creates a fresh database in the shared container, migrates it, and
// returns a pool closed at test end.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(context.Background(), FreshDB(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// NewSession inserts the minimum fixture rows (agent, agent version,
// environment of the given kind, session) and returns the session and
// environment ids.
func NewSession(t *testing.T, pool *pgxpool.Pool, envKind string) (sessionID, envID domain.ID) {
	t.Helper()
	envID = domain.NewID("env")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO environments (id, name, kind, config) VALUES ($1, 'fixture', $2, $3)`,
		envID, envKind, `{"type":"`+envKind+`"}`); err != nil {
		t.Fatalf("fixture insert: %v", err)
	}
	return NewSessionInEnv(t, pool, envID), envID
}

// NewSessionInEnv inserts an additional idle session (with its own throwaway
// agent + version) into an existing environment and returns the session id. Use
// it to place several sessions — and thus several work items — under one
// environment, since Enqueue dedupes per (session, kind) while a live item
// exists.
func NewSessionInEnv(t *testing.T, pool *pgxpool.Pool, envID domain.ID) (sessionID domain.ID) {
	t.Helper()
	ctx := context.Background()
	agentID := domain.NewID("agent")
	sessionID = domain.NewID("sesn")
	resolved := fmt.Sprintf(`{"type":"agent","id":%q,"version":1,"name":"fixture",`+
		`"model":{"id":"fixture-model"},"system":"","description":"",`+
		`"tools":[],"mcp_servers":[],"skills":[],"multiagent":null}`, agentID)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO agents (id, name, version, spec) VALUES ($1, 'fixture', 1, '{"model":{"id":"fixture-model"}}')`,
			[]any{agentID}},
		{`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, 1, 'fixture', '{"model":{"id":"fixture-model"}}')`,
			[]any{agentID}},
		{`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		  VALUES ($1, $2, 1, $3, $4, 'idle')`, []any{sessionID, agentID, resolved, envID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture insert: %v", err)
		}
	}
	return sessionID
}
