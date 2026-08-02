package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// fakeSandbox is an in-memory sandbox. The driver tests exercise the write and
// read tools, which use the file primitives directly (no shell), so a minimal
// file store is enough. failPath, if set, faults WriteFile for a path with that
// suffix, letting a test fault one tool of a set while the others succeed.
// entered/gate (if set) let a test hold a tool mid-run: WriteFile signals
// entered once and blocks on gate, so the lease loop can be observed while a
// tool is in flight.
type fakeSandbox struct {
	files    map[string]string
	failPath string
	entered  chan struct{}
	gate     chan struct{}
	// bulkSizes records the member count of every WriteFiles call, so a test can
	// hold a materializer to one batched call carrying a skill's whole tree,
	// rather than one write per file (#206).
	bulkSizes []int
}

func (f *fakeSandbox) ID() string { return "fake" }
func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	// Reflect real file presence for SetupFiles's mountsPresent probe
	// (`test -e '<p>' && … && true`), so a deleted mount reports absent and forces
	// re-materialization. The exact shape match keeps ordinary tool commands on
	// the unconditional exit-0 path.
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
	data, ok := f.files[path]
	if !ok {
		return nil, sandbox.ErrFileNotExist
	}
	return []byte(data), nil
}
func (f *fakeSandbox) ReadFileStream(ctx context.Context, path string, maxBytes int64) (io.ReadCloser, int64, error) {
	data, err := f.ReadFile(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > maxBytes {
		return nil, 0, sandbox.ErrFileTooLarge
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
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

// WriteFiles is the batch every backend lands for a fixed couple of execs
// however many members it carries; the fake keeps the same observable semantics
// by writing the members in order and stopping at the first failure.
func (f *fakeSandbox) WriteFiles(ctx context.Context, files []sandbox.FileWrite) error {
	f.bulkSizes = append(f.bulkSizes, len(files))
	for _, w := range files {
		if err := f.WriteFile(ctx, w.Path, w.Data); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeSandbox) Destroy(context.Context) error { return nil }

type fakeProvider struct {
	sb           *fakeSandbox
	provisionErr error
	provisions   int
	lastSpec     sandbox.Spec // captured for the hardening-wiring assertion
}

func (p *fakeProvider) Provision(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	p.provisions++
	p.lastSpec = spec
	if p.provisionErr != nil {
		return nil, p.provisionErr
	}
	return p.sb, nil
}

type harness struct {
	pool      *pgxpool.Pool
	log       *events.Log
	prov      *fakeProvider
	blobs     *blobtest.MemStore
	client    sdk.Client
	run       func() error
	serverURL string
	sid       domain.ID
	envID     domain.ID
}

const workerKey = "ek-worker-test"

// newHarness stands up a control plane over a fresh Dockerized Postgres, exposes
// it over HTTP, and wires a worker SDK client to it — the same wire path a real
// BYOC worker takes. The session is a self_hosted one flipped to running, as the
// brain leaves it when a turn suspends for a tool.
func newHarness(t *testing.T, sb *fakeSandbox) *harness {
	return newHarnessWrapped(t, sb, nil)
}

// newHarnessWrapped is newHarness with an optional handler wrapper, letting a
// test intercept the wire (e.g. fault heartbeat requests) between the worker
// client and the real control plane. wrap == nil is the plain control plane.
func newHarnessWrapped(t *testing.T, sb *fakeSandbox, wrap func(http.Handler) http.Handler) *harness {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET status = 'running' WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if err := api.EnsureEnvironmentKey(ctx, pool, envID.String(), workerKey); err != nil {
		t.Fatalf("ensure env key: %v", err)
	}
	blobs := blobtest.Mem()
	var handler http.Handler = api.NewHandler(pool, blobs, nil)
	if wrap != nil {
		handler = wrap(handler)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	prov := &fakeProvider{sb: sb}
	client := NewClient(srv.URL, workerKey)
	return &harness{
		pool: pool, log: events.NewLog(pool), prov: prov, blobs: blobs, client: client, serverURL: srv.URL, sid: sid, envID: envID,
		run: func() error {
			return RunSessionTools(ctx, client, prov, sid.String(), ToolExecConfig{})
		},
	}
}

// suspend mimics the brain suspending a turn on built-in tools: it appends the
// agent.tool_use intents and returns them (with their server-assigned ids).
func (h *harness) suspend(t *testing.T, uses ...string) []domain.Event {
	t.Helper()
	var evs []events.NewEvent
	for _, u := range uses {
		evs = append(evs, events.NewEvent{Type: domain.EventAgentToolUse, Payload: json.RawMessage(u)})
	}
	out, err := h.log.AppendWith(context.Background(), h.sid, evs, events.AppendOptions{})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	return out
}

// answerAsPlatform appends the agent.tool_result that answers one tool use — the
// shape a denied confirmation synthesizes, and the other of the two types that
// count as answering (the worker's own user.tool_result is exercised by
// answeredHistory and TestAlreadyAnsweredIsNoOp).
func (h *harness) answerAsPlatform(t *testing.T, useID domain.ID) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     []map[string]any{{"type": "text", "text": "already done"}},
		"is_error":    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.AppendWith(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventAgentToolResult, Payload: body}}, events.AppendOptions{}); err != nil {
		t.Fatalf("answer %s: %v", useID, err)
	}
}

// answeredHistory appends n fully-answered agent.tool_use / user.tool_result
// pairs — the long prior history the scan must not re-page. The use ids are
// minted here rather than read back, so the whole history lands in one append.
func (h *harness) answeredHistory(t *testing.T, n int) {
	t.Helper()
	var evs []events.NewEvent
	for i := 0; i < n; i++ {
		id := domain.NewID("sevt")
		body, err := json.Marshal(map[string]any{
			"tool_use_id": id.String(),
			"content":     []map[string]any{{"type": "text", "text": "done"}},
			"is_error":    false,
		})
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs,
			events.NewEvent{ID: id, Type: domain.EventAgentToolUse,
				Payload: json.RawMessage(writeUse(fmt.Sprintf("old%d.txt", i), "x"))},
			events.NewEvent{Type: domain.EventUserToolResult, Payload: body},
		)
	}
	if _, err := h.log.AppendWith(context.Background(), h.sid, evs, events.AppendOptions{}); err != nil {
		t.Fatalf("answered history: %v", err)
	}
}

func (h *harness) types(t *testing.T, typ string) []domain.Event {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sid, events.ListQuery{Types: []string{typ}})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// liveModelTurns counts the resume work items the control plane enqueued for the
// session — the observable signal that the brain will wake and continue.
func (h *harness) liveModelTurns(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1 AND kind = 'model_turn' AND state != 'stopped'`,
		h.sid.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func writeUse(path, content string) string {
	b, _ := json.Marshal(map[string]any{
		"name": "write", "input": map[string]string{"file_path": path, "content": content},
	})
	return string(b)
}

func readUse(path string) string {
	b, _ := json.Marshal(map[string]any{
		"name": "read", "input": map[string]string{"file_path": path},
	})
	return string(b)
}

// resultBody is the stored user.tool_result shape the assertions read back.
type resultBody struct {
	ToolUseID string           `json:"tool_use_id"`
	IsError   bool             `json:"is_error"`
	Content   []map[string]any `json:"content"`
}

func (h *harness) results(t *testing.T) []resultBody {
	t.Helper()
	evs := h.types(t, string(domain.EventUserToolResult))
	out := make([]resultBody, len(evs))
	for i, ev := range evs {
		if err := json.Unmarshal(ev.Body, &out[i]); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
	}
	return out
}

// TestRunsToolAndControlPlaneResumes is the happy path: the worker reads the
// session's outstanding tool over the wire, runs it in the sandbox, posts a
// user.tool_result, and — because that completes the set — the control plane
// enqueues the resume turn on its own. The worker never enqueues a turn itself.
func TestRunsToolAndControlPlaneResumes(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	uses := h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	results := h.results(t)
	if len(results) != 1 {
		t.Fatalf("user.tool_result = %d, want 1", len(results))
	}
	if results[0].ToolUseID != uses[0].ID.String() {
		t.Errorf("result references %q, want %q", results[0].ToolUseID, uses[0].ID)
	}
	if results[0].IsError {
		t.Errorf("result is_error = true, want false: %+v", results[0])
	}
	if got, _ := results[0].Content[0]["text"].(string); got != "wrote 5 bytes to out.txt" {
		t.Errorf("result content = %+v", results[0].Content)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("sandbox file = %q, want the tool to have written it", sb.files["/workspace/out.txt"])
	}
	if got := h.liveModelTurns(t); got != 1 {
		t.Errorf("model_turn items = %d, want 1 (the completed set resumes)", got)
	}
}

// TestParallelToolsResumeOnlyWhenComplete pins that posting per tool does not
// resume the turn early: two outstanding tools yield two user.tool_result events
// but exactly one resume, because the control plane waits for the full set.
func TestParallelToolsResumeOnlyWhenComplete(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	if got := len(h.results(t)); got != 2 {
		t.Errorf("user.tool_result = %d, want both tools answered", got)
	}
	if got := h.liveModelTurns(t); got != 1 {
		t.Errorf("model_turn items = %d, want exactly 1 for the full set", got)
	}
	if h.prov.provisions != 1 {
		t.Errorf("provisions = %d, want 1 (one sandbox for the session)", h.prov.provisions)
	}
}

// TestToolLevelErrorIsAnsweredNotAbandoned: a read of a missing file is a tool
// error the model must see, not a worker fault — the worker posts an is_error
// result and the turn still resumes.
func TestToolLevelErrorIsAnsweredNotAbandoned(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, readUse("nope.txt"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	results := h.results(t)
	if len(results) != 1 {
		t.Fatalf("user.tool_result = %d, want 1", len(results))
	}
	if !results[0].IsError {
		t.Error("missing-file read should post an is_error result")
	}
	if got := h.liveModelTurns(t); got != 1 {
		t.Errorf("model_turn = %d, want 1 (a tool error still resumes)", got)
	}
}

// TestEmptyToolResultOmitsContent: empty tool output must post no content blocks
// (stored as null content), never an empty text block — a Messages endpoint
// rejects an empty text block, and that request is what the brain replays.
func TestEmptyToolResultOmitsContent(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{"/workspace/empty.txt": ""}}
	h := newHarness(t, sb)
	h.suspend(t, readUse("empty.txt"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	results := h.results(t)
	if len(results) != 1 {
		t.Fatalf("user.tool_result = %d, want 1", len(results))
	}
	if results[0].IsError {
		t.Errorf("empty read is not an error: %+v", results[0])
	}
	if len(results[0].Content) != 0 {
		t.Errorf("content = %v, want empty (no content blocks for empty output)", results[0].Content)
	}
}

// TestAlreadyAnsweredIsNoOp: a session whose tools already carry results (a
// redundant reclaim) runs nothing — no sandbox is provisioned and no duplicate
// result is posted.
func TestAlreadyAnsweredIsNoOp(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	uses := h.suspend(t, writeUse("out.txt", "hi"))

	// A prior pass already answered it (here as a user.tool_result on the log).
	answered, _ := json.Marshal(map[string]any{
		"tool_use_id": uses[0].ID.String(),
		"content":     []map[string]any{{"type": "text", "text": "already done"}},
		"is_error":    false,
	})
	if _, err := h.log.AppendWith(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventUserToolResult, Payload: answered}}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (nothing to run)", h.prov.provisions)
	}
	if _, wrote := sb.files["/workspace/out.txt"]; wrote {
		t.Error("an already-answered tool was re-run")
	}
	if got := len(h.results(t)); got != 1 {
		t.Errorf("user.tool_result = %d, want 1 (no duplicate)", got)
	}
}

// TestWebToolUseIsNeverAnsweredByTheWorker: a web call (web_fetch/web_search)
// is the platform executor's, whatever environment the session runs in — this
// worker implements the six sandbox tools, like the official client toolset.
// The enqueue hold-back means a polled item should never coexist with an
// unanswered web call; if a stray log shape ever presents one anyway, the scan
// must skip it rather than feed it to the Runner's unknown-tool arm and post a
// wrong-shaped answer to a call that is not this worker's to answer.
func TestWebToolUseIsNeverAnsweredByTheWorker(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	webUse, _ := json.Marshal(map[string]any{
		"name": "web_search", "input": map[string]string{"query": "golang"},
	})
	uses := h.suspend(t, string(webUse), writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	results := h.results(t)
	if len(results) != 1 {
		t.Fatalf("user.tool_result = %d, want 1 (the sandbox tool only)", len(results))
	}
	if got := results[0].ToolUseID; got != uses[1].ID.String() {
		t.Errorf("result references %q, want the write use %q — never the web call", got, uses[1].ID)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("the sandbox tool did not run: %v", sb.files)
	}
}

// countEventReads returns a counter of the scan's wire cost and the handler
// wrapper that feeds it: one tick per GET of the session events list, which is
// the only request the diff makes. Both bound tests share it so they cannot
// drift into counting different things.
func countEventReads() (*atomic.Int32, func(http.Handler) http.Handler) {
	var reads atomic.Int32
	return &reads, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
				reads.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TestScanIsBoundedByTheTrailingTurn: the outstanding set is always the last
// suspended turn's, so the wire scan must cost the same on a long session as on
// a fresh one. With sixty answered tool calls behind it, reading the one
// outstanding tool takes a single events request — the newest-first walk stops
// at the first result older than the trailing turn's uses. The old
// oldest-first full scan paged the whole history twice (seven requests here,
// growing with the session), which is what #76 bounds.
func TestScanIsBoundedByTheTrailingTurn(t *testing.T) {
	reads, count := countEventReads()
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, count)
	h.answeredHistory(t, 60)
	uses := h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	if got := reads.Load(); got != 1 {
		t.Errorf("events requests = %d, want 1 (the scan must not re-page prior history)", got)
	}
	results := h.results(t)
	if len(results) != 61 {
		t.Fatalf("user.tool_result = %d, want 61 (60 historical + 1 new)", len(results))
	}
	if got := results[60].ToolUseID; got != uses[0].ID.String() {
		t.Errorf("the posted result references %q, want the outstanding use %q", got, uses[0].ID)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("outstanding tool did not run: %v", sb.files)
	}
	if _, reran := sb.files["/workspace/old0.txt"]; reran {
		t.Error("an answered historical tool was re-run")
	}
}

// TestScanPagesATurnWiderThanOnePage: a turn with more tools than one page
// holds still walks to its own boundary — the follow-up request must keep
// walking newest-first (the cursor binds the direction the control plane minted
// it under), stop on the later page where the prior turn's result appears, and
// still hand the tools back in log order across the page break.
func TestScanPagesATurnWiderThanOnePage(t *testing.T) {
	const tools = toolScanPageSize + 5
	reads, count := countEventReads()
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, count)
	h.answeredHistory(t, 30)
	var batch []string
	for i := 0; i < tools; i++ {
		batch = append(batch, writeUse(fmt.Sprintf("new%d.txt", i), fmt.Sprint(i)))
	}
	uses := h.suspend(t, batch...)

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	if got := reads.Load(); got != 2 {
		t.Errorf("events requests = %d, want 2 (the turn spans two pages, and no more)", got)
	}
	results := h.results(t)
	if len(results) != 30+tools {
		t.Fatalf("user.tool_result = %d, want %d", len(results), 30+tools)
	}
	for i, r := range results[30:] {
		if r.ToolUseID != uses[i].ID.String() {
			t.Fatalf("result %d references %q, want %q (tools must run in log order)", i, r.ToolUseID, uses[i].ID)
		}
	}
}

// TestOutOfOrderAnswerStillRunsTheEarlierTool: within one suspended turn a later
// tool can be answered before an earlier one — a denial synthesizes its
// agent.tool_result immediately while the allowed tool is still outstanding, and
// a faulted pass answers only what ran. The newest-first walk must therefore
// read the whole trailing turn, not stop at the first answered use it meets.
func TestOutOfOrderAnswerStillRunsTheEarlierTool(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, nil)
	h.answeredHistory(t, 3)
	uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))
	h.answerAsPlatform(t, uses[1].ID) // the second use, answered first

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	if _, ran := sb.files["/workspace/a.txt"]; !ran {
		t.Error("the still-unanswered earlier tool was skipped")
	}
	if _, reran := sb.files["/workspace/b.txt"]; reran {
		t.Error("the already-answered later tool was re-run")
	}
	results := h.results(t)
	if len(results) != 4 {
		t.Fatalf("user.tool_result = %d, want 4 (3 historical + 1 new)", len(results))
	}
	if got := results[3].ToolUseID; got != uses[0].ID.String() {
		t.Errorf("the posted result references %q, want the earlier use %q", got, uses[0].ID)
	}
}

// TestBackendFaultPostsRanResultsAndStops: with two tools where the second
// backend-faults, the first result is posted (so a reclaim skips it) and the
// driver returns the fault with the second tool left unanswered — the set is
// incomplete, so the turn does not resume.
func TestBackendFaultPostsRanResultsAndStops(t *testing.T) {
	sb := &fakeSandbox{failPath: "b.txt"}
	h := newHarness(t, sb)
	uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))

	err := h.run()
	if err == nil {
		t.Fatal("RunSessionTools returned nil, want the backend fault")
	}

	results := h.results(t)
	if len(results) != 1 {
		t.Fatalf("user.tool_result = %d, want 1 (only the tool that ran)", len(results))
	}
	if results[0].ToolUseID != uses[0].ID.String() {
		t.Errorf("posted result references %q, want the first use %q", results[0].ToolUseID, uses[0].ID)
	}
	if got := h.liveModelTurns(t); got != 0 {
		t.Errorf("model_turn = %d, want 0 (set incomplete)", got)
	}
}

// TestProvisionFaultSurfaces: a sandbox that fails to provision surfaces as an
// error with nothing posted — there is no partial state to leave behind.
func TestProvisionFaultSurfaces(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.provisionErr = fmt.Errorf("docker daemon unreachable")
	h.suspend(t, writeUse("out.txt", "hi"))

	if err := h.run(); err == nil {
		t.Fatal("RunSessionTools returned nil, want the provision fault")
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (nothing ran)", got)
	}
	if got := h.liveModelTurns(t); got != 0 {
		t.Errorf("model_turn = %d, want 0", got)
	}
}

// TestArchivedSessionPostIsRefusedAndSurfaces: the control plane refuses a
// result posted to an archived (read-only) session with a 400, and the driver
// surfaces that error rather than wedging the log — the safety net behind a
// caller that has not yet gated on session liveness (see RunSessionTools). No
// result lands and nothing resumes.
func TestArchivedSessionPostIsRefusedAndSurfaces(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, writeUse("out.txt", "hi"))
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET archived_at = now() WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}

	if err := h.run(); err == nil {
		t.Fatal("RunSessionTools returned nil, want the archived-session rejection")
	}
	if got := len(h.results(t)); got != 0 {
		t.Errorf("user.tool_result = %d, want 0 (the append was refused)", got)
	}
	if got := h.liveModelTurns(t); got != 0 {
		t.Errorf("model_turn = %d, want 0 (nothing resumed)", got)
	}
}

// TestInvalidEnvironmentKeyRejected: the worker authenticates with its
// environment key; a bad key is rejected by the control plane and surfaces as an
// error from the very first read, before any sandbox is provisioned.
func TestInvalidEnvironmentKeyRejected(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.suspend(t, writeUse("out.txt", "hi"))

	badClient := NewClient(h.serverURL, "ek-not-a-real-key")
	err := RunSessionTools(context.Background(), badClient, h.prov, h.sid.String(), ToolExecConfig{})
	if err == nil {
		t.Fatal("RunSessionTools with a bad key returned nil, want an auth error")
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 (rejected before provisioning)", h.prov.provisions)
	}
}

// The BYOC worker shares the toolset boundary, so a NUL-emitting tool answers
// on this path too - sanitized once in the shared dispatch, not at a second
// worker-side site (#223).
func TestNULToolOutputIsSanitizedByTheWorker(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{"/workspace/nul.dat": "a\x00b"}}
	h := newHarness(t, sb)
	uses := h.suspend(t, readUse("nul.dat"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}

	results := h.results(t)
	if len(results) != 1 || results[0].IsError || results[0].ToolUseID != uses[0].ID.String() {
		t.Fatalf("results = %+v, want one non-error answer to the read", results)
	}
	if len(results[0].Content) != 1 || results[0].Content[0]["text"] != "ab" {
		t.Errorf("content = %+v, want one text block %q", results[0].Content, "ab")
	}
}
