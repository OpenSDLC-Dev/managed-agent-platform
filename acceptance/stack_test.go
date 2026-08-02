package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// acceptanceKey is the management credential the rehearsal's SDK client sends
// as x-api-key. A throwaway for a database that dies with the test.
const acceptanceKey = "map-acceptance-local-key"

// sandboxImage is the rehearsal's sandbox base — the platform default, already
// pulled by every Docker suite in the gate, so the rehearsal adds no new pull.
const sandboxImage = "debian:stable-slim"

// scriptedModel plays a fixed sequence of provider streams, one per Generate
// call, in the order the brain makes them. The rehearsal's sequence is
// deterministic — agent tool turn, agent closing turn, grader verdict — because
// the whole pipeline between the calls (queue, executor, harvest) is serial
// for a single session.
type scriptedModel struct {
	mu      sync.Mutex
	scripts [][]provider.Chunk
	call    int
}

func (p *scriptedModel) Generate(_ context.Context, _ provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call >= len(p.scripts) {
		return nil, fmt.Errorf("scripted model: no script for call %d", p.call)
	}
	s := p.scripts[p.call]
	p.call++
	return &scriptedStream{chunks: s}, nil
}

type scriptedStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *scriptedStream) Next() bool {
	if s.i >= len(s.chunks) {
		return false
	}
	s.i++
	return true
}
func (s *scriptedStream) Chunk() provider.Chunk { return s.chunks[s.i-1] }
func (s *scriptedStream) Err() error            { return nil }
func (s *scriptedStream) Close() error          { return nil }

// stack is the platform in one process, mirroring evals' construction (which
// mirrors cmd/*/main.go): real control plane over real Postgres, real executor
// over real Docker sandboxes, and a brain whose provider registry routes every
// model string to the scripted model above. Only main()'s glue is absent.
type stack struct {
	url string

	mu       sync.Mutex
	sessions []string
}

// track records a session for container reaping at teardown. The SDK creates
// sessions, so the harness reports them here rather than the stack minting
// them itself.
func (s *stack) track(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, sessionID)
}

func (s *stack) reapAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sessions {
		// Idempotent: rm --force on a never-created or already-gone container
		// is a no-op. The name is the docker provider's convention.
		_ = exec.Command("docker", "rm", "--force", "--volumes", "map-"+id).Run()
	}
}

func newStack(t *testing.T, scripts [][]provider.Chunk) *stack {
	t.Helper()
	ctx := context.Background()

	pool := pgtest.NewPool(t)
	if err := api.EnsureAPIKey(ctx, pool, "acceptance", acceptanceKey); err != nil {
		t.Fatalf("seed the management key: %v", err)
	}

	// One in-memory blob store shared by the API (file uploads, downloads),
	// the brain (rubric snapshots) and the executor (deliverable harvest) —
	// the same sharing the compose stack gets from MinIO.
	blobs := blobtest.Mem()
	srv := httptest.NewServer(api.NewHandler(pool, blobs, nil))
	t.Cleanup(srv.Close)

	scripted := &scriptedModel{scripts: scripts}
	registry, err := provider.NewRegistry(
		[]provider.Route{{
			Model: "*",
			Config: provider.Config{
				Protocol: "anthropic",
				Model:    "acceptance-scripted",
				BaseURL:  "http://scripted.invalid",
				APIKey:   "unused",
			},
		}},
		map[string]provider.Factory{
			"anthropic": func(provider.Config) (provider.Provider, error) { return scripted, nil },
		},
	)
	if err != nil {
		t.Fatalf("build the provider registry: %v", err)
	}

	sbx, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("the rehearsal requires Docker: %v", err)
	}

	s := &stack{url: srv.URL}
	// Registered before the loop-stop cleanup so LIFO runs it after the loops
	// have stopped — when no executor can re-provision behind the reap.
	t.Cleanup(s.reapAll)

	loopCtx, stop := context.WithCancel(ctx)
	brainDone := runLoop(func() error {
		return brain.New(pool, registry, blobs, brain.Config{
			LeaseTTL:     2 * time.Minute,
			PollInterval: 100 * time.Millisecond,
		}).Run(loopCtx)
	})
	execDone := runLoop(func() error {
		return executor.New(pool, events.NewLog(pool), queue.New(pool), sbx, blobs, executor.Config{
			Image:        sandboxImage,
			LeaseTTL:     5 * time.Minute,
			PollInterval: 100 * time.Millisecond,
		}).Run(loopCtx)
	})
	t.Cleanup(func() {
		stop()
		waitLoop(t, "brain", brainDone)
		waitLoop(t, "executor", execDone)
	})

	return s
}

func runLoop(run func() error) chan error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	return done
}

// waitLoop accepts both spellings of a clean shutdown — brain.Run returns
// ctx.Err(), executor.Run returns nil — and reports anything else.
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
