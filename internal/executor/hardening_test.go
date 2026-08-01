package executor

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The executor's configured containment must reach the Spec it provisions with:
// that one assignment is what makes "hardened by default" true for every
// platform-managed session, and nothing else in this suite would notice it
// going missing — the sandbox contract rows pass a Hardening straight to
// Provision and never travel through here.
func TestExecutorHardeningReachesTheSandbox(t *testing.T) {
	uid := int64(10001)
	want := sandbox.Hardening{
		PidsLimit: 512, CPUMillis: 2000, MemoryBytes: 1 << 30,
		CapDrop: []string{"NET_RAW"}, ReadOnlyRootfs: true, RunAsUser: &uid,
	}
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{Hardening: want})
	h.prov = prov

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}

	if got := h.prov.lastSpec.Hardening; got.PidsLimit != want.PidsLimit ||
		got.CPUMillis != want.CPUMillis || got.MemoryBytes != want.MemoryBytes ||
		!got.ReadOnlyRootfs || got.RunAsUser == nil || *got.RunAsUser != uid ||
		len(got.CapDrop) != 1 || got.CapDrop[0] != "NET_RAW" {
		t.Errorf("provisioned Hardening = %+v, want %+v", got, want)
	}
}
