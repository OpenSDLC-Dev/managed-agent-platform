package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
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
	// writes counts every WriteFile, successful or not, so a test can pin how
	// many attempts a pass made rather than only what survived them.
	writes int
	// failPath, if set, makes WriteFile fail (a backend fault) for a path with
	// this suffix, so a test can fault one tool of a parallel set while the
	// others succeed.
	failPath string
	// entered (if set) receives one signal the first time WriteFile is entered,
	// and gate (if set) blocks WriteFile until closed — together they let a test
	// hold a tool mid-run to observe the lease keeper renew. gateFrom is the
	// WriteFile call the gate starts holding at (1-based; 0 means the first), so
	// a test can let some tools finish before one wedges. writes counts them.
	entered  chan struct{}
	gate     chan struct{}
	gateFrom int
	// delay, if set, is how long each WriteFile takes, so a test can build a run
	// that is long overall while every single step is quick — the shape a stall
	// guard must carry rather than kill (#383).
	delay time.Duration
	// bulkSizes records the member count of every WriteFiles call, so a test can
	// hold a materializer to one batched call carrying a skill's whole tree,
	// rather than one write per file (#206).
	bulkSizes []int
	// execStdout, if set, is returned verbatim as the harvest listing script's
	// stdout — a test forging what an agent-writable sandbox could emit; and
	// execTruncated marks that listing as overflowing the exec output cap.
	execStdout    string
	execTruncated bool
	// cmds records every Exec command in order; execHook, when set and
	// answering non-nil, overrides one command's result (the restore tests
	// fault the in-sandbox extraction).
	cmds     []string
	execHook func(sandbox.ExecRequest) *sandbox.ExecResult
	// listTruncated marks the memory sync's tree listing as overflowing the
	// exec output cap, and listExit is a status the listing fails with (a
	// directory find could not enter, under the command's pipefail) — the
	// two answers that must skip a store's sync.
	listTruncated bool
	listExit      int
	// modes records the Mode each WriteFiles member asked for, by path — the
	// fake lands no permission bits, so this is where a test reads them.
	modes map[string]fs.FileMode
}

func (f *fakeSandbox) ID() string { return "fake" }
func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.cmds = append(f.cmds, req.Command)
	if f.execHook != nil {
		if res := f.execHook(req); res != nil {
			return *res, nil
		}
	}
	// The harvest's listing: synthesize the NUL-separated relative paths from
	// the in-memory tree (or a forged/truncated listing when a test sets one).
	if req.Command == harvestListScript {
		if f.execTruncated {
			return sandbox.ExecResult{Stdout: f.execStdout, Truncated: true}, nil
		}
		if f.execStdout != "" {
			return sandbox.ExecResult{Stdout: f.execStdout}, nil
		}
		var out strings.Builder
		for p := range f.files {
			if rel, ok := strings.CutPrefix(p, outputsDir+"/"); ok {
				out.WriteString(rel)
				out.WriteByte(0)
			}
		}
		return sandbox.ExecResult{Stdout: out.String()}, nil
	}
	// The memory sync's listing (memsync.HashTreeCommand) and its deletions
	// (memsync.RemoveCommands), answered from the in-memory tree so the
	// three-phase sync runs without a shell: every regular file under the
	// mount but the marker, digest first, in byte order, NUL-terminated as
	// `sha256sum -z` prints it; an absent directory lists nothing and exits 0,
	// as the command's own `[ -d ]` guard does.
	if mount, ok := hashTreeMount(req.Command); ok {
		var paths []string
		for p := range f.files {
			if strings.HasPrefix(p, mount+"/") && p != mount+"/.anthropic-memory-store" {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		var out strings.Builder
		for _, p := range paths {
			sum := sha256.Sum256([]byte(f.files[p]))
			out.WriteString(hex.EncodeToString(sum[:]) + "  ." + strings.TrimPrefix(p, mount) + "\x00")
		}
		if f.listExit != 0 {
			return sandbox.ExecResult{ExitCode: f.listExit, Stderr: "find: './a': Permission denied\n"}, nil
		}
		return sandbox.ExecResult{Stdout: out.String(), Truncated: f.listTruncated}, nil
	}
	if rest, ok := strings.CutPrefix(req.Command, "[ -d '"); ok && strings.Contains(rest, "; rm -f -- ") {
		// memsync.RemoveCommands: the prelude names the mount, then `rm -f --
		// 'rel'… || exit 1[; rmdir …]`; the named files go, and the fake has
		// no directories to prune.
		mount, list, _ := strings.Cut(rest, "' ] || exit 0; ")
		_, list, _ = strings.Cut(list, "; rm -f -- ")
		list, _, _ = strings.Cut(list, " || exit 1")
		for _, q := range strings.Split(list, " ") {
			p := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(q, "'"), "'"), `'\''`, "'")
			delete(f.files, mount+"/"+p)
		}
		return sandbox.ExecResult{}, nil
	}
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

