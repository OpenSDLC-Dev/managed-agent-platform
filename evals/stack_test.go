package evals

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// evalKey is the management credential the harness sends as x-api-key. It is a
// throwaway for a database that lives and dies with the test binary.
const evalKey = "map-evals-local-key"

// stack is the whole platform in one process: the real control plane over a real
// Postgres, a real brain against a real model endpoint, and a real executor
// driving real Docker sandboxes. Only main()'s glue is absent — env parsing, a
// TCP listen, telemetry init, signal handling — and CI's compose job smokes
// that. Everything a trial touches below this line is the code that ships.
type stack struct {
	url  string
	pool *pgxpool.Pool
	sbx  *docker.Provider
	// model is MODEL_ID, the string every agent is created with. See agentBody.
	model string

	// sessions is every session id created through this stack, reaped as a set
	// at teardown once the loops are stopped. Guarded because trials are serial
	// today but nothing requires it, and a race found by a future -parallel
	// would be a maddening way to learn it.
	mu       sync.Mutex
	sessions []string
}

// newStack builds one, mirroring cmd/*/main.go's construction order, and tears
// it down with the test.
func newStack(t *testing.T, cfg modeltest.Config) *stack {
	t.Helper()
	ctx := context.Background()

	// A fresh migrated database per call: NewPool creates the database and
	// store.Open migrates it on connect, so there is no separate migrate step
	// and no state carried between trials.
	pool := pgtest.NewPool(t)

	if err := api.EnsureAPIKey(ctx, pool, "evals", evalKey); err != nil {
		t.Fatalf("seed the management key: %v", err)
	}

	// NewHandler builds its own event log, broker and queue over the pool, so
	// the harness cannot accidentally hand it a different one than the loops
	// use. No object store: the eval stack exercises no skills yet.
	// One in-memory blob store shared by the API (skill uploads) and the
	// executor (materialization), so eval tasks can exercise skills end to end.
	blobs := blobtest.Mem()
	srv := httptest.NewServer(api.NewHandler(pool, blobs))
	t.Cleanup(srv.Close)

	// One default route. Config.Model is the id the *endpoint* receives, so it
	// must be MODEL_ID: left empty, the agent's own model string would be
	// passed upstream verbatim and a gateway would reject a name it has never
	// heard of. Both factories are registered because NewRegistry hard-errors
	// on a protocol with no factory, and .env decides which one is in play.
	registry, err := provider.NewRegistry(
		[]provider.Route{{
			Model: "*",
			Config: provider.Config{
				Protocol: cfg.Protocol,
				Model:    cfg.Model,
				BaseURL:  cfg.BaseURL,
				APIKey:   cfg.APIKey,
			},
		}},
		map[string]provider.Factory{"anthropic": anthropic.New, "openai": openai.New},
	)
	if err != nil {
		t.Fatalf("build the provider registry: %v", err)
	}

	// docker.New does not dial the daemon, so a missing Docker surfaces inside
	// the first trial rather than here.
	sbx, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("evals require Docker: %v", err)
	}

	s := &stack{url: srv.URL, pool: pool, sbx: sbx, model: cfg.Model}

	// Reap every session's container. Registered BEFORE the loop-stop cleanup so
	// that LIFO runs it AFTER the loops have stopped — the moment when no
	// executor can re-provision a container behind the reap (see runTrial).
	// Idempotent: docker rm on an already-gone or never-created container is a
	// no-op, so trials that never provisioned cost nothing here.
	t.Cleanup(s.reapAll)

	loopCtx, stop := context.WithCancel(ctx)
	brainDone := runLoop(func() error {
		return brain.New(pool, registry, brain.Config{
			LeaseTTL:     2 * time.Minute,
			PollInterval: 100 * time.Millisecond,
		}).Run(loopCtx)
	})
	execDone := runLoop(func() error {
		return executor.New(pool, events.NewLog(pool), queue.New(pool), sbx, blobs, executor.Config{
			Image: evalImage,
			// Workdir left empty, which resolves to sandbox.DefaultWorkdir on
			// both this executor and the file tools it runs, so a relative path
			// resolves against the directory bash runs in.
			LeaseTTL:     5 * time.Minute,
			PollInterval: 100 * time.Millisecond,
		}).Run(loopCtx)
	})
	// Stop the loops before httptest's server closes, so neither is mid-request
	// against a server that has gone away.
	t.Cleanup(func() {
		stop()
		waitLoop(t, "brain", brainDone)
		waitLoop(t, "executor", execDone)
	})

	return s
}

// reapAll removes the container of every session this stack created. It runs at
// teardown, after the loops are stopped, so a re-provision cannot race it.
func (s *stack) reapAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sessions {
		reap(id)
	}
}

func runLoop(run func() error) chan error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	return done
}

// waitLoop reports a loop that died of anything other than the shutdown we
// asked for. The two loops disagree about how to say "you cancelled me" —
// brain.Run returns ctx.Err(), executor.Run returns nil — so both are accepted
// and anything else is a real failure worth seeing.
func waitLoop(t *testing.T, name string, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("%s loop exited with %v, want a clean shutdown", name, err)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("%s loop did not stop within 30s of cancellation", name)
	}
}
