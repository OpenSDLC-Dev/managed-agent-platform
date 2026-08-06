package executor

// The idle-TTL tier (plan 24 slice 5): an idle cloud session past the TTL is
// checkpointed and its sandbox reaped — unless it still owes work or an
// unanswered confirmation ask. These tests drive reapPass over the same
// harness the terminal tiers use; the capture/restore engine itself is
// checkpoint_test.go's.

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

var errBoom = errors.New("daemon boom")

// ttlHarness builds a harness whose session is idle, backdated past the
// one-hour TTL, with a one-file workspace export — the eligible steady state
// each test then perturbs.
func ttlHarness(t *testing.T, sb *fakeSandbox) *harness {
	t.Helper()
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{SandboxIdleTTL: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/notes.txt": "keep"}, nil),
	}
	setStatus(t, h, "idle")
	backdateSession(t, h, 2*time.Hour)
	return h
}

func backdateSession(t *testing.T, h *harness, age time.Duration) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET updated_at = now() - make_interval(secs => $2) WHERE id = $1`,
		h.sid.String(), age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// seedWorkItem inserts a work item for the harness session in the given state.
func seedWorkItem(t *testing.T, h *harness, id, state string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO work_items (id, environment_id, session_id, kind, state)
		 VALUES ($1, $2, $3, 'tool_exec', $4)`,
		id, h.envID.String(), h.sid.String(), state); err != nil {
		t.Fatal(err)
	}
}

// seedUnansweredAsk appends an ask-gated agent.tool_use with no confirmation —
// the HITL-idle shape (the brain suspends requires_action, the session shows
// idle, and the approval is human latency).
func seedUnansweredAsk(t *testing.T, h *harness, id string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO events (id, session_id, seq, type, payload)
		 VALUES ($1, $2, (SELECT COALESCE(max(seq), 0) + 1 FROM events WHERE session_id = $2),
		         'agent.tool_use', '{"evaluated_permission": "ask"}')`,
		id, h.sid.String()); err != nil {
		t.Fatal(err)
	}
}

// TestReapPassIdleSessionPastTTLCheckpointsAndReaps: the tier's happy path —
// the workspace lands in object storage with a ready marker, then the sandbox
// goes.
func TestReapPassIdleSessionPastTTLCheckpointsAndReaps(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("idle session past the TTL not reaped")
	}
	if state, _ := markerState(t, h); state != "ready" {
		t.Errorf("marker after idle reap = %q, want ready", state)
	}
	if got := checkpointMembers(t, h); got["workspace/notes.txt"] != "keep" {
		t.Errorf("checkpoint members = %v, want the workspace file", keys(got))
	}
}

// TestReapPassIdleTierLeavesIneligibleSessions: each exclusion on its own
// keeps the sandbox — freshness, owed work in every live state, an unanswered
// ask, a disabled tier (zero TTL), and a blob-less executor.
func TestReapPassIdleTierLeavesIneligibleSessions(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		h := ttlHarness(t, &fakeSandbox{})
		backdateSession(t, h, 0)
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Error("a fresh idle session was reaped")
		}
	})
	for _, state := range []string{"queued", "starting", "active"} {
		t.Run("work-"+state, func(t *testing.T) {
			h := ttlHarness(t, &fakeSandbox{})
			seedWorkItem(t, h, "work_"+state, state)
			if err := h.exec.reapPass(context.Background()); err != nil {
				t.Fatalf("reap pass: %v", err)
			}
			if len(h.prov.reapedSnapshot()) != 0 {
				t.Errorf("reaped with a %s work item owed", state)
			}
		})
	}
	t.Run("work-stopped-is-not-owed", func(t *testing.T) {
		h := ttlHarness(t, &fakeSandbox{})
		seedWorkItem(t, h, "work_stopped", "stopped")
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 1 {
			t.Error("a stopped work item blocked the reap")
		}
	})
	t.Run("unanswered-ask", func(t *testing.T) {
		h := ttlHarness(t, &fakeSandbox{})
		seedUnansweredAsk(t, h, "sevt_ask1")
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Error("reaped with an unanswered confirmation ask")
		}
	})
	t.Run("ttl-zero-disables", func(t *testing.T) {
		h := newHarness(t, &fakeSandbox{})
		h.prov.owned = []domain.ID{h.sid}
		setStatus(t, h, "idle")
		backdateSession(t, h, 100*24*time.Hour)
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Error("a zero TTL still reaped")
		}
	})
	t.Run("blob-less-disables", func(t *testing.T) {
		h := ttlHarness(t, &fakeSandbox{})
		bare := New(h.pool, h.log, h.queue, h.prov, nil, Config{SandboxIdleTTL: time.Hour})
		if err := bare.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Error("a blob-less executor ran the idle tier")
		}
	})
}

// TestReapPassIdleReapsOnceTheAskIsAnswered: the ask exclusion is about the
// outstanding question, not the event's existence — a confirmation referencing
// the tool use lifts it (here without the follow-on work item, which would
// block on its own).
func TestReapPassIdleReapsOnceTheAskIsAnswered(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	seedUnansweredAsk(t, h, "sevt_ask1")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO events (id, session_id, seq, type, payload)
		 VALUES ('sevt_conf1', $1, (SELECT max(seq) + 1 FROM events WHERE session_id = $1),
		         'user.tool_confirmation', '{"tool_use_id": "sevt_ask1"}')`,
		h.sid.String()); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 1 {
		t.Error("an answered ask still blocked the idle reap")
	}
}

