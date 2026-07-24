package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// fakeSandbox is an in-memory sandbox. The executor tests drive read/write
// tools, which use the file primitives directly (no shell template), so a
// minimal store is enough; bash is covered by the real-container test below.
type fakeSandbox struct {
	files    map[string]string
	writeErr error
	readErr  error
	// failPath, if set, makes WriteFile fail (a backend fault) for a path with
	// this suffix, so a test can fault one tool of a parallel set while the
	// others succeed.
	failPath string
	// entered (if set) receives one signal the first time WriteFile is entered,
	// and gate (if set) blocks WriteFile until closed — together they let a test
	// hold a tool mid-run to observe the lease keeper renew.
	entered chan struct{}
	gate    chan struct{}
}

func (f *fakeSandbox) ID() string { return "fake" }
func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	// Reflect real file presence for the executor's mountsPresent probe
	// (`test -e '<p1>' && test -e '<p2>' && true`), so a deleted mount actually
	// reports absent and forces re-materialization — an always-true Exec would
	// make the `&& mountsPresent` skip-guard untestable. The exact shape match
	// keeps ordinary tool commands on the unconditional exit-0 path.
	if strings.HasPrefix(req.Command, "test -e ") && strings.HasSuffix(req.Command, "&& true") {
		for _, tok := range strings.Split(req.Command, " && ") {
			if tok == "true" {
				continue
			}
			p := strings.TrimPrefix(tok, "test -e ")
			p = strings.TrimSuffix(strings.TrimPrefix(p, "'"), "'")
			p = strings.ReplaceAll(p, `'\''`, "'") // reverse shellQuote
			if _, ok := f.files[p]; !ok {
				return sandbox.ExecResult{ExitCode: 1}, nil
			}
		}
	}
	return sandbox.ExecResult{}, nil
}
func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[path]
	if !ok {
		return nil, sandbox.ErrFileNotExist
	}
	return []byte(data), nil
}
func (f *fakeSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.failPath != "" && strings.HasSuffix(path, f.failPath) {
		return fmt.Errorf("backend fault writing %s", path)
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}
func (f *fakeSandbox) WriteFileStream(ctx context.Context, path string, src io.Reader, size int64) error {
	data, err := io.ReadAll(io.LimitReader(src, size))
	if err != nil {
		return err
	}
	return f.WriteFile(ctx, path, data)
}
func (f *fakeSandbox) Destroy(context.Context) error { return nil }

type fakeProvider struct {
	sb           *fakeSandbox
	provisionErr error
	provisions   int
	lastSpec     sandbox.Spec // captured for env-injection assertions
	// entered/gate mirror fakeSandbox's, for a test that holds provisioning open
	// (a slow image pull) to observe the lease keeper renew across it.
	entered chan struct{}
	gate    chan struct{}
}

func (p *fakeProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	p.provisions++
	p.lastSpec = spec
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.provisionErr != nil {
		return nil, p.provisionErr
	}
	return p.sb, nil
}

type harness struct {
	pool  *pgxpool.Pool
	log   *events.Log
	queue *queue.Queue
	exec  *Executor
	prov  *fakeProvider
	blobs *blobtest.MemStore
	sid   domain.ID
	envID domain.ID
}

// newHarness builds an executor over a fresh Dockerized Postgres and a fake
// sandbox, with a session already flipped to running (as the brain leaves it
// when a turn suspends for a tool).
func newHarness(t *testing.T, sb *fakeSandbox) *harness {
	t.Helper()
	prov := &fakeProvider{sb: sb}
	h := newHarnessWith(t, prov, Config{})
	h.prov = prov
	return h
}

// newHarnessWith is the provider-agnostic core: it seeds the fixture, flips the
// session to running, and wires an executor over the given provider and config.
func newHarnessWith(t *testing.T, provider sandbox.Provider, cfg Config) *harness {
	t.Helper()
	pool := pgtest.NewPool(t)
	// The executor is the cloud hands: it only claims tool_exec work for cloud
	// environments (self_hosted work is served by a BYOC worker via Poll).
	sid, envID := pgtest.NewSession(t, pool, "cloud")
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'running' WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		blobs: blobtest.Mem(), sid: sid, envID: envID,
	}
	h.exec = New(pool, h.log, h.queue, provider, h.blobs, cfg)
	return h
}

// suspend mimics the brain suspending a turn on a built-in tool: it appends the
// agent.tool_use intents and enqueues one tool_exec item, one transaction.
func (h *harness) suspend(t *testing.T, uses ...string) []domain.Event {
	t.Helper()
	return h.suspendUnder(t, context.Background(), uses...)
}

