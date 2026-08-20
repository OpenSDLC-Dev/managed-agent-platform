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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
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
	// delay, if set, is how long each WriteFile takes, so a test can build a run
	// that is long overall while every single step is quick — the shape a stall
	// guard must carry rather than kill (#383).
	delay time.Duration
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
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	// The seam's own refusal, so the fake cannot be more permissive than the
	// backends it stands in for. Without it the fake reads -1 as "no bytes" and
	// lands an empty file, so a caller that passes an unknown length looks
	// correct here and fails on every mount in production (#386).
	if err := sandbox.CheckWriteSize(size); err != nil {
		return fmt.Errorf("fake: write %s: %w", path, err)
	}
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

// Attach answers ErrNotFound: nothing on the BYOC side reaches for a sandbox it
// did not provision, and a fake that handed one back would hide a caller that
// started to.
func (p *fakeProvider) Attach(context.Context, domain.ID) (sandbox.Sandbox, error) {
	return nil, sandbox.ErrNotFound
}

func (p *fakeProvider) Owned(context.Context) ([]domain.ID, error) { return nil, nil }

func (p *fakeProvider) Reap(context.Context, domain.ID) error { return nil }

func (p *fakeProvider) Export(context.Context, domain.ID, string) (io.ReadCloser, error) {
	return nil, sandbox.ErrNotFound
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
	// key is the environment key this harness issued; the platform generates
	// the value, so a test that builds its own SDK client takes it from here.
	key string
}

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
	pgtest.SetSessionStatus(t, pool, sid, "running")
	workerKey, err := api.IssueEnvironmentKey(ctx, pool, envID.String(), "worker-test")
	if err != nil {
		t.Fatalf("issue env key: %v", err)
	}
	blobs := blobtest.Mem()
	var handler http.Handler = api.NewHandler(pool, blobs, nil, nil)
	if wrap != nil {
		handler = wrap(handler)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	prov := &fakeProvider{sb: sb}
	client := NewClient(srv.URL, workerKey)
	return &harness{
		pool: pool, log: events.NewLog(pool), prov: prov, blobs: blobs, client: client, serverURL: srv.URL, sid: sid, envID: envID, key: workerKey,
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

// newest is the id of the session's most recent event among the four types the
// tool scan reads — the anchor a walk starting now would record.
func (h *harness) newest(t *testing.T) string {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sid, events.ListQuery{Types: []string{
		string(domain.EventAgentToolUse), string(domain.EventAgentToolResult),
		string(domain.EventUserToolResult), string(domain.EventUserToolConfirm),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no scannable event to anchor on")
	}
	return evs[len(evs)-1].ID.String()
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

// TestEmptyToolResultPostsPlaceholder: empty tool output posts the reference
// runner's "(no output)" text block (v1.63.1), never an empty text block — a
// Messages endpoint rejects an empty text block, and that request is what the
// brain replays.
func TestEmptyToolResultPostsPlaceholder(t *testing.T) {
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
	if len(results[0].Content) != 1 || results[0].Content[0]["type"] != "text" || results[0].Content[0]["text"] != toolset.NoOutput {
		t.Errorf("content = %v, want one text block %q", results[0].Content, toolset.NoOutput)
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

func TestRunSessionToolsReportsProgressPerItem(t *testing.T) {
	// The BYOC twin of the executor's per-item reporting (#383). The heartbeat
	// bounds this run by its silence, so every step that costs a round trip has
	// to report — including the scan that runs before provisioning, and the
	// references that resolve to nothing and so never reach the write loop.
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "prog-good", "100", "prog-notes", map[string]string{"SKILL.md": "ok"})
	h.refSkills(t, [2]string{"prog-gone", "latest"}, [2]string{"prog-good", "100"})
	h.suspend(t, writeUse("out.txt", "hello"))

	var reports int
	if err := RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(),
		ToolExecConfig{Progress: func() { reports++ }}); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	// The boundary before the scan (whatever the caller did last ends there), the
	// one event the scan walks and the scan's own boundary, the provision,
	// the two references resolved plus the version GET the concrete one goes on to
	// make (the dangling reference never reaches it, its listing failing first),
	// the sentinel decision, the one skill written,
	// the skills pass boundary before its sentinel write, the caller's skills and
	// files boundaries, and — for the one tool — its run and its posted result,
	// which are two steps and not one (#383). This session mounts no files, so
	// that pass returns before any boundary of its own.
	if reports != 14 {
		t.Errorf("progress reports = %d, want 14", reports)
	}

	// A second pass over an unchanged set takes the sentinel skip, which returns
	// before the write loop — so that decision, one sandbox read per recorded
	// tree, has to report on its own account.
	h.refSkills(t, [2]string{"prog-good", "100"})
	h.suspend(t, writeUse("out2.txt", "again"))
	reports = 0
	if err := RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(),
		ToolExecConfig{Progress: func() { reports++ }}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The boundary before the scan, the two events this scan walks before it can
	// bound the set, its own boundary,
	// the provision, the one reference resolved and its version GET, the one
	// sentinel read, the
	// caller's skills and files boundaries, and the tool's run and posted result.
	// The skip returns before the pass boundary the first run reached, which is
	// what makes this count one lower than a rewriting pass.
	if reports != 12 {
		t.Errorf("unchanged-set reports = %d, want 12", reports)
	}
}

// suspendOnThread is suspend for a child thread's turn: the intents carry the
// thread's id, the way the coordinator's spawns leave a subagent's calls on the
// log. On a self_hosted session the session view carries them (plan 35 decision
// 13 i), which is the only surface this thread-unaware worker reads.
func (h *harness) suspendOnThread(t *testing.T, threadID domain.ID, uses ...string) []domain.Event {
	t.Helper()
	var evs []events.NewEvent
	for _, u := range uses {
		evs = append(evs, events.NewEvent{Type: domain.EventAgentToolUse, ThreadID: threadID, Payload: json.RawMessage(u)})
	}
	out, err := h.log.AppendWith(context.Background(), h.sid, evs, events.AppendOptions{ThreadID: threadID})
	if err != nil {
		t.Fatalf("suspend on %s: %v", threadID, err)
	}
	return out
}

// answerOnThread is answerAsPlatform for a child thread's call: this worker's
// own user.tool_result, stored on the thread that made the call.
func (h *harness) answerOnThread(t *testing.T, threadID, useID domain.ID) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     []map[string]any{{"type": "text", "text": "done"}},
		"is_error":    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.AppendWith(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventUserToolResult, ThreadID: threadID, Payload: body}},
		events.AppendOptions{ThreadID: threadID}); err != nil {
		t.Fatalf("answer %s on %s: %v", useID, threadID, err)
	}
}

// confirm appends a human's verdict on an ask-gated call. It goes straight to
// the log rather than through the events API, because the inbound route would
// also resume the turn; this fixture is about what the scan sees. But the
// payload is built by the control plane's own writer (events.NormalizeInbound)
// rather than by hand, because the gate under test reads one field out of it
// by name: a hand-rolled fixture would keep saying "result" after a rename and
// leave these cases green while every confirmed ask-gated call on a
// self_hosted session silently stopped running.
func (h *harness) confirm(t *testing.T, useID domain.ID, result string) {
	t.Helper()
	evs, err := events.NormalizeInbound(string(domain.EnvSelfHosted), []json.RawMessage{json.RawMessage(
		fmt.Sprintf(`{"type":"user.tool_confirmation","tool_use_id":%q,"result":%q}`, useID, result))})
	if err != nil {
		t.Fatalf("normalize confirmation for %s: %v", useID, err)
	}
	if _, err := h.log.AppendWith(context.Background(), h.sid, evs, events.AppendOptions{}); err != nil {
		t.Fatalf("confirm %s: %v", useID, err)
	}
}

// runMode runs the driver in the given mode — coordinator being the multiagent
// session's full walk and re-scan loop, which the lease loop derives from the
// session's own snapshot.
func (h *harness) runMode(coordinator bool) error {
	return RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(),
		ToolExecConfig{Coordinator: coordinator})
}