// TestIdleReapDegradesToReapWithoutCheckpoint: D8 — a capture that cannot
// succeed (over budget, unreadable sandbox) must not pin the sandbox immortal.
// The reap proceeds, no marker and no blob are written, and the next provision
// starts fresh.
func TestIdleReapDegradesToReapWithoutCheckpoint(t *testing.T) {
	for name, rig := range map[string]func(*harness){
		"over-budget": func(h *harness) {}, // CheckpointMaxBytes set below
		"unreadable":  func(h *harness) { h.prov.exportErr = errBoom },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{SandboxIdleTTL: time.Hour}
			if name == "over-budget" {
				cfg.CheckpointMaxBytes = 8
			}
			h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, cfg)
			h.prov = h.exec.provider.(*fakeProvider)
			h.prov.owned = []domain.ID{h.sid}
			h.prov.exports = map[string][]byte{
				"/workspace": exportTar(t, map[string]string{"workspace/big.bin": "far more than eight bytes"}, nil),
			}
			setStatus(t, h, "idle")
			backdateSession(t, h, 2*time.Hour)
			rig(h)

			if err := h.exec.reapPass(context.Background()); err != nil {
				t.Fatalf("reap pass: %v", err)
			}
			if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
				t.Errorf("%s: the degraded reap did not happen", name)
			}
			if state, _ := markerState(t, h); state != "" {
				t.Errorf("%s: a failed capture left marker %q", name, state)
			}
			if hasCheckpoint(t, h) {
				t.Errorf("%s: a failed capture left a blob", name)
			}
		})
	}
}

// TestIdleReapRechecksUnderTheLock: a session revived between the candidate
// classification and the lock — a user.message flips it running — must be left
// alone; the under-lock re-read is the authority (plan 24 D4).
func TestIdleReapRechecksUnderTheLock(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	reapHookAfterClassify = func(sid domain.ID) {
		if sid == h.sid {
			setStatus(t, h, "running")
		}
	}
	defer func() { reapHookAfterClassify = nil }()
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Error("a session revived before the lock was reaped on the stale answer")
	}
}

// TestIdleReapThenProvisionRestores: the tier's whole point, end to end — the
// TTL reap's checkpoint is exactly what the next provision replays.
func TestIdleReapThenProvisionRestores(t *testing.T) {
	sb := &fakeSandbox{}
	h := ttlHarness(t, sb)
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}); err != nil {
		t.Fatalf("provision after idle reap: %v", err)
	}
	shipped, ok := sb.files[restoreTarPath]
	if !ok {
		t.Fatal("no restore tar shipped after an idle reap")
	}
	if !containsTarMember(t, shipped, "workspace/notes.txt", "keep") {
		t.Error("the restored tar is not the idle reap's checkpoint")
	}
	if state, _ := markerState(t, h); state != "consumed" {
		t.Errorf("marker after the resume = %q, want consumed", state)
	}
}

// TestReapDeletedTierAfterAnIdleReap: the idle tier's steady state feeds the
// delete path — a checkpointed-then-deleted session's blob is removed even
// though its sandbox is long gone, by the API delete (the marker row died in
// the deleting transaction; the reaper never sees a sandbox-less session).
// Here the sandbox is still owned, so the tier itself removes the blob.
func TestReapDeletedTierAfterAnIdleReap(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	setMarker(t, h, "consumed", checkpointBlob(t, h, map[string]string{"workspace/old.txt": "x"}))
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if hasCheckpoint(t, h) {
		t.Error("the deleted tier left the checkpoint blob")
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("the deleted session's sandbox was not reaped")
	}
}

// containsTarMember reports whether the tar bytes carry the named regular
// member with the given content.
func containsTarMember(t *testing.T, tarBytes, name, content string) bool {
	t.Helper()
	tr := tar.NewReader(strings.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatalf("walk tar: %v", err)
		}
		if hdr.Name == name {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return string(b) == content
		}
	}
}
