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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
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
// readyTimeout). The retry is unconditional on the error's class because even
// a failed `docker run` can be load-induced (port programming races under a
// crowded daemon); when the daemon is simply absent, the second attempt fails
// in milliseconds and costs nothing.
//
// Teardown below is a defer, which a killed process never reaches, so the run
// opens by reaping what an earlier killed run left (#346; dockertest holds the
// whole rationale, including why a container young enough to be a peer's is
// never touched).
func Main(m *testing.M) int {
	dockertest.SweepStrays("pgtest")
	containerID, err := startReady()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v; retrying\n", err)
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
	// No --rm: a container whose entrypoint crashes would be auto-removed by
	// the daemon before containerDiag could read its state and logs — the one
	// forensic a dead start leaves. Removal is wholly removeContainer's job.
	out, err := exec.Command("docker", dockertest.RunArgs("pgtest",
		"-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", pgImage)...).Output()
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
	if err := waitReady(dsn, containerID, readyTimeout); err != nil {
		diag := containerDiag(containerID)
		removeContainer(containerID)
		return "", fmt.Errorf("postgres never became ready: %v (%s)", err, diag)
	}
	adminDSN = dsn
	return containerID, nil
}

// removeContainer force-removes with -v: the container runs without --rm (so
// a crash leaves evidence for containerDiag), and the Postgres image declares
// VOLUME /var/lib/postgresql/data — without -v every test binary would leak
// one anonymous volume per run. A failed removal is reported rather than
// swallowed: on the retry path it means the dead first container is still on
// the machine, and a silent leak here is indistinguishable from a healthy run.
// The timeout keeps a wedged daemon from hanging the run inside TestMain,
// where go test's own alarm is not yet armed.
func removeContainer(containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", containerID).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: remove container %s: %v: %s\n",
			containerID, err, strings.TrimSpace(string(out)))
	}
}

// containerState asks the daemon for the container's status ("running",
// "exited", ...), empty when the daemon cannot answer. Bounded like
// removeContainer: this runs on failure paths where a wedged daemon must not
// hang the report.
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

