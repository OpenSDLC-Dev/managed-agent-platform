package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// Plan 36 slice 4's brain rows (decision 9): the "Memory stores" block a cloud
// session's agent sees — every attached store with its mount, name, access,
// description and instructions; an archived store rendered read-only; a
// deleted store hedged rather than dropped — placed after the repositories
// block, and absent on self_hosted, where nothing mounts a store yet. This
// replaces slice 3's inert-attachment test.

const memoryResources = `[{"id":"sesrsc_w","type":"github_repository",` +
	`"url":"https://github.com/example-org/widget","mount_path":"/workspace/widget","checkout":null,` +
	`"created_at":"2026-08-07T00:00:00Z","updated_at":"2026-08-07T00:00:00Z"},` +
	`{"type":"memory_store","memory_store_id":"memstore_a0000000000000000000000",` +
	`"access":"read_write","instructions":"consult before answering","name":"Notes",` +
	`"description":"the user's notes","mount_path":"/mnt/memory/notes"},` +
	`{"type":"memory_store","memory_store_id":"memstore_b0000000000000000000000",` +
	`"access":"read_only","instructions":null,"name":"Archive",` +
	`"description":"","mount_path":"/mnt/memory/archive"},` +
	`{"type":"memory_store","memory_store_id":"memstore_c0000000000000000000000",` +
	`"access":"read_write","instructions":null,"name":"Frozen",` +
	`"description":"archived since","mount_path":"/mnt/memory/frozen"},` +
	`{"type":"memory_store","memory_store_id":"memstore_d0000000000000000000000",` +
	`"access":"read_write","instructions":null,"name":"Gone",` +
	`"description":"","mount_path":"/mnt/memory/gone"}]`

// seedMemory plants the three stores that still exist (one archived), points
// the session at the resources above, drives one turn and returns the system
// prompt.
func seedMemory(t *testing.T, h *harness) string {
	t.Helper()
	ctx := context.Background()
	for _, row := range [][2]string{{"memstore_a0000000000000000000000", "Notes"}, {"memstore_b0000000000000000000000", "Archive"}} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO memory_stores (id, name) VALUES ($1, $2)`, row[0], row[1]); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO memory_stores (id, name, archived_at) VALUES ('memstore_c0000000000000000000000', 'Frozen', now())`); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET resources = $1, resolved_agent = $2 WHERE id = $3`,
		memoryResources, repoResolvedAgent, h.sessionID.String()); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) == 0 {
		t.Fatal("the provider was never called")
	}
	return h.provider.calls[0].System
}

func TestMemoryBlockInjected(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	sys := seedMemory(t, h)

	if !strings.Contains(sys, "Memory stores.") {
		t.Fatalf("system prompt carries no memory block:\n%s", sys)
	}
	for _, want := range []string{
		"/mnt/memory/notes — Notes (read_write): the user's notes — Instructions: consult before answering",
		"/mnt/memory/archive — Archive (read_only)",
		"/mnt/memory/frozen — Frozen (read_only, archived): archived since",
		"/mnt/memory/gone — Gone (read_write) — NOT AVAILABLE: the memory store no longer exists",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sys)
		}
	}
	// The block says what the directory cannot: the files persist through the
	// store, and a read-only store takes no writes.
	for _, want := range []string{"syncs the directory with the store", "read-only store cannot be changed"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sys)
		}
	}
	// The agent's own prompt leads, the repositories block precedes.
	if !strings.HasPrefix(sys, "base prompt") {
		t.Errorf("the block displaced the agent's system prompt:\n%s", sys)
	}
	if repos, mem := strings.Index(sys, "Mounted repositories"), strings.Index(sys, "Memory stores."); repos < 0 || mem < repos {
		t.Errorf("the memory block (%d) does not follow the repositories block (%d):\n%s", mem, repos, sys)
	}
	// The line for a store with nothing optional carries no dangling separators.
	if line := memoryLine(t, sys, "/mnt/memory/archive"); line != "- /mnt/memory/archive — Archive (read_only)" {
		t.Errorf("archive line = %q", line)
	}
}

// TestMemoryBlockSkippedOnSelfHosted: nothing materializes a store on
// self_hosted until slice 6, so the block is absent even though the elements
// are present — the repositories block's own rule.
func TestMemoryBlockSkippedOnSelfHosted(t *testing.T) {
	h := newHarnessEnv(t, "self_hosted", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	sys := seedMemory(t, h)
	for _, leaked := range []string{"Memory stores", "/mnt/memory", "consult before answering"} {
		if strings.Contains(sys, leaked) {
			t.Errorf("a self_hosted session's prompt names a memory store (%q):\n%s", leaked, sys)
		}
	}
}

// memoryLine returns the block line naming the mount.
func memoryLine(t *testing.T, sys, mount string) string {
	t.Helper()
	for _, line := range strings.Split(sys, "\n") {
		if strings.HasPrefix(line, "- "+mount+" ") {
			return line
		}
	}
	t.Fatalf("no line for %s:\n%s", mount, sys)
	return ""
}
