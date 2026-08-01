package worker

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The worker's sandboxes are capped the way the platform executor's are, which
// is one assignment (toolexec.go) and therefore one thing a refactor can drop
// in silence: nothing else in this package's suite would notice, because every
// other test provisions through a fake that ignores the Spec.
func TestWorkerHardeningReachesTheSandbox(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))

	uid := int64(10001)
	want := sandbox.Hardening{
		PidsLimit: 512, CPUMillis: 2000, MemoryBytes: 1 << 30,
		CapDrop: []string{"NET_RAW"}, ReadOnlyRootfs: true, RunAsUser: &uid,
	}
	if err := RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(),
		ToolExecConfig{Hardening: want}); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	if got := h.prov.lastSpec.Hardening; got.PidsLimit != want.PidsLimit ||
		got.CPUMillis != want.CPUMillis || got.MemoryBytes != want.MemoryBytes ||
		!got.ReadOnlyRootfs || got.RunAsUser == nil || *got.RunAsUser != uid ||
		len(got.CapDrop) != 1 || got.CapDrop[0] != "NET_RAW" {
		t.Errorf("provisioned Hardening = %+v, want %+v", got, want)
	}
}
