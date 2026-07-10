// Package pgtest is test support: it starts one Dockerized Postgres per test
// binary and hands out freshly-migrated databases, the same pattern the
// store/events/api suites carry privately. New packages use this shared copy;
// production code must never import it. A missing Docker daemon is a hard
// failure, not a skip: skipped contract tests would silently hollow out the
// coverage gate.
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

// Main wraps testing.M: it starts the shared container, runs the suite, and
// tears the container down. Use from TestMain: os.Exit(pgtest.Main(m)).
func Main(m *testing.M) int {
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", pgImage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		fmt.Fprintf(os.Stderr, "contract tests require Docker for Postgres: %v\n", err)
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

// NewPool creates a fresh database in the shared container, migrates it, and
// returns a pool closed at test end.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := fmt.Sprintf("pgtest_%d", dbCounter.Add(1))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	_ = conn.Close(ctx)

	pool, err := store.Open(ctx, strings.TrimSuffix(adminDSN, "/postgres")+"/"+name)
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
	ctx := context.Background()
	agentID := domain.NewID("agent")
	envID = domain.NewID("env")
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
		{`INSERT INTO environments (id, name, kind, config) VALUES ($1, 'fixture', $2, $3)`,
			[]any{envID, envKind, `{"type":"` + envKind + `"}`}},
		{`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		  VALUES ($1, $2, 1, $3, $4, 'idle')`, []any{sessionID, agentID, resolved, envID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture insert: %v", err)
		}
	}
	return sessionID, envID
}