// suspendUnder is suspend under a caller's context, so a test can enqueue the
// item from inside a span the way a real mid-turn brain does. Enqueue dedupes
// per (session, kind) while an item is live, so the span has to be here — a
// second Enqueue would be a no-op and leave the first item's context in place.
func (h *harness) suspendUnder(t *testing.T, ctx context.Context, uses ...string) []domain.Event {
	t.Helper()
	var evs []events.NewEvent
	for _, u := range uses {
		evs = append(evs, events.NewEvent{Type: domain.EventAgentToolUse, Payload: json.RawMessage(u)})
	}
	out, err := h.log.AppendWith(ctx, h.sid, evs, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sid, queue.ToolExec)
			return err
		},
	})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	return out
}

func (h *harness) types(t *testing.T, typ string) []domain.Event {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sid, events.ListQuery{Types: []string{typ}})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (h *harness) liveOf(t *testing.T, kind queue.Kind) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id=$1 AND kind=$2 AND state != 'stopped'`,
		h.sid.String(), string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) leaseOf(t *testing.T) time.Time {
	t.Helper()
	var lease time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM work_items WHERE session_id=$1 AND kind='tool_exec'`,
		h.sid.String()).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// waitFor polls cond up to ~3s, failing the test if it never holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func writeUse(path, content string) string {
	b, _ := json.Marshal(map[string]any{
		"name": "write", "input": map[string]string{"file_path": path, "content": content},
	})
	return string(b)
}

func TestRunsToolAndSchedulesResume(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hello"))

	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !worked {
		t.Fatal("step found no work")
	}

	// The result is on the log, referencing the tool use, not an error.
	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
		Content   []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	uses := h.types(t, "agent.tool_use")
	if body.ToolUseID != uses[0].ID.String() {
		t.Errorf("result references %q, want %q", body.ToolUseID, uses[0].ID)
	}
	if body.IsError || body.Content[0].Text != "wrote 5 bytes to out.txt" {
		t.Errorf("result body = %+v", body)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("sandbox file = %q", sb.files["/workspace/out.txt"])
	}

	// The set is complete: a model_turn wakes the brain, the tool_exec is done.
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn items = %d, want 1 (resume)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec still live = %d, want 0 (completed)", got)
	}
}

func TestParallelToolsAllAnsweredBeforeResume(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := len(h.types(t, "agent.tool_result")); got != 2 {
		t.Errorf("results = %d, want both tools answered", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn items = %d, want exactly 1 for the full set", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec still live = %d, want 0", got)
	}
}

