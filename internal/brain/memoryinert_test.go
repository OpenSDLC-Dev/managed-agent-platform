package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// TestMemoryElementInertBeforeSync pins plan 36 slice 3's promise: a memory
// store attached as an id-less resources[] element is invisible to the model
// until slice 4 materializes it — nothing is mounted yet, so naming a mount
// would be a false statement (the repositories block's own rule). The element
// must also pass through the file and repository decoders that share the
// array without tripping either. Slice 4 replaces this test with the block's.
func TestMemoryElementInertBeforeSync(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	const memoryResources = `[{"type":"memory_store","memory_store_id":"memstore_a0000000000000000000000",` +
		`"access":"read_write","instructions":"consult before answering","name":"Notes",` +
		`"description":"the user's notes","mount_path":"/mnt/memory/notes"}]`
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = $1, resolved_agent = $2 WHERE id = $3`,
		memoryResources, repoResolvedAgent, h.sessionID.String()); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) == 0 {
		t.Fatal("the provider was never called")
	}
	sys := h.provider.calls[0].System
	for _, leaked := range []string{"/mnt/memory", "Notes", "consult before answering", "memstore_"} {
		if strings.Contains(sys, leaked) {
			t.Errorf("the system prompt names the inert attachment (%q):\n%s", leaked, sys)
		}
	}
	if !strings.HasPrefix(sys, "base prompt") {
		t.Errorf("the agent's own prompt did not lead:\n%s", sys)
	}
}
