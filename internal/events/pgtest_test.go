package events_test

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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// The event-log contract tests run against a real Postgres started in Docker,
// the same pattern as internal/store and internal/api. A missing Docker
// daemon is a hard failure, not a skip: skipped contract tests would silently
// hollow out the coverage gate.

const pgImage = "postgres:16-alpine"

var adminDSN string

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", pgImage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		fmt.Fprintf(os.Stderr, "events tests require Docker for Postgres: %v\n", err)
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

var dbCounter atomic.Int64

// newPool creates a fresh database in the shared container, migrates it, and
// returns a pool closed at test end.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := fmt.Sprintf("events_test_%d", dbCounter.Add(1))
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

// newPoolFromDSN opens a second pool over an existing migrated database,
// standing in for another control-plane replica.
func newPoolFromDSN(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// swapTracerProvider installs a tracer provider on the OTel global and
// returns a restore func, so span tests observe what production wiring emits.
func swapTracerProvider(tp trace.TracerProvider) func() {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return func() { otel.SetTracerProvider(prev) }
}

// newSession inserts the minimum fixture rows (agent, version, environment,
// session) and returns the session id.
func newSession(t *testing.T, pool *pgxpool.Pool) domain.ID {
	t.Helper()
	return newSessionKind(t, pool, "cloud")
}

func newSessionKind(t *testing.T, pool *pgxpool.Pool, kind string) domain.ID {
	t.Helper()
	ctx := context.Background()
	agentID := domain.NewID("agent")
	envID := domain.NewID("env")
	sessionID := domain.NewID("sesn")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO agents (id, name, spec) VALUES ($1, 'a', '{}')`, []any{agentID.String()}},
		{`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, 1, 'a', '{}')`, []any{agentID.String()}},
		{`INSERT INTO environments (id, name, kind, config) VALUES ($1, 'e', $2, $3)`,
			[]any{envID.String(), kind, `{"type":"` + kind + `"}`}},
		{`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id) VALUES ($1, $2, 1, '{}', $3)`,
			[]any{sessionID.String(), agentID.String(), envID.String()}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture %s: %v", q.sql, err)
		}
	}
	return sessionID
}