// hashTreeMount recognizes the memory sync's listing command and returns the
// mount it lists.
func hashTreeMount(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, "[ -d '")
	if !ok || !strings.Contains(cmd, "sha256sum -z") {
		return "", false
	}
	mount, _, ok := strings.Cut(rest, "' ] || exit 0; cd -P '")
	return mount, ok
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
	f.writes++
	held := f.gateFrom <= 1 || f.writes >= f.gateFrom
	if f.entered != nil && held {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil && held {
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
	// The seam's own refusal, so the fake cannot be more permissive than the
	// backends it stands in for: a -1 read through the LimitReader below would
	// otherwise land an empty file and report success (#386).
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
	if f.modes == nil {
		f.modes = map[string]fs.FileMode{}
	}
	for _, w := range files {
		f.modes[w.Path] = w.Mode
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
	lastSpec     sandbox.Spec // captured for env-injection assertions
	// entered/gate mirror fakeSandbox's, for a test that holds provisioning open
	// (a slow image pull) to observe the lease keeper renew across it.
	entered chan struct{}
	gate    chan struct{}
	// owned/reaped drive and record the reaper (reaper_test.go); mu guards
	// them because Run's reap loop is a separate goroutine. reapFailFor makes
	// one session's Reap fail, for the error-isolation row. destroyed tracks
	// the sandboxes currently gone — Reap adds, Provision removes — so Export
	// after a reap fails the way both real backends do (a removed container
	// or deleted pod answers ErrNotFound), which is what pins the tier's
	// capture-BEFORE-destroy ordering under test.
	mu    sync.Mutex
	owned []domain.ID
	// attached records every session Attach was asked about, and attachErr and
	// running drive its answer: running is the set this endpoint holds a live
	// sandbox for, so a test can be a session that has one without provisioning.
	attached    []domain.ID
	attachErr   error
	running     map[domain.ID]bool
	reaped      []domain.ID
	destroyed   map[domain.ID]bool
	reapFailFor domain.ID
	// exports feeds Export: root path → tar bytes (checkpoint tests); calls
	// records provision/reap ordering for the restore-replaces-first rule.
	// exportTrailErr, when set, arrives only AFTER the tar bytes — the shape
	// of a K8s tar that exits non-zero behind a complete-looking archive.
	exports        map[string][]byte
	exportErr      error
	exportTrailErr error
	calls          []string
}

func (p *fakeProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	p.provisions++
	p.lastSpec = spec
	p.mu.Lock()
	p.calls = append(p.calls, "provision")
	delete(p.destroyed, spec.SessionID)
	// A provisioned session has a running sandbox, so Attach finds one — the
	// fake's lifecycle must not say the opposite of a real provider's, or a
	// test that provisions and then spills would exercise a state production
	// never reaches.
	if p.provisionErr == nil {
		if p.running == nil {
			p.running = map[domain.ID]bool{}
		}
		p.running[spec.SessionID] = true
	}
	p.mu.Unlock()
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

func (p *fakeProvider) Owned(context.Context) ([]domain.ID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.owned), nil
}

// Attach is Provision's read-only half: it creates nothing, so a session this
// fixture was not told is running answers ErrNotFound however many times it is
// asked.
func (p *fakeProvider) Attach(_ context.Context, sid domain.ID) (sandbox.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attached = append(p.attached, sid)
	if p.attachErr != nil {
		return nil, p.attachErr
	}
	if !p.running[sid] {
		return nil, sandbox.ErrNotFound
	}
	return p.sb, nil
}

// attachCount is how many times Attach was asked about any session, under the
// lock the reaper goroutine shares.
func (p *fakeProvider) attachCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.attached)
}

