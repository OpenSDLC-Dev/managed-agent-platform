package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
)

// The executor's own scripts answer through an image whose BASH_ENV file
// prints around every exec, leaves the directory and traps EXIT (#860), on
// Docker and on Kubernetes: the memory store lands (its tree listing of an
// empty directory read as empty, not as the banner) and syncs both ways
// across two runs (the listing parsed, the marker and baseline read), the
// outputs harvest publishes exactly the deliverables, and the packages probe
// reads a read-only root as one rather than as an answer it does not know.
func TestPlatformScriptsAnswerThroughAnImageHook(t *testing.T) {
	for _, b := range hookedtest.Backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			ro, _ := b.Provision(t, sandbox.Hardening{ReadOnlyRootfs: true})
			if reason, err := probeSandboxForPackages(ctx, ro, time.Minute); err != nil || reason != packageReasonReadOnly {
				t.Errorf("packages probe = %q, %v; want %s", reason, err, packageReasonReadOnly)
			}

			h := newHarnessWith(t, b.Provider, Config{Image: b.Image})
			t.Cleanup(func() {
				if sb, err := b.Provider.Attach(context.Background(), h.sid); err == nil {
					_ = sb.Destroy(context.Background())
				} else if !errors.Is(err, sandbox.ErrNotFound) {
					t.Errorf("attach the session's sandbox to destroy it: %v", err)
				}
			})
			h.seedMemoryStore(t, memStoreID, "Notes")
			h.seedMemory(t, memStoreID, "/facts/a.md", "alpha")
			h.refMemory(t, memStoreID, memMount, "read_write")
			bash := func(command string) string {
				use, _ := json.Marshal(map[string]any{"name": "bash", "input": map[string]string{"command": command}})
				return string(use)
			}

			h.suspend(t, bash("mkdir -p /mnt/session/outputs/sub "+memMount+"/log && printf a > /mnt/session/outputs/a.txt"+
				" && printf b > /mnt/session/outputs/sub/b.txt && printf hello > "+memMount+"/log/b.md"))
			h.stepOnce(t)
			sb, err := b.Provider.Attach(ctx, h.sid)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if got, err := sb.ReadFile(ctx, memMount+"/facts/a.md"); err != nil || string(got) != "alpha" {
				t.Errorf("landed memory = %q, %v; want alpha", got, err)
			}
			if got, ok := h.memoryContent(t, memStoreID, "/log/b.md"); !ok || got != "hello" {
				t.Errorf("pushed memory = %q, %v; want hello", got, ok)
			}
			raw, err := sb.ReadFile(ctx, baselinePath(memStoreID))
			if err != nil {
				t.Fatalf("read the baseline: %v", err)
			}
			if base, err := memsync.DecodeBaseline(raw); err != nil || len(base.Synced) != 2 {
				t.Errorf("baseline = %+v, %v; want the two memories synced", base, err)
			}

			// The second run's sync reads the baseline the first wrote, and
			// pushes an edit against it.
			h.suspend(t, bash("printf edited > "+memMount+"/facts/a.md"))
			h.stepOnce(t)
			if got, ok := h.memoryContent(t, memStoreID, "/facts/a.md"); !ok || got != "edited" {
				t.Errorf("edited memory = %q, %v; want edited", got, ok)
			}

			h.seedOutcome(t, domain.OutcomeResultEvaluating)
			h.enqueueHarvest(t)
			h.stepOnce(t)
			rows := h.fileRows(t)
			if len(rows) != 2 || rows[0].filename != "a.txt" || rows[1].filename != "sub/b.txt" {
				t.Errorf("harvested %+v; want a.txt and sub/b.txt", rows)
			}
		})
	}
}