func TestToolLevelErrorIsAnsweredNotAbandoned(t *testing.T) {
	// A read of a missing file is a tool error the model reads — the executor
	// still answers it and resumes the turn.
	h := newHarness(t, &fakeSandbox{})
	read, _ := json.Marshal(map[string]any{"name": "read", "input": map[string]string{"file_path": "nope.txt"}})
	h.suspend(t, string(read))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		IsError bool `json:"is_error"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	if !body.IsError {
		t.Error("missing-file read should be an is_error result")
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (a tool error still resumes)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0", got)
	}
}

func TestBackendFaultLeavesItemForReclaim(t *testing.T) {
	// A backend fault (the sandbox write fails) is the executor's problem, not
	// the model's: the tool stays unanswered, no resume is scheduled, and the
	// tool_exec item is not completed so a reclaim retries it.
	boom := errors.New("connection refused")
	h := newHarness(t, &fakeSandbox{writeErr: boom})
	var faults int
	h.exec.onFault = func(*queue.Item, error) { faults++ }
	h.suspend(t, writeUse("out.txt", "hi"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if faults != 1 {
		t.Errorf("faults = %d, want 1", faults)
	}
	if got := len(h.types(t, "agent.tool_result")); got != 0 {
		t.Errorf("results = %d, want none (nothing ran to completion)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn = %d, want 0 (no resume on a backend fault)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1 (left for reclaim)", got)
	}
}

func TestReclaimReRunsOnlyUnanswered(t *testing.T) {
	// One of two tools already has a result on the log (a crash after the first
	// committed): the executor runs only the second, then resumes.
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))

	// Pretend the first tool's result already landed.
	answered, _ := json.Marshal(map[string]any{
		"tool_use_id": uses[0].ID.String(),
		"content":     []map[string]any{{"type": "text", "text": "wrote 3 bytes to a.txt"}},
		"is_error":    false,
	})
	if _, err := h.log.AppendWith(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventAgentToolResult, Payload: answered}}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	// Only b.txt was written this pass; a.txt was not re-run.
	if _, wrote := sb.files["/workspace/a.txt"]; wrote {
		t.Error("already-answered tool a.txt was re-run")
	}
	if sb.files["/workspace/b.txt"] != "two" {
		t.Error("unanswered tool b.txt was not run")
	}
	if got := len(h.types(t, "agent.tool_result")); got != 2 {
		t.Errorf("results = %d, want 2 (the pre-existing one plus b)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1", got)
	}
}

func TestProvisionFaultLeavesItemForReclaim(t *testing.T) {
	h := newHarness(t, nil)
	h.prov.provisionErr = errors.New("docker daemon unreachable")
	var faults int
	h.exec.onFault = func(*queue.Item, error) { faults++ }
	h.suspend(t, writeUse("out.txt", "hi"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if faults != 1 {
		t.Errorf("faults = %d, want 1", faults)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1", got)
	}
}

func TestEmptyClaimSleeps(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if worked {
		t.Error("step reported work with an empty queue")
	}
}

func TestRunProcessesQueuedWorkAndStopsOnCancel(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{PollInterval: 10 * time.Millisecond})
	h.suspend(t, writeUse("out.txt", "hi"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.exec.Run(ctx) }()

	// The loop drains the item, then idles on the empty queue until cancelled.
	waitFor(t, func() bool { return len(h.types(t, "agent.tool_result")) == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (completed)", got)
	}
}

func TestLeaseLostDuringToolAbortsCommit(t *testing.T) {
	// If the lease lapses mid-run (another executor reclaimed it), nothing this
	// executor ran may commit: no result, no resume — the reclaiming pass owns
	// the outcome. Stealing the lease from under a gated tool forces the keeper's
	// next renewal to fail, which cancels the work and aborts the commit.
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{LeaseTTL: 300 * time.Millisecond})
	var faults int
	h.exec.onFault = func(*queue.Item, error) { faults++ }
	h.suspend(t, writeUse("out.txt", "hi"))

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	<-sb.entered
	// Move the lease off the value the keeper holds: its next Extend finds no row.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE work_items SET lease_expires_at = lease_expires_at + interval '1 second'
		 WHERE session_id=$1 AND kind='tool_exec'`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	<-done // keeper failure cancels the work context, unblocking the gated tool

	if faults != 1 {
		t.Errorf("faults = %d, want 1 (lost lease)", faults)
	}
	if got := len(h.types(t, "agent.tool_result")); got != 0 {
		t.Errorf("results = %d, want 0 (nothing commits on a lost lease)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn = %d, want 0 (no resume on a lost lease)", got)
	}
	close(sb.gate) // release, though the tool already returned via cancellation
}

func TestLeaseRenewedWhileToolRuns(t *testing.T) {
	// A tool that outlives TTL/3 must not lose its lease: the keeper renews it in
	// the background, and the renewed proof is what the settling commit uses.
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{LeaseTTL: 300 * time.Millisecond})
	h.suspend(t, writeUse("out.txt", "hi"))

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	<-sb.entered
	lease0 := h.leaseOf(t)
	waitFor(t, func() bool { return h.leaseOf(t).After(lease0) }) // keeper renewed it
	close(sb.gate)
	<-done

	// The renewal did not break the commit: the result landed, the turn resumes,
	// and the item completed under the renewed lease.
	if got := len(h.types(t, "agent.tool_result")); got != 1 {
		t.Errorf("results = %d, want 1", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (completed under renewed lease)", got)
	}
}

