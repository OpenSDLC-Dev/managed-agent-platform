package events_test

import (
	"context"
	"os"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Package-local fixtures for the event-log suite; the shared Docker-Postgres
// harness (container lifecycle, migrated pools) is internal/pgtest.

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
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
