package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
)

// The worker's memory sync answers through an image whose BASH_ENV file
// prints around every exec, leaves the directory and traps EXIT (#860), on
// Docker and on Kubernetes: the store lands over the empty directory its tree
// listing reads as empty, a memory the agent writes is pushed, and the next
// run's sync, reading the marker and the baseline the first one wrote, pushes
// an edit against it.
func TestMemorySyncAnswersThroughAnImageHook(t *testing.T) {
	for _, b := range hookedtest.Backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			// The control plane is the harness's; the sandbox is the backend's.
			h := newHarness(t, &fakeSandbox{})
			t.Cleanup(func() {
				if sb, err := b.Provider.Attach(context.Background(), h.sid); err == nil {
					_ = sb.Destroy(context.Background())
				} else if !errors.Is(err, sandbox.ErrNotFound) {
					t.Errorf("attach the session's sandbox to destroy it: %v", err)
				}
			})
			h.seedMemoryStore(t, memStoreID, "Notes")
			h.seedMemory(t, memStoreID, "/facts/a.md", "alpha")
			h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
			cfg := ToolExecConfig{Image: b.Image, SessionsToken: h.sessionsToken(t)}
			run := func(use string) {
				t.Helper()
				h.suspend(t, use)
				if err := RunSessionTools(ctx, h.client, b.Provider, h.sid.String(), cfg); err != nil {
					t.Fatalf("RunSessionTools: %v", err)
				}
			}

			run(writeUse(memMount+"/log/b.md", "hello"))
			if got, ok := h.memoryContent(t, memStoreID, "/log/b.md"); !ok || got != "hello" {
				t.Errorf("pushed memory = %q, %v; want hello", got, ok)
			}
			sb, err := b.Provider.Attach(ctx, h.sid)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if got, err := sb.ReadFile(ctx, memMount+"/facts/a.md"); err != nil || string(got) != "alpha" {
				t.Errorf("landed memory = %q, %v; want alpha", got, err)
			}

			run(writeUse(memMount+"/facts/a.md", "edited"))
			if got, ok := h.memoryContent(t, memStoreID, "/facts/a.md"); !ok || got != "edited" {
				t.Errorf("edited memory = %q, %v; want edited", got, ok)
			}
		})
	}
}