// gatedUse is writeUse carrying a permission verdict — the field the brain
// stamps on every platform tool call, "allow" unless the agent's policy asks or
// denies.
func gatedUse(path, content, permission string) string {
	b, _ := json.Marshal(map[string]any{
		"name": "write", "input": map[string]string{"file_path": path, "content": content},
		"evaluated_permission": permission,
	})
	return string(b)
}

// useIDs renders a scan's result as the use ids it returned, in order, so a
// mismatch reads as two lists rather than two structs.
func useIDs(uses []toolUse) []string {
	out := make([]string, len(uses))
	for i, u := range uses {
		out[i] = u.id.String()
	}
	return out
}

// TestCoordinatorScanWalksTheWholeLog: sibling threads break the #76 bound's
// premise. Thread A's call sits below thread B's answered pair, so a walk that
// stops at the first result older than the trailing run never reads it — and
// nothing else will, because B's enqueue is a no-op against the live item. A
// coordinator session therefore walks to the log's start; a single-agent one
// must keep the bound exactly, which is what the other half of this test pins.
func TestCoordinatorScanWalksTheWholeLog(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	a := pgtest.NewChildThread(t, h.pool, h.sid)
	b := pgtest.NewChildThread(t, h.pool, h.sid)
	stranded := h.suspendOnThread(t, a, writeUse("a.txt", "one"))
	answered := h.suspendOnThread(t, b, writeUse("b1.txt", "two"))
	h.answerOnThread(t, b, answered[0].ID)
	trailing := h.suspendOnThread(t, b, writeUse("b2.txt", "three"))

	bounded, err := unansweredToolUses(context.Background(), h.client, h.sid.String(), false, func() {})
	if err != nil {
		t.Fatalf("bounded scan: %v", err)
	}
	if got, want := useIDs(bounded), []string{trailing[0].ID.String()}; !slices.Equal(got, want) {
		t.Errorf("single-agent scan = %v, want the trailing turn only %v", got, want)
	}

	full, err := unansweredToolUses(context.Background(), h.client, h.sid.String(), true, func() {})
	if err != nil {
		t.Fatalf("full scan: %v", err)
	}
	want := []string{stranded[0].ID.String(), trailing[0].ID.String()}
	if got := useIDs(full); !slices.Equal(got, want) {
		t.Errorf("coordinator scan = %v, want every unanswered call oldest-first %v", got, want)
	}
}