func waitReady(dsn, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// A container that has exited can never become ready, so waiting out the
	// budget on it only delays the retry (and the report). Liveness is checked
	// on a coarse cadence — the readiness probe stays the primary signal and
	// the check must not add meaningful load to the crowded daemon it runs on.
	nextLiveness := time.Now().Add(5 * time.Second)
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
		if time.Now().After(nextLiveness) {
			nextLiveness = time.Now().Add(5 * time.Second)
			// Only states that end a container are terminal here; an empty
			// answer (daemon hiccup) keeps polling rather than giving up.
			if state := containerState(containerID); state == "exited" || state == "dead" {
				return fmt.Errorf("%v (container %s before the wait expired)", err, state)
			}
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

// defaultScope is the tenancy triple a fixture writes when its caller names no
// other: the single-tenant defaults every scoped table has declared since 0001,
// so the ordinary fixture keeps writing exactly what the schema wrote for it.
// The workspace is the registry's own id rather than a literal, because it is
// the one member of the triple a row can be asked to disagree with.
var defaultScope = domain.Scope{
	OrgID: "default", WorkspaceID: domain.DefaultWorkspaceID, ProjectID: "default",
}

// NewSession inserts the minimum fixture rows (agent, agent version,
// environment of the given kind, session) in the default workspace and returns
// the session and environment ids.
func NewSession(t *testing.T, pool *pgxpool.Pool, envKind string) (sessionID, envID domain.ID) {
	t.Helper()
	return NewSessionInScope(t, pool, envKind, defaultScope)
}

// NewSessionInScope is NewSession in the named scope — a second workspace's
// fixture, for the isolation tests. It is the one place a fixture CHOOSES a
// tenant: every row below the environment reads its scope off the row it hangs
// from, so no fixture can assemble a chain whose halves disagree about whose
// data it is.
func NewSessionInScope(t *testing.T, pool *pgxpool.Pool, envKind string, scope domain.Scope) (sessionID, envID domain.ID) {
	t.Helper()
	envID = domain.NewID("env")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO environments (id, name, kind, config, org_id, workspace_id, project_id)
		 VALUES ($1, 'fixture', $2, $3, $4, $5, $6)`,
		envID, envKind, `{"type":"`+envKind+`"}`,
		scope.OrgID, scope.WorkspaceID, scope.ProjectID); err != nil {
		t.Fatalf("fixture insert: %v", err)
	}
	return NewSessionInEnv(t, pool, envID), envID
}

// NewSessionInEnv inserts an additional idle session (with its own throwaway
// agent + version) into an existing environment and returns the session id. Use
// it to place several sessions — and thus several work items — under one
// environment, since Enqueue dedupes per (session, kind) while a live item
// exists.
//
// The session inherits the environment's scope, and its agent and primary
// thread inherit it in turn, each SELECTing the columns off its parent row
// rather than being told them. agent_versions takes none: it has no scope
// columns and inherits through its foreign key, as 0001_init.sql:9-10 says.
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
		{`INSERT INTO agents (id, name, version, spec, org_id, workspace_id, project_id)
		  SELECT $1, 'fixture', 1, '{"model":{"id":"fixture-model"}}', e.org_id, e.workspace_id, e.project_id
		    FROM environments e WHERE e.id = $2`, []any{agentID, envID}},
		{`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, 1, 'fixture', '{"model":{"id":"fixture-model"}}')`,
			[]any{agentID}},
		{`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status,
		                        org_id, workspace_id, project_id)
		  SELECT $1, $2, 1, $3, $4, 'idle', e.org_id, e.workspace_id, e.project_id
		    FROM environments e WHERE e.id = $4`, []any{sessionID, agentID, resolved, envID}},
		// The primary thread every session has (plan 35): status the session's,
		// no agent of its own.
		{`INSERT INTO session_threads (id, session_id, agent_name, status, org_id, workspace_id, project_id)
		  SELECT $1, $2, 'fixture', 'idle', s.org_id, s.workspace_id, s.project_id
		    FROM sessions s WHERE s.id = $2`, []any{domain.PrimaryThreadID(sessionID), sessionID}},
	} {
		tag, err := pool.Exec(ctx, q.sql, q.args...)
		if err != nil {
			t.Fatalf("fixture insert: %v", err)
		}
		// A row whose scope is SELECTed from its parent inserts NOTHING when the
		// parent is missing, where the old VALUES form raised a foreign-key
		// error. Without this the fixture would hand back an id for a row it
		// never wrote.
		if tag.RowsAffected() != 1 {
			t.Fatalf("fixture insert wrote %d rows (missing parent?): %s", tag.RowsAffected(), q.sql)
		}
	}
	return sessionID
}

// SetSessionStatus moves a fixture session to status the way the platform
// does — the primary thread's row and the session's column together (plan 35
// decision 4: the session's status is a fold over its threads', so a fixture
// that moved the column alone would be read back as its idle primary).
func SetSessionStatus(t *testing.T, pool *pgxpool.Pool, sessionID domain.ID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET status = $2 WHERE id = $1`, sessionID, status); err != nil {
		t.Fatalf("fixture status: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE session_threads SET status = $2 WHERE session_id = $1 AND parent_thread_id IS NULL`,
		sessionID, status); err != nil {
		t.Fatalf("fixture thread status: %v", err)
	}
}

// NewChildThread inserts an idle child thread under the session's primary —
// the test seam plan 35 slice 3 runs the thread substrate through until slice
// 4's delegation spawns real ones — carrying a minimal agent snapshot (model
// "fixture-model", no tools) named "worker", and returns its id.
func NewChildThread(t *testing.T, pool *pgxpool.Pool, sessionID domain.ID) domain.ID {
	t.Helper()
	return NewChildThreadWithAgent(t, pool, sessionID, `{"type":"agent","id":"agent_worker","version":1,"name":"worker",`+
		`"model":{"id":"fixture-model"},"system":"","description":"","tools":[],"mcp_servers":[],"skills":[]}`)
}

// NewChildThreadWithAgent is NewChildThread with the given SessionThreadAgent
// snapshot (its name is the thread's agent_name).
func NewChildThreadWithAgent(t *testing.T, pool *pgxpool.Pool, sessionID domain.ID, agentJSON string) domain.ID {
	t.Helper()
	id := domain.NewID("sthr")
	// The child carries its session's scope, read off the session row: a
	// delegated thread is the same tenant's work as the session that spawned it.
	tag, err := pool.Exec(context.Background(),
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status,
		                              org_id, workspace_id, project_id)
		 SELECT $1, $2, $3, $4::jsonb, COALESCE($4::jsonb->>'name', 'worker'), 'idle',
		        s.org_id, s.workspace_id, s.project_id
		   FROM sessions s WHERE s.id = $2`,
		id, sessionID, domain.PrimaryThreadID(sessionID), agentJSON)
	if err != nil {
		t.Fatalf("fixture child thread: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("fixture child thread wrote %d rows (missing session?)", tag.RowsAffected())
	}
	return id
}
