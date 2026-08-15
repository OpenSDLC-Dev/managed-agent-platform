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

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
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

// seedStoppedClaim seeds a work item an interrupt just cancelled: state
// stopped with stopped_at set, age old. The row is dead, but until the
// executor's lease TTL has passed its physical claimant may still be running
// the tool in the sandbox — it only notices the cancellation at its next
// lease renewal.
func seedStoppedClaim(t *testing.T, h *harness, id string, age time.Duration) {
	t.Helper()
	seedWorkItem(t, h, id, "stopped")
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE work_items SET stopped_at = now() - make_interval(secs => $2) WHERE id = $1`,
		id, age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// seedUnansweredAsk appends an ask-gated tool use of typ with no confirmation —
// the HITL-idle shape (the brain suspends requires_action, the session shows
// idle, and the approval is human latency). typ is a parameter because both
// platform-executed families are gated, and a session waiting on either is
// waiting on a human.
func seedUnansweredAsk(t *testing.T, h *harness, id string, typ domain.EventType) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO events (id, session_id, seq, type, payload)
		 VALUES ($1, $2, (SELECT COALESCE(max(seq), 0) + 1 FROM events WHERE session_id = $2),
		         $3, '{"evaluated_permission": "ask"}')`,
		id, h.sid.String(), string(typ)); err != nil {
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
	t.Run("freshly-stopped-claim", func(t *testing.T) {
		// An interrupt cancels the row immediately, but the physical claimant
		// only notices at its next lease renewal — until the executor's lease
		// TTL has passed, the tool may still be running in the sandbox.
		h := ttlHarness(t, &fakeSandbox{})
		seedStoppedClaim(t, h, "work_fresh_stop", 0)
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass: %v", err)
		}
		if len(h.prov.reapedSnapshot()) != 0 {
			t.Error("reaped under a freshly-stopped claim whose tool may still be running")
		}
	})
	// Either gated family pins the sandbox. The MCP arm needs no sandbox to run
	// its own call, but the session it belongs to is mid-turn and its built-in
	// tools do — reaping under a question no human has answered would destroy
	// the workspace the approved turn resumes into.
	for _, typ := range []domain.EventType{domain.EventAgentToolUse, domain.EventAgentMCPToolUse} {
		t.Run("unanswered-ask-"+string(typ), func(t *testing.T) {
			h := ttlHarness(t, &fakeSandbox{})
			seedUnansweredAsk(t, h, "sevt_ask1", typ)
			if err := h.exec.reapPass(context.Background()); err != nil {
				t.Fatalf("reap pass: %v", err)
			}
			if len(h.prov.reapedSnapshot()) != 0 {
				t.Errorf("reaped with an unanswered %s confirmation ask", typ)
			}
		})
	}
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
		bare := New(h.pool, h.log, h.queue, h.prov, nil, h.cipher, Config{SandboxIdleTTL: time.Hour})
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
	seedUnansweredAsk(t, h, "sevt_ask1", domain.EventAgentToolUse)
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
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err != nil {
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

// TestReapPassIdleReapsPastAStaleStoppedClaim: the stopped-claim exclusion is
// a lease-length grace, not a permanent hold — once the executor's lease TTL
// has passed, every claimant has noticed the cancellation and the reap runs.
func TestReapPassIdleReapsPastAStaleStoppedClaim(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	seedStoppedClaim(t, h, "work_stale_stop", 2*h.exec.cfg.LeaseTTL)
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 1 {
		t.Error("a claim stopped a full lease ago still blocked the reap")
	}
}

// TestReapPassIdleIgnoresOtherSessionsWork: the owed-work exclusion is scoped
// to the candidate — another session's live work item is no reason to keep
// this one's sandbox.
func TestReapPassIdleIgnoresOtherSessionsWork(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	otherSid, otherEnv := pgtest.NewSession(t, h.pool, "cloud")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO work_items (id, environment_id, session_id, kind, state)
		 VALUES ('work_other', $1, $2, 'tool_exec', 'active')`,
		otherEnv.String(), otherSid.String()); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("another session's live work item blocked this session's reap")
	}
}

// failPutStore fails every upload — the shape of an object store outage.
type failPutStore struct{ blob.Store }

func (failPutStore) Put(context.Context, string, io.Reader, int64, string) error {
	return errBoom
}

// TestIdleReapAbortsWhenTheStoreFails: a failure outside the sandbox — here
// the object store refusing the upload — must NOT degrade to a destructive
// reap: the pass reports the error, the sandbox stays owned, and the next
// interval retries with the workspace intact (the deleted tier's own rule).
func TestIdleReapAbortsWhenTheStoreFails(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	bare := New(h.pool, h.log, h.queue, h.prov, failPutStore{h.blobs}, h.cipher, Config{SandboxIdleTTL: time.Hour})
	err := bare.reapPass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "upload checkpoint") {
		t.Fatalf("reap pass error = %v, want the upload failure surfaced", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Error("a store outage degraded to a destructive reap")
	}
	if state, _ := markerState(t, h); state != "" {
		t.Errorf("an aborted capture left marker %q", state)
	}
}

// TestCaptureWritesNoMarkerForADeletedSession: a DELETE landing between the
// reaper's under-lock re-classification and the marker write must not be
// resurrected — the guarded insert sees the session row gone, withdraws the
// just-uploaded blob, and reports the race as its own outcome.
func TestCaptureWritesNoMarkerForADeletedSession(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	deleteSessionRow(t, h)
	err := h.exec.captureCheckpoint(context.Background(), h.sid)
	if !errors.Is(err, errCaptureSessionDeleted) {
		t.Fatalf("capture after delete = %v, want errCaptureSessionDeleted", err)
	}
	if state, _ := markerState(t, h); state != "" {
		t.Errorf("a capture racing a delete left marker %q", state)
	}
	if hasCheckpoint(t, h) {
		t.Error("a capture racing a delete left its blob behind")
	}
}

// failDeleteStore fails every Delete — the withdraw outage's shape.
type failDeleteStore struct{ blob.Store }

func (failDeleteStore) Delete(context.Context, string) error { return errBoom }

// TestCaptureDeleteRaceWithFailedWithdrawAborts: when the concurrent-DELETE
// race is detected but the blob withdraw fails, the capture must NOT report
// the benign sentinel — aborting keeps the sandbox owned, and the next
// pass's deleted tier (the tombstone is already written) retries the blob
// delete before reaping. The sentinel would reap now and orphan the blob
// forever.
func TestCaptureDeleteRaceWithFailedWithdrawAborts(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	bare := New(h.pool, h.log, h.queue, h.prov, failDeleteStore{h.blobs}, h.cipher, Config{SandboxIdleTTL: time.Hour})
	deleteSessionRow(t, h)
	err := bare.captureCheckpoint(context.Background(), h.sid)
	if err == nil || errors.Is(err, errCaptureSessionDeleted) {
		t.Fatalf("capture with a failed withdraw = %v, want a non-sentinel abort", err)
	}
	if state, _ := markerState(t, h); state != "" {
		t.Errorf("a capture racing a delete left marker %q", state)
	}
}

// recordingQuerier records each statement's SQL in call order, delegating to
// the pool — the seam that pins classifyForReap's read ordering.
type recordingQuerier struct {
	inner events.Querier
	sqls  []string
}

func (r *recordingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.sqls = append(r.sqls, sql)
	return r.inner.QueryRow(ctx, sql, args...)
}

func (r *recordingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sqls = append(r.sqls, sql)
	return r.inner.Query(ctx, sql, args...)
}

// TestClassifyReadsAsksBeforeMainQuery: the ask exclusion is deliberately the
// EARLIER read — a confirmation batch answers the ask, enqueues the work item,
// and flips the session running in one transaction, so asks-first means that
// transaction lands either before both reads (the main query sees running or
// the live item) or after the ask read (which still saw the ask). Main-first
// would admit the double-permissive interleaving. The commit-timing race
// itself is not constructible in a test; the ordering that closes it is.
func TestClassifyReadsAsksBeforeMainQuery(t *testing.T) {
	h := ttlHarness(t, &fakeSandbox{})
	rq := &recordingQuerier{inner: h.pool}
	tier, err := h.exec.classifyForReap(context.Background(), rq, h.sid)
	if err != nil || tier != tierIdle {
		t.Fatalf("classify = %v, %v; want tierIdle", tier, err)
	}
	ask, main := -1, -1
	for i, sql := range rq.sqls {
		if strings.Contains(sql, "evaluated_permission") {
			ask = i
		}
		if strings.Contains(sql, "FROM sessions s JOIN environments") {
			main = i
		}
	}
	if ask < 0 || main < 0 {
		t.Fatalf("expected both reads recorded, got %d statements", len(rq.sqls))
	}
	if ask > main {
		t.Errorf("the ask read ran at %d, after the main query at %d", ask, main)
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