// TestAskGatedUseWaitsForItsVerdict: the worker runs the runnable set, not the
// unanswered one — an ask-gated call is nobody's to run until a human allows it
// (plan 35 decision 5). The rule is not coordinator-scoped: one turn's two asks
// are released one at a time on a single-agent session too, and running the
// unreleased sibling would execute a command its human never answered.
func TestAskGatedUseWaitsForItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name         string
		permission   string
		confirmation string
		wantRun      bool
	}{
		{"an allow policy runs", "allow", "", true},
		{"an absent verdict field reads as allow", "", "", true},
		{"an ask without a confirmation waits", "ask", "", false},
		{"an ask a human allowed runs", "ask", "allow", true},
		{"an ask a human denied never runs", "ask", "deny", false},
		{"a deny never runs", "deny", "", false},
		// The API cannot produce this shape — ValidateToolConfirmations refuses a
		// confirmation that does not name an ask-gated call — so the case exists
		// to pin the scan's own gate rather than the route's. The scan reads the
		// log over the wire and cannot see that validation; a release bound to
		// "any confirmation" rather than to "ask" would run here what the policy
		// refused.
		{"a deny a human allowed still never runs", "deny", "allow", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &fakeSandbox{})
			use := writeUse("out.txt", "hi")
			if tc.permission != "" {
				use = gatedUse("out.txt", "hi", tc.permission)
			}
			uses := h.suspend(t, use)
			if tc.confirmation != "" {
				h.confirm(t, uses[0].ID, tc.confirmation)
			}

			got, err := unansweredToolUses(context.Background(), h.client, h.sid.String(), false, func() {})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if tc.wantRun && len(got) != 1 {
				t.Fatalf("scan = %v, want the call to be runnable", useIDs(got))
			}
			if !tc.wantRun && len(got) != 0 {
				t.Fatalf("scan = %v, want nothing runnable", useIDs(got))
			}
		})
	}
}

// TestAnsweredDuringTheWalkIsNotRun: the coordinator's walk pages by a cursor on
// the sequence it started from, so a result appended above that cursor while the
// walk is still running is invisible to the rest of it — the call it answers sits
// below, still looking unanswered. Running it would execute a command that had
// already been settled, which is what a thread-scoped interrupt does to a
// sibling's outstanding call: it synthesizes the result and cancels nothing.
//
// The race itself needs an append wedged inside a multi-page walk, which no
// fixture can time. What is testable is the check that closes it, and on its own
// contract: given the anchor the walk started from, a call answered above it is
// dropped, and one answered below it — which the walk saw for itself — is left to
// the walk's own judgement.
func TestAnsweredDuringTheWalkIsNotRun(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))
	first, second := uses[0].ID, uses[1].ID

	scanned := []toolUse{{id: first, name: "write"}, {id: second, name: "write"}}
	// The anchor is the newest event the walk saw. Answering past it is exactly
	// the append the walk cannot see.
	head := h.newest(t)
	h.answerAsPlatform(t, second)

	kept, err := dropAnsweredSince(context.Background(), h.client, h.sid.String(), head, scanned, func() {})
	if err != nil {
		t.Fatalf("dropAnsweredSince: %v", err)
	}
	if got := useIDs(kept); !slices.Equal(got, []string{first.String()}) {
		t.Errorf("kept = %v, want only the call still outstanding %v", got, first)
	}

	// Anchored at the head as it now stands, the same result is below the anchor
	// — the walk's own view — and nothing is dropped a second time.
	kept, err = dropAnsweredSince(context.Background(), h.client, h.sid.String(), h.newest(t), scanned, func() {})
	if err != nil {
		t.Fatalf("dropAnsweredSince: %v", err)
	}
	if len(kept) != 2 {
		t.Errorf("kept = %v, want both left to the walk that already saw the answer", useIDs(kept))
	}
}

