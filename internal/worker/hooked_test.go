package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
)

// The worker's memory sync and file mounts' probe answer through an image
// whose BASH_ENV file prints around every exec, leaves the directory and
// traps EXIT (#860), on Docker and on Kubernetes: the store lands over the
// empty directory its tree listing reads as empty, a memory the agent writes
// is pushed, and the next run's sync, reading the marker and the baseline the
// first one wrote, pushes an edit against it; that run's probe lists the one
// mount the agent moved away, and that one alone lands again.
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
			mountA, mountB := "/mnt/session/uploads/a.csv", "/mnt/session/uploads/b.csv"
			idA, idB := domain.NewID("file").String(), domain.NewID("file").String()
			h.seedFile(t, idA, "a.csv", "text/csv", "a's bytes")
			h.seedFile(t, idB, "b.csv", "text/csv", "b's bytes")
			// Beside the store, not in place of it, as refFileMounts would
			// write resources[].
			mounts, _ := json.Marshal([]map[string]string{
				{"type": "file", "file_id": idA, "mount_path": mountA},
				{"type": "file", "file_id": idB, "mount_path": mountB},
			})
			if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resources = resources || $2::jsonb WHERE id = $1`, h.sid.String(), mounts); err != nil {
				t.Fatalf("add the file mounts: %v", err)
			}
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

			// The agent moves one mount away and edits the other; the next
			// run's probe lists the one moved away, and that one alone lands
			// again.
			if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "mv " + mountA + " /tmp/a.csv && printf edited > " + mountB}); err != nil || res.ExitCode != 0 {
				t.Fatalf("move a mount away and edit the other: %+v, %v", res, err)
			}
			run(writeUse(memMount+"/facts/a.md", "edited"))
			if got, ok := h.memoryContent(t, memStoreID, "/facts/a.md"); !ok || got != "edited" {
				t.Errorf("edited memory = %q, %v; want edited", got, ok)
			}
			for mount, want := range map[string]string{mountA: "a's bytes", mountB: "edited"} {
				if got, err := sb.ReadFile(ctx, mount); err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", mount, got, err, want)
				}
			}
		})
	}
}