func (p *fakeProvider) Reap(_ context.Context, sid domain.ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "reap")
	delete(p.running, sid)
	if sid == p.reapFailFor && sid != "" {
		return errors.New("daemon unreachable")
	}
	p.reaped = append(p.reaped, sid)
	if p.destroyed == nil {
		p.destroyed = map[domain.ID]bool{}
	}
	p.destroyed[sid] = true
	return nil
}

// Export answers with the canned per-root tars in exports (keyed by root
// path), sandbox.ErrFileNotExist for a root without one — the normal shape
// for a session that never used a root — and ErrNotFound when the provider
// holds nothing for the session, including one whose sandbox a Reap just
// destroyed and no Provision has replaced.
func (p *fakeProvider) Export(_ context.Context, sid domain.ID, root string) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exportErr != nil {
		return nil, p.exportErr
	}
	if !slices.Contains(p.owned, sid) || p.destroyed[sid] {
		return nil, sandbox.ErrNotFound
	}
	data, ok := p.exports[root]
	if !ok {
		return nil, sandbox.ErrFileNotExist
	}
	r := io.Reader(bytes.NewReader(data))
	if p.exportTrailErr != nil {
		r = io.MultiReader(r, errReader{p.exportTrailErr})
	}
	return io.NopCloser(r), nil
}

// errReader answers every Read with its error — the tail of a stream that
// died after delivering complete-looking bytes.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// reapedSnapshot reads the reap record under the mutex — Run's reaper
// goroutine appends concurrently with a polling test.
func (p *fakeProvider) reapedSnapshot() []domain.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reaped)
}