// TestTheWalkRechecksThroughTheRealPath is TestAnsweredDuringTheWalkIsNotRun's
// twin one level up: that one pins dropAnsweredSince's own contract, this one
// pins that the walk actually uses it — with the anchor it walked from, and only
// in the mode that needs it. Between them, dropping the call, passing the wrong
// head or running it in the wrong mode all go red.
//
// The race is an append landing after a page has been served, which no fixture
// can time from outside. So the harness wraps the control plane and does the
// appending itself: the first events list the walk asks for is served, and then,
// before the reply is anything the walk can act on, a result answers one of the
// calls that list just reported unanswered. That is exactly the interleaving a
// thread-scoped interrupt produces on a sibling's outstanding call.
func TestTheWalkRechecksThroughTheRealPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coordinator bool
		wantKept    int
	}{
		// A coordinator's walk spans the whole log, so the window is arbitrarily
		// wide and the recheck runs.
		{"a coordinator's walk drops it", true, 1},
		// A single-agent walk stops at the trailing turn's boundary; its window
		// is one page and it keeps today's behaviour, late result dropped on
		// post rather than on scan.
		{"a single-agent walk is unchanged", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			var armed, fired atomic.Bool
			var answer domain.ID
			h = newHarnessWrapped(t, &fakeSandbox{}, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					if !armed.Load() || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/events") {
						return
					}
					if fired.CompareAndSwap(false, true) {
						h.answerAsPlatform(t, answer)
					}
				})
			})
			uses := h.suspend(t, writeUse("a.txt", "one"), writeUse("b.txt", "two"))
			answer = uses[1].ID

			armed.Store(true)
			got, err := unansweredToolUses(context.Background(), h.client, h.sid.String(), tc.coordinator, func() {})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !fired.Load() {
				t.Fatal("the walk asked for no events page; the fixture never got to inject")
			}
			if len(got) != tc.wantKept {
				t.Fatalf("scan = %v, want %d call(s) runnable", useIDs(got), tc.wantKept)
			}
			if tc.coordinator && got[0].id != uses[0].ID {
				t.Errorf("scan kept %v, want the one still outstanding %v", useIDs(got), uses[0].ID)
			}
		})
	}
}

// TestRescanIsCoordinatorOnly: a sibling thread can commit tool calls while the
// worker is running another thread's, and its enqueue is a no-op against the
// live item — so a one-pass driver would leave them for nobody. A coordinator
// run re-scans after answering the set it found and picks them up; a
// single-agent run keeps today's single pass, where the window cannot open
// because a session's next turn cannot start before its last result lands.
func TestRescanIsCoordinatorOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coordinator bool
		want        int
	}{
		{"coordinator answers the call committed mid-run", true, 2},
		{"single agent leaves it for the next item", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
			h := newHarness(t, sb)
			h.suspend(t, writeUse("first.txt", "one"))

			done := make(chan error, 1)
			go func() { done <- h.runMode(tc.coordinator) }()
			<-sb.entered // the first tool is in the sandbox: commit the late call now
			h.suspend(t, writeUse("late.txt", "two"))
			close(sb.gate)

			if err := <-done; err != nil {
				t.Fatalf("RunSessionTools: %v", err)
			}
			if got := len(h.results(t)); got != tc.want {
				t.Errorf("user.tool_result = %d, want %d", got, tc.want)
			}
			if _, ran := sb.files["/workspace/late.txt"]; ran != tc.coordinator {
				t.Errorf("the late call ran = %v, want %v", ran, tc.coordinator)
			}
			if h.prov.provisions != 1 {
				t.Errorf("provisions = %d, want 1 (the re-scan reuses the sandbox)", h.prov.provisions)
			}
		})
	}
}

// TestCoordinatorSkipsDelegationCalls: a delegation call is answered by the
// settlement that emitted it, so the full walk should never meet an unanswered
// one. If a log ever presented one anyway, running it would answer it
// "unknown tool" and the post would be refused — faulting the item into a
// reclaim loop. Skipped by name, for the reason the web filter gives.
func TestCoordinatorSkipsDelegationCalls(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	spawn, _ := json.Marshal(map[string]any{
		"name": toolset.ToolCreateAgent, "input": map[string]string{"agent_name": "worker", "message": "go"},
	})
	uses := h.suspend(t, string(spawn), writeUse("out.txt", "hello"))

	got, err := unansweredToolUses(context.Background(), h.client, h.sid.String(), true, func() {})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if want := []string{uses[1].ID.String()}; !slices.Equal(useIDs(got), want) {
		t.Errorf("scan = %v, want the sandbox tool only %v", useIDs(got), want)
	}
}
