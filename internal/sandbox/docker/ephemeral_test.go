package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// Docker's writable-layer quota needs a specific storage driver rather than
// being something every daemon has, so this backend ignores an
// EphemeralStorageBytes rather than inventing an approximation — the mirror
// image of Kubernetes ignoring PidsLimit. This row is what keeps it deliberate:
// the create payload for a disk cap must be the payload for no disk cap.
func TestSandboxConfigIgnoresEphemeralStorage(t *testing.T) {
	plain := sandboxConfig(hardenedSpec(sandbox.Hardening{}), "/workspace", "", 0)
	disk := sandboxConfig(hardenedSpec(sandbox.Hardening{EphemeralStorageBytes: 2 << 30}),
		"/workspace", "", 0)
	// The whole payload, not a chosen field: the failure this guards against is
	// the cap leaking into *some* other limit, and naming the fields in advance
	// would be guessing which one.
	want, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal the unhardened payload: %v", err)
	}
	got, err := json.Marshal(disk)
	if err != nil {
		t.Fatalf("marshal the disk-capped payload: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("a disk cap changed the create payload:\n want %s\n  got %s", want, got)
	}
}

// Ignoring it silently is the failure the Kubernetes side already refuses for
// PidsLimit: an operator who capped the sandbox's disk and got nothing should
// be told, once per provider rather than once per sandbox.
func TestWarnsOnceForAnUnenforceableDiskCap(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	p := &Provider{}
	ctx := context.Background()
	h := sandbox.Hardening{EphemeralStorageBytes: 2 << 30}
	p.warnUnenforceableEphemeralStorage(ctx, h)
	p.warnUnenforceableEphemeralStorage(ctx, h)

	if n := strings.Count(buf.String(), "ephemeral storage"); n != 1 {
		t.Errorf("logged the unenforceable-disk-cap warning %d times, want exactly 1:\n%s", n, buf.String())
	}
}

// And says nothing when nothing was asked for: a warning on every unconfigured
// deployment is noise that trains operators to ignore the one that matters.
func TestNoWarningWithoutADiskCap(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	(&Provider{}).warnUnenforceableEphemeralStorage(context.Background(), sandbox.Hardening{})
	if buf.Len() != 0 {
		t.Errorf("warned without a configured disk cap: %s", buf.String())
	}
}