func TestLeaseRenewedDuringSlowProvision(t *testing.T) {
	// Provisioning can be slow (an image pull); the keeper must renew across it,
	// or the lease lapses before the first tool runs and a second executor
	// reclaims. The keeper starts before Provision, so a run held in Provision
	// past TTL/3 still has its lease advanced.
	prov := &fakeProvider{sb: &fakeSandbox{}, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWith(t, prov, Config{LeaseTTL: 300 * time.Millisecond})
	h.suspend(t, writeUse("out.txt", "hi"))

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	<-prov.entered
	lease0 := h.leaseOf(t)
	waitFor(t, func() bool { return h.leaseOf(t).After(lease0) }) // renewed mid-provision
	close(prov.gate)
	<-done

	if got := len(h.types(t, "agent.tool_result")); got != 1 {
		t.Errorf("results = %d, want 1 (run completed after a slow provision)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0", got)
	}
}

func TestPartialFaultCommitsRanResultsLeavesItemLive(t *testing.T) {
	// Two tools where the first succeeds and the second backend-faults: the
	// first result commits (so a reclaim skips it) but the set is incomplete, so
	// no resume is scheduled and the item stays live for reclaim.
	sb := &fakeSandbox{failPath: "b.txt"}
	h := newHarnessWith(t, &fakeProvider{sb: sb}, Config{})
	var faults int
	h.exec.onFault = func(*queue.Item, error) { faults++ }
	uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if faults != 1 {
		t.Errorf("faults = %d, want 1", faults)
	}

	// Exactly the first tool's result committed, referencing the first use.
	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("agent.tool_result = %d, want 1 (only the tool that ran)", len(results))
	}
	var body struct {
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	if body.ToolUseID != uses[0].ID.String() {
		t.Errorf("committed result references %q, want the first use %q", body.ToolUseID, uses[0].ID)
	}
	if _, wrote := sb.files["/workspace/a.txt"]; !wrote {
		t.Error("first tool did not run")
	}

	// The set is incomplete (b unanswered), so no resume and the item is live.
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn = %d, want 0 (set incomplete)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1 (left for reclaim)", got)
	}
}

func TestStaleSessionDrainsWithoutRunning(t *testing.T) {
	// A session archived (or moved off running) while suspended on a tool must
	// not reclaim-loop: the executor drains the item instead of provisioning and
	// re-running its tools every lease period.
	for _, tc := range []struct {
		name   string
		mutate string
	}{
		{"archived", `UPDATE sessions SET archived_at = now() WHERE id = $1`},
		{"not running", `UPDATE sessions SET status = 'idle' WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &fakeSandbox{})
			h.suspend(t, writeUse("out.txt", "hi"))
			if _, err := h.pool.Exec(context.Background(), tc.mutate, h.sid.String()); err != nil {
				t.Fatal(err)
			}

			worked, err := h.exec.step(context.Background())
			if err != nil {
				t.Fatalf("step: %v", err)
			}
			if !worked {
				t.Fatal("step should have claimed the stale item")
			}
			if h.prov.provisions != 0 {
				t.Errorf("provisioned %d sandboxes for a stale session, want 0", h.prov.provisions)
			}
			if got := len(h.types(t, "agent.tool_result")); got != 0 {
				t.Errorf("agent.tool_result = %d, want 0", got)
			}
			if got := h.liveOf(t, queue.ToolExec); got != 0 {
				t.Errorf("tool_exec live = %d, want 0 (drained)", got)
			}
			if got := h.liveOf(t, queue.ModelTurn); got != 0 {
				t.Errorf("model_turn = %d, want 0 (a dead session is not resumed)", got)
			}
		})
	}
}

func TestUserToolResultCountsAsAnswered(t *testing.T) {
	// A tool_use already carries a result (here a user.tool_result) when the
	// executor claims its item — e.g. after a reclaim following a crash. The
	// executor must recognize the tool as answered and not re-run it or append a
	// duplicate result. (Cloud and self_hosted queues no longer overlap, so this
	// is the residual crash-recovery defense, not a cross-path race.)
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	uses := h.suspend(t, writeUse("out.txt", "hi"))

	ans, _ := json.Marshal(map[string]any{
		"tool_use_id": uses[0].ID.String(),
		"content":     []map[string]any{{"type": "text", "text": "worker ran it"}},
		"is_error":    false,
	})
	if _, err := h.log.AppendWith(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventUserToolResult, Payload: ans}}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, wrote := sb.files["/workspace/out.txt"]; wrote {
		t.Error("executor re-ran a tool already answered by user.tool_result")
	}
	if got := len(h.types(t, "agent.tool_result")); got != 0 {
		t.Errorf("agent.tool_result = %d, want 0 (no duplicate answer)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (drained)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (the answered set resumes)", got)
	}
}

func TestEmptyToolResultOmitsEmptyTextBlock(t *testing.T) {
	// A read of an empty file yields empty output. It must be an empty content
	// array, never a text block with an empty string — a Messages endpoint
	// rejects an empty text block, which would wedge the session on every resume.
	sb := &fakeSandbox{files: map[string]string{"/workspace/empty.txt": ""}}
	h := newHarness(t, sb)
	read, _ := json.Marshal(map[string]any{"name": "read", "input": map[string]string{"file_path": "empty.txt"}})
	h.suspend(t, string(read))

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		IsError bool             `json:"is_error"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(results[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.IsError {
		t.Errorf("empty read is not an error: %+v", body)
	}
	if len(body.Content) != 0 {
		t.Errorf("content = %v, want an empty array (no empty text block)", body.Content)
	}
}