type harness struct {
	pool   *pgxpool.Pool
	log    *events.Log
	queue  *queue.Queue
	exec   *Executor
	prov   *fakeProvider
	blobs  *blobtest.MemStore
	cipher secrets.Cipher
	sid    domain.ID
	envID  domain.ID
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
	pgtest.SetSessionStatus(t, pool, sid, "running")
	// A real (local AES-GCM) cipher under a fixed test key, so the repository
	// clone path exercises the sealed-token decrypt end to end — the api
	// harness's twin.
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	h := &harness{
		pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		blobs: blobtest.Mem(), cipher: cipher, sid: sid, envID: envID,
	}
	h.exec = New(pool, h.log, h.queue, provider, h.blobs, cipher, cfg)
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

// shutdownRaceCtx reproduces the #282 race deterministically: cancellation
// lands between Run's loop-top liveness check and its inspection of the step
// error — the window where a claim surfaces a transport-level failure instead
// of context.Canceled. Err reports live exactly once (Run's loop-top check is
// its first caller) and cancelled ever after; Done stays open so the claim
// fails on the closed pool, not on the context.
type shutdownRaceCtx struct {
	done chan struct{}
	errs atomic.Int32
}

func (c *shutdownRaceCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *shutdownRaceCtx) Done() <-chan struct{}       { return c.done }
func (c *shutdownRaceCtx) Value(any) any               { return nil }
func (c *shutdownRaceCtx) Err() error {
	if c.errs.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestRunTreatsClaimErrorDuringShutdownAsCleanStop(t *testing.T) {
	// A claim racing shutdown can fail with a transport-level error from the
	// dying connection — not context.Canceled — and Run must still stop clean
	// (#282). The closed pool stands in for the dropped connection.
	h := newHarness(t, nil)
	h.pool.Close()
	if err := h.exec.Run(&shutdownRaceCtx{done: make(chan struct{})}); err != nil {
		t.Errorf("Run returned %v, want nil when shutdown races the claim failure", err)
	}
}

func TestRunReturnsClaimErrorWhileLive(t *testing.T) {
	// The shutdown tolerance must not swallow a genuine claim failure: with the
	// context live, a dead queue is still fatal to the loop.
	h := newHarness(t, nil)
	h.pool.Close()
	if err := h.exec.Run(context.Background()); err == nil {
		t.Error("Run returned nil, want the claim error while the context is live")
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

func TestStalledToolReleasesTheItemForReclaim(t *testing.T) {
	// The wedge #383 is about: a sandbox call that never returns leaves this
	// executor blocked, the row untouched and the lease renewing forever, so the
	// documented recovery — the lease lapses, another executor reclaims — never
	// fires, because nothing crashed. A run that reports no progress for its
	// stall budget is cancelled and its lease left to lapse: nothing commits, and
	// the item stays live for the reclaim.
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	// A second of budget, not a few hundred milliseconds: the clock starts before
	// the session load and the provision, so a tight budget could cancel the run
	// before the gated tool is ever entered — and then the receive below would
	// block until go test's package alarm, which is the very failure this change
	// exists to remove (#318).
	h := newHarnessWith(t, &fakeProvider{sb: sb},
		Config{LeaseTTL: 1500 * time.Millisecond, StallTimeout: time.Second})
	var faultErr error
	h.exec.onFault = func(_ *queue.Item, err error) { faultErr = err }
	h.suspend(t, writeUse("out.txt", "hi"))

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	select {
	case <-sb.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated tool was never entered")
	}
	// Without the guard the gated tool holds the run — and the lease — forever.
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the wedged run was never given up")
	}

	if !errors.Is(faultErr, queue.ErrWorkStalled) {
		t.Errorf("fault = %v, want ErrWorkStalled", faultErr)
	}
	// And it says which tool wedged. "The item stalled" alone sends an operator
	// to the whole session; the tool fault carries the name and the tool_use id,
	// and this is the only place either is written down.
	if faultErr == nil || !strings.Contains(faultErr.Error(), "tool write") {
		t.Errorf("fault = %v, want the wedged tool named alongside the stall", faultErr)
	}
	if got := len(h.types(t, "agent.tool_result")); got != 0 {
		t.Errorf("results = %d, want 0 (the wedged tool never answered)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1 (a stalled item is left for reclaim, not completed)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn = %d, want 0 (a stalled run resumes nothing)", got)
	}
	close(sb.gate) // release, though the tool already returned via cancellation
}

func TestLongButMovingToolRunKeepsItsItem(t *testing.T) {
	// The other half of the guard, and the one that decides whether it can ship:
	// a run that is long overall but keeps finishing steps must never be killed
	// for being long. The budget is 900ms against thirty 50ms tools — one and two
	// thirds of the budget in total, an eighteenth of it per step, so a step can
	// take eighteen times its usual round trip and still report inside the budget.
	// That margin is what keeps this test from becoming the flake class it is
	// defending against. Only the per-tool progress reports keep the run alive.
	//
	// The lease TTL is short on purpose: the keeper checks for a stall on its
	// renewal tick (TTL/3), so a long TTL would let a run with no progress
	// reports finish before the first check and pass this test with the reports
	// removed. At a 1.5s TTL the checks land at 500ms, 1s and 1.5s, and the 900ms
	// budget is what puts the mutation's death on the second of them rather than
	// the third: 1s of silence exceeds it by a clear 100ms, where a 1s budget
	// would be decided by whether the tick landed a hair either side of exactly
	// its own length — and the third check is a dead heat with this run's own
	// 1.5s. So a report-less run dies at ~1s, this one finishes at ~1.5s, and the
	// gap between them is arithmetic rather than luck.
	const tools = 30
	sb := &fakeSandbox{delay: 50 * time.Millisecond}
	h := newHarnessWith(t, &fakeProvider{sb: sb},
		Config{LeaseTTL: 1500 * time.Millisecond, StallTimeout: 900 * time.Millisecond})
	uses := make([]string, tools)
	for i := range uses {
		uses[i] = writeUse(fmt.Sprintf("out%d.txt", i), "hi")
	}
	h.suspend(t, uses...)

	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := len(h.types(t, "agent.tool_result")); got != tools {
		t.Errorf("results = %d, want %d (a moving run must not be cut short)", got, tools)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (the run completed)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (a completed set resumes the turn)", got)
	}
}

// TestStallFaultKeepsBothTheSentinelAndTheCause pins the property that the
// obvious spelling of this fold silently breaks. Choosing between the two with
// cmp.Or looks equivalent and is not: a stall almost always cancels a call in
// flight, so a cause almost always exists, so the sentinel almost always loses —
// and errors.Is(err, queue.ErrWorkStalled), which is how an operator and the
// settling lanes recognise a stall at all, goes quietly false in exactly the
// case it is needed. Every lane that settles a stall folds through here, and the
// worst path — a settlement whose own append fails — is the one where nothing
// else records either fact (#383).
func TestStallFaultKeepsBothTheSentinelAndTheCause(t *testing.T) {
	cause := fmt.Errorf("web tool web_fetch (toolu_1): %w", context.Canceled)
	got := stallFault(queue.ErrWorkStalled, cause)
	if !errors.Is(got, queue.ErrWorkStalled) {
		t.Errorf("errors.Is(%v, ErrWorkStalled) = false, want true (the sentinel is what a stall is matched on)", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false, want true (the cause must survive the fold)", got)
	}
	if !strings.Contains(got.Error(), "toolu_1") {
		t.Errorf("stallFault = %q, want it to name the tool_use the stall cut short", got)
	}
	// Nothing to fold in: setup stalled before a call was reached, and the
	// sentinel must come back unwrapped rather than wrapped around nil.
	if got := stallFault(queue.ErrWorkStalled, nil); !errors.Is(got, queue.ErrWorkStalled) {
		t.Errorf("stallFault(kerr, nil) = %v, want the sentinel itself", got)
	}
}

func TestStalledRunCommitsTheToolsThatAlreadyAnswered(t *testing.T) {
	// Containment must not cost more than the wedge did. A stalled item is
	// released for reclaim, so anything this pass ran and did not commit would be
	// run a second time by the reclaiming pass — and a tool's side effects are
	// already spent the moment it returns (a push, a POST, a file appended). So a
	// stall commits the results that did answer, down the same partial-commit
	// path a backend fault uses: the item stays live, no turn is enqueued while
	// uses are unanswered, and only the wedged tool and the ones behind it are
	// re-derived. A lost lease still commits nothing — there the row belongs to
	// someone else (#383).
	sb := &fakeSandbox{entered: make(chan struct{}, 1), gate: make(chan struct{}), gateFrom: 2}
	h := newHarnessWith(t, &fakeProvider{sb: sb},
		Config{LeaseTTL: 1500 * time.Millisecond, StallTimeout: time.Second})
	var faultErr error
	h.exec.onFault = func(_ *queue.Item, err error) { faultErr = err }
	h.suspend(t, writeUse("first.txt", "one"), writeUse("second.txt", "two"))

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	// The first tool answered; the second is held open and never returns.
	select {
	case <-sb.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the second tool was never entered")
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the wedged run was never given up")
	}

	if !errors.Is(faultErr, queue.ErrWorkStalled) {
		t.Errorf("fault = %v, want ErrWorkStalled", faultErr)
	}
	if got := len(h.types(t, "agent.tool_result")); got != 1 {
		t.Errorf("results = %d, want 1 (the tool that answered before the stall must not be re-run)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1 (a stalled item is left for reclaim)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn = %d, want 0 (a tool is still unanswered)", got)
	}
	close(sb.gate) // release, though the tool already returned via cancellation
}

func TestMaterializationReportsProgressPerItem(t *testing.T) {
	// Each materialization pass reports per item, not once per pass — the
	// difference decides whether the stall bound can be worn (#383). A session
	// may mount eight repositories, each allowed RepoCloneTimeout, and each mount
	// may be 500 MB, so a pass over a legitimate set outlasts any budget a wedge
	// should not. Counted rather than timed, and counted over items that are
	// *skipped* — a dangling skill, a repository this executor cannot clone
	// without a cipher, a file whose row is gone — because a tolerated miss still
	// costs a round trip and still means the run is moving.
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_progressa", "20260101", "alpha", map[string]string{"SKILL.md": "---\nname: alpha\n---\n"})
	h.seedSkill(t, "skill_progressb", "20260101", "beta", map[string]string{"SKILL.md": "---\nname: beta\n---\n"})
	ctx := context.Background()

	var reports int
	count := func() { reports++ }
	// Three references, the third dangling. Resolution is its own loop, and the
	// dangling one leaves it by the skip path without ever reaching the write
	// loop — so a per-write report alone would count a set of dangling skills as
	// silence, however many round trips resolving them took.
	h.exec.materializeSkills(ctx, sb, h.sid, []skillRef{
		{SkillID: "skill_progressa", Version: "20260101"},
		{SkillID: "skill_progressb", Version: "20260101"},
		{SkillID: "skill_progressgone", Version: "20260101"},
	}, count)
	if reports != 7 {
		t.Errorf("skills reports = %d, want 7 (3 resolved + 1 sentinel decision + 2 written + the pass boundary)", reports)
	}

	// The same set again: the sentinel now matches, so the pass returns without
	// entering the write loop. The probe is a sandbox read per recorded tree —
	// the one step of this lane a large unchanged set spends real time on — so
	// those reads report as they land. (The skip itself does not report: the
	// caller reports the moment this returns.)
	reports = 0
	h.exec.materializeSkills(ctx, sb, h.sid, []skillRef{
		{SkillID: "skill_progressa", Version: "20260101"},
		{SkillID: "skill_progressb", Version: "20260101"},
	}, count)
	if reports != 4 {
		t.Errorf("unchanged-set reports = %d, want 4 (2 resolved + 2 sentinel reads)", reports)
	}

	reports = 0
	h.exec.materializeRepos(ctx, sb, h.sid, []repoRef{
		{Type: "github_repository", ID: "sesrsc_a", URL: "https://github.com/o/a", MountPath: "/workspace/a"},
		{Type: "github_repository", ID: "sesrsc_b", URL: "https://github.com/o/b", MountPath: "/workspace/b"},
		{Type: "github_repository", ID: "sesrsc_c", URL: "https://github.com/o/c", MountPath: "/workspace/c"},
	}, count)
	if reports != 6 {
		t.Errorf("repos reports = %d, want 6 (one per repository, refused ones included, plus one before each clone)", reports)
	}

	reports = 0
	h.exec.materializeFiles(ctx, sb, h.sid, []fileRef{
		{Type: "file", FileID: "file_gone1", MountPath: "/mnt/session/uploads/a"},
		{Type: "file", FileID: "file_gone2", MountPath: "/mnt/session/uploads/b"},
	}, count)
	if reports != 3 {
		t.Errorf("files reports = %d, want 3 (one per mount, dangling ones included, plus the pass boundary)", reports)
	}

	// And the boundary is not decoration: the last mount landing and the sentinel
	// write behind it are two steps. A 500 MB mount is well inside the budget and
	// so is a slow sandbox write; together they need not be, and a pass cancelled
	// between them writes no sentinel, so every reclaim repeats the whole
	// materialization and dies at the same place (#383).
	reports = 0
	h.exec.materializeFiles(ctx, sb, h.sid, []fileRef{
		{Type: "file", FileID: "file_gone1", MountPath: "/mnt/session/uploads/a"},
	}, count)
	if reports != 2 {
		t.Errorf("one-mount reports = %d, want 2 (the mount, then the boundary before the sentinel write)", reports)
	}

	// The unchanged-set path is the files lane's own probe: a marker read, then
	// one exec that tests every mount. It returns without entering the write
	// loop, so the read has to report before the exec rather than the two
	// counting as one silent step — a session with hundreds of mounts spends the
	// whole probe there (#383).
	h.seedFile(t, "file_probe", "mounted")
	mounted := []fileRef{{Type: "file", FileID: "file_probe", MountPath: "/mnt/session/uploads/probe"}}
	h.exec.materializeFiles(ctx, sb, h.sid, mounted, count)
	reports = 0
	h.exec.materializeFiles(ctx, sb, h.sid, mounted, count)
	if reports != 1 {
		t.Errorf("unchanged-mount reports = %d, want 1 (the marker read, before the presence exec)", reports)
	}
}

func TestProvisioningReportsBetweenItsSteps(t *testing.T) {
	// Provisioning is the run's longest stretch of not-a-tool-call — resolving
	// credentials, waiting out another goroutine's checkpoint capture on the
	// session lock, then the pull — and the budget clears one silent step at a
	// time. Reporting only on return would make the whole stretch one interval
	// and put the largest healthy pause of all under the wedge bound (#383).
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	var reports int
	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}},
		func() { reports++ }); err != nil {
		t.Fatalf("provisionSandbox: %v", err)
	}
	if reports != 3 {
		t.Errorf("provision reports = %d, want 3 (credentials resolved, session lock taken, sandbox up)", reports)
	}
}

func TestAStallWithNothingLeftUnansweredCompletesTheItem(t *testing.T) {
	// A stall can land on a pass with nothing left to answer — here a redundant
	// reclaim wedged in provisioning, whose session's tools another pass already
	// committed. Leaving that item live would be worse than untidy: the
	// model_turn the settlement schedules would find its own follow-on tool_exec
	// swallowed by the live-item dedupe, and the session would sit still until
	// the abandoned lease lapsed. Nothing is unanswered, so there is nothing for
	// a reclaim to do: the item is finished.
	prov := &fakeProvider{sb: &fakeSandbox{}, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newHarnessWith(t, prov, Config{LeaseTTL: 1500 * time.Millisecond, StallTimeout: time.Second})
	var faultErr error
	h.exec.onFault = func(_ *queue.Item, err error) { faultErr = err }
	h.suspend(t) // an item, no unanswered uses

	done := make(chan struct{})
	go func() { _, _ = h.exec.step(context.Background()); close(done) }()

	select {
	case <-prov.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("provisioning was never entered")
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the wedged provision was never given up")
	}

	if !errors.Is(faultErr, queue.ErrWorkStalled) {
		t.Errorf("fault = %v, want ErrWorkStalled", faultErr)
	}
	if faultErr == nil || !strings.Contains(faultErr.Error(), "provision sandbox") {
		t.Errorf("fault = %v, want the provisioning error kept alongside the stall", faultErr)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (nothing was left unanswered, so the item is done)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (every use is answered, so the turn resumes)", got)
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

func TestEmptyToolResultPostsPlaceholder(t *testing.T) {
	// A read of an empty file yields empty output. It must be the reference
	// runner's "(no output)" text block (since v1.63.1), never a text block with an
	// empty string — a Messages endpoint rejects an empty text block, which
	// would wedge the session on every resume.
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
	if len(body.Content) != 1 || body.Content[0]["type"] != "text" || body.Content[0]["text"] != toolset.NoOutput {
		t.Errorf("content = %v, want one text block %q", body.Content, toolset.NoOutput)
	}
}

// A tool result carrying a NUL byte must still answer and resume the turn:
// Postgres's jsonb cannot store \u0000, so before the toolset boundary
// sanitized it, the append faulted and the item reclaim-looped forever,
// re-running the same command into the same failure (#223).
func TestNULToolOutputStillAnswersAndResumes(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{"/workspace/nul.dat": "a\x00b"}}
	h := newHarness(t, sb)
	uses := h.suspend(t, readUse("nul.dat"))

	worked, err := h.exec.step(context.Background())
	if err != nil || !worked {
		t.Fatalf("step = %v, %v", worked, err)
	}

	results := h.toolResults(t)
	if len(results) != 1 || results[0].IsError || results[0].ToolUseID != uses[0].ID.String() {
		t.Fatalf("results = %+v, want one non-error answer to the read", results)
	}
	if len(results[0].Content) != 1 || results[0].Content[0].Text != "ab" {
		t.Errorf("content = %+v, want one text block %q", results[0].Content, "ab")
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 - a NUL result must not reclaim-loop", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}
