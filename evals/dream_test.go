package evals

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// The dream tier (plan 41 §7): the pipeline driven end to end against a real
// model and real containers, which is the only place its four stages, its
// fan-out and its redaction are all exercised at once.
//
// Two tests, and they measure different things. TestDreamPipeline is the
// seeded case — a store whose flaws are known, two transcripts, and graders
// that read the output store back through the public routes. TestDreamPipeline
// Hundred is the bound: a hundred transcripts, one dream, and the digest-thread
// count the batching promises. Neither is a Task: they run beside TestEvals
// rather than inside it, so the pinned task set and the two documents that
// spell its size are untouched.
//
// The runner is not part of newStack: the control plane starts it (cmd/
// controlplane/main.go) and only a dream test needs it, so each test starts
// its own over the stack's pool, blob store and cipher — the same three the
// binary hands it.

const (
	// dreamTick paces the runner's sweep, and is therefore a dream's start
	// latency. Two seconds rather than the binary's thirty: a test that waits
	// half a minute for the first tick is measuring the ticker.
	dreamTick = 2 * time.Second
	// The two DREAM_TIMEOUTs — the budget from creation, after which the runner
	// settles the dream as failed{timeout}. Both are well under the binary's
	// two hours, and each sits *below* its test's own poll deadline, which is
	// the ordering that makes a stuck dream report itself: the runner reaches
	// its budget first and writes failed{timeout} with a message, where the
	// poll reaching its deadline first would only produce a test failure
	// saying the dream was still running. The seeded run finishes in about two
	// minutes and the hundred in about eleven, so both budgets are slack, not
	// a schedule.
	dreamRunnerTimeout        = 15 * time.Minute
	dreamRunnerTimeoutHundred = 40 * time.Minute
	// dreamMaxInputBytes is cmd/controlplane's DREAM_MAX_INPUT_BYTES default,
	// spelled here because the runner takes it as configuration and no default
	// lives in the package under test.
	dreamMaxInputBytes = 64 << 20

	// dreamPoll is the retrieve interval while a dream runs. The route is a
	// single indexed read, so this is about not flooding the log, not cost.
	dreamPoll = 5 * time.Second
	// dreamMemoryPage is the memory list's page size. view=full is silently
	// capped at 20 (memoryFullViewLimit), so asking for more would only make
	// the paging untested.
	dreamMemoryPage = 20
)

// startDreamRunner runs one dream runner against the stack for the life of the
// test. The cancel is registered after newStack's own cleanups, so LIFO stops
// the runner FIRST: a runner ticking after the brain and executor loops have
// gone would post a stage message nothing can answer, and one ticking after
// the pool closes would log an error per tick on the way out.
func startDreamRunner(t *testing.T, s *stack, cfg api.DreamRunnerConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.StartDreamRunner(ctx, s.pool, s.blobs, s.cipher, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("the dream runner did not stop within 30s of cancellation")
		}
	})
}

// dreamRunnerConfig is what both tests start the runner with.
func dreamRunnerConfig(timeout time.Duration) api.DreamRunnerConfig {
	return api.DreamRunnerConfig{
		TickInterval:  dreamTick,
		Timeout:       timeout,
		MaxInputBytes: dreamMaxInputBytes,
	}
}

// dreamAgentBody is the agent every input session in these tests runs on: the
// bare toolset, unattended, on the stack's model. Nothing here is the
// pipeline's own agent — that one is the platform's hidden row, created by the
// first dream and never visible to a client.
func dreamAgentBody(model, name string) map[string]any {
	return map[string]any{
		"name":  name,
		"model": model,
		"tools": []any{map[string]any{
			"type": "agent_toolset_20260401",
			"default_config": map[string]any{
				"enabled":           true,
				"permission_policy": map[string]any{"type": "always_allow"},
			},
		}},
	}
}

// oneTurn creates a session, sends one user message and waits out the turn,
// returning the session id. The stream is opened before the message for the
// reason openStream documents: it is a live tail with no cursor.
//
// Safe to call from a goroutine, which the hundred-transcript test does: every
// value it touches is this session's own, and the two it shares — the stack's
// session list and testing's own bookkeeping — are mutex-guarded. A t.Fatalf
// reached from a worker goroutine marks the test failed and Goexits only that
// goroutine; the caller's `defer wg.Done()` still runs (Goexit runs defers),
// and the wave's t.Failed() check is what stops the run before it dreams over
// a half-built input set.
func (s *stack) oneTurn(t *testing.T, agentID, envID, message string) string {
	t.Helper()
	sessionID := s.createSession(t, agentID, envID, nil)
	stream := s.openStream(t, sessionID)
	s.sendEvents(t, sessionID, userMessage(message))
	if _, err := stream.awaitIdle(turnTimeout); err != nil {
		t.Fatalf("session %s never went idle: %v", sessionID, err)
	}
	return sessionID
}

// createDream posts one dream over a store and a list of sessions, and returns
// the create response.
func (s *stack) createDream(t *testing.T, storeID string, sessionIDs []string, instructions string) map[string]any {
	t.Helper()
	return s.createDreamWith(t, storeID, sessionIDs, instructions, nil)
}

// createDreamWith is createDream plus an explicit output_behavior, which is the
// only thing an in-place dream's body says differently. A nil behavior sends no
// key at all rather than an explicit create_new, so the default path stays the
// one the other tests exercise.
func (s *stack) createDreamWith(t *testing.T, storeID string, sessionIDs []string, instructions string, behavior map[string]any) map[string]any {
	t.Helper()
	ids := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		ids[i] = id
	}
	body := map[string]any{
		"inputs": []any{
			map[string]any{"type": "memory_store", "memory_store_id": storeID},
			map[string]any{"type": "sessions", "session_ids": ids},
		},
		"model":        s.model,
		"instructions": instructions,
	}
	if behavior != nil {
		body["output_behavior"] = behavior
	}
	return s.do(t, http.MethodPost, "/v1/dreams", body)
}

// awaitDream polls the retrieve route until the dream is terminal, and returns
// the body it saw there. A dream that is still pending or running at the
// deadline fails the test with the status it was stuck in — the runner's own
// timeout is longer, so reaching this line means the runner is not settling.
func (s *stack) awaitDream(t *testing.T, dreamID string, deadline time.Duration) map[string]any {
	t.Helper()
	stop := time.Now().Add(deadline)
	tracked := ""
	for {
		d := s.do(t, http.MethodGet, "/v1/dreams/"+dreamID, nil)
		// The pipeline session belongs to the runner, not to createSession, so
		// the stack has never heard of it — and the container its first tool
		// call provisions would outlive the run. Registering it the moment the
		// dream names it puts that container on the same post-loop reap every
		// other session's gets (stack.reapAll), whether the dream completes or
		// fails.
		if sessionID, _ := d["session_id"].(string); sessionID != "" && sessionID != tracked {
			tracked = sessionID
			s.mu.Lock()
			s.sessions = append(s.sessions, sessionID)
			s.mu.Unlock()
		}
		status, _ := d["status"].(string)
		switch status {
		case "completed", "failed", "canceled":
			return d
		}
		if time.Now().After(stop) {
			t.Fatalf("dream %s was still %q after %s (session %v)", dreamID, status, deadline, d["session_id"])
		}
		time.Sleep(dreamPoll)
	}
}

// outputStoreID is the store the dream wrote — outputs[0], which the start arm
// commits together with `running`, so a dream that ever ran has one.
func outputStoreID(t *testing.T, dream map[string]any) string {
	t.Helper()
	outputs, _ := dream["outputs"].([]any)
	if len(outputs) == 0 {
		t.Fatalf("dream %v carries no outputs", dream["id"])
	}
	out, _ := outputs[0].(map[string]any)
	storeID, _ := out["memory_store_id"].(string)
	if storeID == "" {
		t.Fatalf("dream output %v names no memory store", outputs[0])
	}
	return storeID
}

// storeMemories reads a whole store as path → content, following next_page.
// The full view is the only one that carries content, and its page cap is
// small enough that a hundred-transcript run's store needs the paging.
func (s *stack) storeMemories(t *testing.T, storeID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	q := url.Values{"view": {"full"}, "limit": {fmt.Sprint(dreamMemoryPage)}}
	for {
		res := s.do(t, http.MethodGet, "/v1/memory_stores/"+storeID+"/memories?"+q.Encode(), nil)
		data, ok := res["data"].([]any)
		if !ok {
			t.Fatalf("memory list has no data array: %v", res)
		}
		for _, m := range data {
			obj, ok := m.(map[string]any)
			if !ok {
				t.Fatalf("memory list entry is not an object: %v", m)
			}
			path, _ := obj["path"].(string)
			content, _ := obj["content"].(string)
			out[path] = content
		}
		next, _ := res["next_page"].(string)
		if next == "" {
			return out
		}
		q.Set("page", next)
	}
}

// childThreads counts a session's child threads — the digest threads, since
// the pipeline session spawns nothing else. The primary thread is on the same
// list and is the one row with a null parent, so it is subtracted by the test
// rather than by the count.
func childThreads(t *testing.T, s *stack, sessionID string) int {
	t.Helper()
	n := 0
	for _, th := range s.listThreads(t, sessionID) {
		if th["parent_thread_id"] != nil {
			n++
		}
	}
	return n
}

// stageSpend is what each stage actually cost, counted the way the runner
// counts it — the model turns settled after that stage's opening user.message
// on the primary thread, every thread's included — but sliced per stage rather
// than cumulatively, so the numbers can be read against internal/api's
// per-stage caps one at a time.
//
// It is logged on every run, passing or failing, because the caps are the one
// number in this pipeline that no reasoning can settle: a stage that dies on
// its cap and a stage that wandered look identical from outside, and only the
// measured spend of a real model tells them apart. The plan's first guess at
// stage 1 was four turns; the first live run spent more than that on the
// manifest alone.
func stageSpend(t *testing.T, s *stack, sessionID string) []int {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
		WITH openers AS (
			SELECT seq, row_number() OVER (ORDER BY seq) AS stage,
			       lead(seq) OVER (ORDER BY seq) AS next_seq
			  FROM events
			 WHERE session_id = $1 AND type = 'user.message' AND thread_id IS NULL)
		SELECT (SELECT count(*) FROM events e
		         WHERE e.session_id = $1 AND e.type = 'span.model_request_end'
		           AND e.seq > o.seq AND (o.next_seq IS NULL OR e.seq < o.next_seq))
		  FROM openers o ORDER BY o.stage`, sessionID)
	if err != nil {
		t.Fatalf("read the stages' turn counts: %v", err)
	}
	defer rows.Close()
	var spend []int
	for rows.Next() {
		var turns int
		if err := rows.Scan(&turns); err != nil {
			t.Fatalf("scan a stage's turn count: %v", err)
		}
		spend = append(spend, turns)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stages' turn counts: %v", err)
	}
	return spend
}

// dreamUsage is the dream's four counters, for the log line and the
// non-zero assertion.
func dreamUsage(t *testing.T, dream map[string]any) (input, output float64) {
	t.Helper()
	usage, ok := dream["usage"].(map[string]any)
	if !ok {
		t.Fatalf("dream %v carries no usage object", dream["id"])
	}
	input, _ = usage["input_tokens"].(float64)
	output, _ = usage["output_tokens"].(float64)
	return input, output
}

// mentionsOutsideIndex is mentions with the store's own index left out, for
// the questions that ask what a memory *states* rather than what appears
// anywhere in the store. /MEMORY.md describes every memory it points at, so it
// mentions whatever they do.
func mentionsOutsideIndex(memories map[string]string, needle string) []string {
	var paths []string
	for _, path := range mentions(memories, needle) {
		if path != "/MEMORY.md" {
			paths = append(paths, path)
		}
	}
	return paths
}

// mentions counts the memories whose content contains needle, case-folded.
func mentions(memories map[string]string, needle string) []string {
	var paths []string
	for path, content := range memories {
		if strings.Contains(strings.ToLower(content), strings.ToLower(needle)) {
			paths = append(paths, path)
		}
	}
	return paths
}

// TestDreamPipeline is the seeded case (plan 41 §7): a store with a stale
// fact, a duplicated preference and a memory nothing touches; two transcripts,
// one carrying durable signal and a planted credential, one carrying nothing.
// The graders read the output store back through the public routes and ask
// what the consolidation did to each.
//
// The gate is modeltest.Endpoint, TestEvals's: an opted-in run with a rotted
// .env fails here rather than skipping.
func TestDreamPipeline(t *testing.T) {
	cfg := modeltest.Endpoint(t, modeltest.EvalsEnv)
	s := newStack(t, cfg)
	startDreamRunner(t, s, dreamRunnerConfig(dreamRunnerTimeout))

	// The harness's two per-run tokens, so the planted credential is unique to
	// this run — a fixed string could be matched by a store some earlier run
	// left behind, or by the model repeating a value it had seen elsewhere.
	tr := &Trial{Nonce: newNonce(t), Recall: newNonce(t)}
	secret := tr.fill("sk-live-{{RECALL}}")

	// The seed. Two facts and two preferences: one fact a transcript
	// contradicts, one fact nothing mentions, and one preference stated twice
	// in two files.
	const untouchedPath = "/facts/build.md"
	seed := &MemoryFixture{
		Name: "dream-eval-input",
		Memories: map[string]string{
			"/facts/deploy-target.md": "# Deploy target\n\n" +
				"Validated fact: Production deploys to eu-west-1. All releases go there.\n",
			"/prefs/indent.md": "# Indentation\n\n" +
				"Explicit preference: the user wants tabs, not spaces.\n",
			"/prefs/style.md": "# Editor style\n\n" +
				"Explicit preference: the user wants tabs rather than spaces for indentation.\n",
			untouchedPath: "# Build\n\n" +
				"Validated fact: the build runs `make verify`, which takes about nine minutes.\n",
		},
	}
	storeID := s.createMemoryStore(t, seed, tr)
	before := s.storeMemories(t, storeID)
	if len(before) != len(seed.Memories) {
		t.Fatalf("seeded %d memories, the store lists %d", len(seed.Memories), len(before))
	}

	agentID := s.createAgent(t, dreamAgentBody(s.model, "dream-eval"))
	envID := s.createEnvironment(t, "dream-eval")

	// Session A: durable signal (a newer deploy target), a planted credential
	// the renderer must redact before the pipeline ever sees it, and one tool
	// call so the transcript carries a tool_use/tool_result pair too.
	sessionA := s.oneTurn(t, agentID, envID, fmt.Sprintf(
		"Note for the future: production now deploys to us-east-2 as of 2026-09-01, "+
			"and our vendor key is %s — never share it. Also run `echo deploy-check-ok`.", secret))
	// Session B: NO SIGNAL — a question whose answer is durable to nobody.
	sessionB := s.oneTurn(t, agentID, envID, "What is 2 + 2? Reply with the number only.")

	start := time.Now()
	created := s.createDream(t, storeID, []string{sessionA, sessionB},
		"Prefer the newest evidence; keep the user's own wording.")
	dreamID := id(t, created, "create dream")
	dream := s.awaitDream(t, dreamID, 25*time.Minute)
	elapsed := time.Since(start)

	in, out := dreamUsage(t, dream)
	t.Logf("dream %s: status %v in %s — usage %.0f in / %.0f out",
		dreamID, dream["status"], elapsed.Round(time.Second), in, out)
	if sid, _ := dream["session_id"].(string); sid != "" {
		t.Logf("stage spend (model turns, every thread's): %v", stageSpend(t, s, sid))
	}
	if dream["status"] != "completed" {
		t.Fatalf("dream %s ended %v: %v (pipeline session %v)",
			dreamID, dream["status"], dream["error"], dream["session_id"])
	}
	if in == 0 && out == 0 {
		t.Errorf("dream %s reports no usage at all: %v", dreamID, dream["usage"])
	}

	after := s.storeMemories(t, outputStoreID(t, dream))
	t.Logf("output store holds %d memories: %v", len(after), memoryPaths(after))

	// The stale fact is replaced. A memory that records the move (both regions
	// in one file) is a correct history line, not a stale fact — what fails is
	// a file still presenting eu-west-1 alone as where production deploys.
	// The index is excluded here for the opposite reason it is excluded below:
	// there, a line about the surviving memory must not count as a second
	// statement; here, a line about a memory that no longer exists must not
	// stand in for the fact itself.
	if len(mentionsOutsideIndex(after, "us-east-2")) == 0 {
		t.Errorf("no memory states us-east-2, the newer deploy target: %v", after)
	}
	for _, path := range mentions(after, "eu-west-1") {
		if !strings.Contains(after[path], "us-east-2") {
			t.Errorf("memory %s still states eu-west-1 with no mention of the move: %q", path, after[path])
		}
	}

	// The duplicated preference is merged: one memory states it, not two. The
	// index is not one of them — /MEMORY.md names what every memory holds, so
	// a line pointing at the surviving preference mentions it by construction,
	// and counting that line as a second statement would fail a store that
	// merged correctly. (The credential check below deliberately does not make
	// this exception: a secret in the index is a leak like any other.)
	// Exactly one, not "at most one": consolidating the pair by deleting both
	// loses the preference, and a grader that only counts down would call that
	// a pass.
	// The needle is "tab" rather than the fixtures' "tabs" because a merge is
	// free to reword — "tab indentation" states the same preference — and now
	// that zero is a failure, a needle the merge can write around would fail a
	// store that consolidated correctly. Nothing else in this seed invites the
	// word.
	if paths := mentionsOutsideIndex(after, "tab"); len(paths) != 1 {
		t.Errorf("the tab preference is stated in %d memories, want exactly 1: %v", len(paths), paths)
	}

	// The planted credential reaches no memory. The renderer redacts it on the
	// way in and the completion scan would have failed the dream on the way
	// out, so this asserts the pair held — the prefix too, since redaction
	// replaces the whole match and any surviving "sk-live-" is a leak.
	for _, needle := range []string{secret, "sk-live-"} {
		if paths := mentions(after, needle); len(paths) > 0 {
			t.Errorf("the planted credential (%q) reached memories %v", needle, paths)
		}
	}

	// The untouched memory is byte-identical: nothing in either transcript
	// speaks to it, and the merge rules change a file only when a digest
	// contradicts it.
	if after[untouchedPath] != before[untouchedPath] {
		t.Errorf("the untouched memory changed:\n before: %q\n  after: %q",
			before[untouchedPath], after[untouchedPath])
	}

	// NO SIGNAL: the arithmetic transcript produced nothing. The markers are
	// the arithmetic itself rather than the bare word "four": an index line
	// reading "four memories" would false-positive that, and this grader must
	// fail only on a memory that actually recorded the sum.
	for _, marker := range []string{"2 + 2", "2+2", "= 4", "equals 4"} {
		if paths := mentions(after, marker); len(paths) > 0 {
			t.Errorf("the NO SIGNAL transcript left %q in memories %v", marker, paths)
		}
	}

	// The index exists and covers the store: one line per other memory, at
	// least. More lines than memories is fine (a heading, a blank line).
	index, ok := after["/MEMORY.md"]
	if !ok {
		t.Errorf("the output store has no /MEMORY.md index: %v", memoryPaths(after))
	} else {
		if lines := nonEmptyLines(index); lines < len(after)-1 {
			t.Errorf("/MEMORY.md has %d non-empty lines for %d other memories:\n%s",
				lines, len(after)-1, index)
		}
		// Counting lines alone passes an index of the right length that names
		// the wrong files, so every memory is looked for by name. The leading
		// slash is trimmed because the prompt asks for the path and both
		// spellings of it are one: "/prefs/indent.md" and "prefs/indent.md".
		for path := range after {
			if path == "/MEMORY.md" {
				continue
			}
			if !strings.Contains(index, strings.TrimPrefix(path, "/")) {
				t.Errorf("/MEMORY.md does not name the memory %s:\n%s", path, index)
			}
		}
	}

	// The fan-out ran: two transcripts are one batch, so the pipeline session
	// has one digest thread. Spelled as ">= 1" because the batch size is
	// internal/api's dreamDigestBatch (8) and any batching of two transcripts
	// yields one thread.
	sessionID, _ := dream["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("completed dream %s names no session", dreamID)
	}
	if n := childThreads(t, s, sessionID); n < 1 {
		t.Errorf("the pipeline session %s ran %d child threads, want at least 1 digest thread", sessionID, n)
	}

	// The input store is untouched: the default output behavior clones, so
	// every path and every byte the seed wrote is still there.
	input := s.storeMemories(t, storeID)
	if len(input) != len(before) {
		t.Errorf("the input store now holds %d memories, seeded with %d", len(input), len(before))
	}
	for path, content := range before {
		if input[path] != content {
			t.Errorf("the input memory %s changed:\n before: %q\n  after: %q", path, content, input[path])
		}
	}
}

// TestDreamPipelineHundred is the bound (plan 41 §7, §9): a hundred
// transcripts through one dream, which is what the batch size and the
// live-thread cap were sized for. It asserts what the seeded case cannot —
// that a hundred transcripts complete inside the timeout, and that they arrive
// as thirteen digest threads.
//
// It costs real money and real minutes. Measured on the first successful run
// (2026-09-06, claude-haiku-4-5): driving the hundred transcripts took 3m4s and
// the dream itself 10m10s against its 40-minute budget, spending 209,898 input
// and 43,124 output tokens across exactly the thirteen digest threads the batch
// size predicts. docs/HISTORY.md carries the run before it, whose input was
// twice this one's, and what the review found between them.
func TestDreamPipelineHundred(t *testing.T) {
	cfg := modeltest.Endpoint(t, modeltest.EvalsEnv)
	s := newStack(t, cfg)
	startDreamRunner(t, s, dreamRunnerConfig(dreamRunnerTimeoutHundred))

	// 8 is internal/api's dreamDigestBatch, and 13 is ceil(100/8) — the digest
	// threads one wave of a hundred transcripts spawns. Both are spelled here
	// because that var is not readable from this package; a change to it
	// changes this expectation.
	const (
		transcripts = 100
		batch       = 8
		batches     = 13
	)

	tr := &Trial{Nonce: newNonce(t), Recall: newNonce(t)}
	agentID := s.createAgent(t, dreamAgentBody(s.model, "dream-eval-hundred"))
	envID := s.createEnvironment(t, "dream-eval-hundred")

	// The store the dream consolidates into. Empty rather than seeded: this
	// test measures the fan-out and the clock, and the seeded case above is
	// where content is graded.
	storeID := s.createMemoryStore(t, &MemoryFixture{Name: "dream-eval-hundred"}, tr)

	// One turn each, no tool call — so no session provisions a container and
	// the hundred cost a model call apiece. Driven ten at a time: serial waves
	// would spend a hundred turn round-trips end to end, and the brain and
	// executor loops are already the concurrent pair a real deployment runs.
	const wave = 10
	var (
		mu       sync.Mutex
		sessions []string
	)
	drivingStart := time.Now()
	for first := 1; first <= transcripts; first += wave {
		var wg sync.WaitGroup
		for n := first; n < first+wave && n <= transcripts; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sessionID := s.oneTurn(t, agentID, envID, fmt.Sprintf(
					"Remember for later: module %d's build id is BLD-%d-%s.", n, n, tr.Recall))
				mu.Lock()
				defer mu.Unlock()
				sessions = append(sessions, sessionID)
			}()
		}
		wg.Wait()
		// A worker that fataled left its session out of the slice (see
		// oneTurn), so the input set is already wrong; stop here rather than
		// dream over a partial one.
		if t.Failed() {
			t.Fatalf("driving the transcripts failed after %d of %d sessions", len(sessions), transcripts)
		}
	}
	t.Logf("drove %d transcripts in %s", len(sessions), time.Since(drivingStart).Round(time.Second))
	if len(sessions) != transcripts {
		t.Fatalf("drove %d sessions, want %d", len(sessions), transcripts)
	}

	start := time.Now()
	created := s.createDream(t, storeID, sessions,
		"Group what you find by module; keep every build id greppable.")
	dreamID := id(t, created, "create dream")
	// 45 minutes is the test's own patience; the runner's DREAM_TIMEOUT (40
	// minutes) settles the dream as failed{timeout} first, so a stuck run is
	// reported as a dream failure rather than as a test that ran out of time.
	dream := s.awaitDream(t, dreamID, 45*time.Minute)
	elapsed := time.Since(start)

	in, out := dreamUsage(t, dream)
	sessionID, _ := dream["session_id"].(string)
	threads := 0
	if sessionID != "" {
		threads = childThreads(t, s, sessionID)
	}
	t.Logf("dream %s over %d transcripts: status %v in %s — usage %.0f in / %.0f out, %d digest threads",
		dreamID, transcripts, dream["status"], elapsed.Round(time.Second), in, out, threads)

	if dream["status"] != "completed" {
		t.Fatalf("dream %s ended %v: %v (pipeline session %v)",
			dreamID, dream["status"], dream["error"], dream["session_id"])
	}
	if threads != batches {
		t.Errorf("the pipeline session %s ran %d digest threads, want %d (%d transcripts in batches of %d)",
			sessionID, threads, batches, transcripts, batch)
	}
	after := s.storeMemories(t, outputStoreID(t, dream))
	if _, ok := after["/MEMORY.md"]; !ok {
		t.Errorf("the output store has no /MEMORY.md index: %v", memoryPaths(after))
	}
}

// TestDreamPipelineInPlace is the update_existing case (plan 41 §5.3, §7): the
// dream consolidates the caller's own store rather than a clone of it, and the
// session that does it has no `bash`. What that costs is deletion — `write` and
// `edit` create and change, and nothing removes — so a memory the transcripts
// retire has to survive as a tombstone naming its successor, listed for the
// caller to remove through the memories API.
//
// The graders here are the seeded case's inverted where in-place inverts them:
// there the input store must come back byte-identical, here it is the output
// and must have changed.
func TestDreamPipelineInPlace(t *testing.T) {
	cfg := modeltest.Endpoint(t, modeltest.EvalsEnv)
	s := newStack(t, cfg)
	startDreamRunner(t, s, dreamRunnerConfig(dreamRunnerTimeout))

	tr := &Trial{Nonce: newNonce(t), Recall: newNonce(t)}

	// The seed. One memory a transcript retires outright — the vendor portal is
	// gone — and one nothing touches.
	const (
		retiredPath   = "/facts/vendor-portal.md"
		untouchedPath = "/facts/build.md"
	)
	seed := &MemoryFixture{
		Name: "dream-eval-in-place",
		Memories: map[string]string{
			retiredPath: "# Vendor portal\n\n" +
				"Validated fact: expenses are filed through the vendor portal at vendor.example.com.\n",
			untouchedPath: "# Build\n\n" +
				"Validated fact: the build runs `make verify`, which takes about nine minutes.\n",
		},
	}
	storeID := s.createMemoryStore(t, seed, tr)
	before := s.storeMemories(t, storeID)

	agentID := s.createAgent(t, dreamAgentBody(s.model, "dream-eval-in-place"))
	envID := s.createEnvironment(t, "dream-eval-in-place")

	// The transcript that retires it: not a contradiction to weigh, an
	// outright end. The successor is named, because a tombstone that names no
	// successor is just a deletion the caller has to reconstruct.
	sessionA := s.oneTurn(t, agentID, envID,
		"Note for the future: the vendor portal at vendor.example.com was shut down on "+
			"2026-09-01. Expenses are now filed in the internal finance app at finance.internal.")

	start := time.Now()
	created := s.createDreamWith(t, storeID, []string{sessionA},
		"Retire what the transcript ends; keep the user's own wording.",
		map[string]any{"type": "update_existing", "memory_store_id": storeID})
	dreamID := id(t, created, "create dream")
	dream := s.awaitDream(t, dreamID, 25*time.Minute)

	in, out := dreamUsage(t, dream)
	t.Logf("in-place dream %s: status %v in %s — usage %.0f in / %.0f out",
		dreamID, dream["status"], time.Since(start).Round(time.Second), in, out)
	if sid, _ := dream["session_id"].(string); sid != "" {
		t.Logf("stage spend (model turns, every thread's): %v", stageSpend(t, s, sid))
	}
	if dream["status"] != "completed" {
		t.Fatalf("dream %s ended %v: %v (pipeline session %v)",
			dreamID, dream["status"], dream["error"], dream["session_id"])
	}

	// The output IS the input: no clone was written, and outputs[] says so.
	if got := outputStoreID(t, dream); got != storeID {
		t.Fatalf("outputs[] names %s, want the input store %s — an in-place dream cloned", got, storeID)
	}

	after := s.storeMemories(t, storeID)
	t.Logf("the store now holds %d memories: %v", len(after), memoryPaths(after))

	// Nothing was removed, because nothing could be: the retired memory is
	// still a path in the store.
	if _, ok := after[retiredPath]; !ok {
		t.Errorf("%s is gone from an in-place store, where no tool can delete: %v",
			retiredPath, memoryPaths(after))
	}
	// And it is a tombstone rather than the fact it was: the portal no longer
	// reads as where expenses are filed, and the successor is named. Emptying
	// the file is not tombstoning it — that is a deletion this session could
	// not make, spelled a different way — so the empty case is a failure here
	// rather than a case to skip.
	tomb := after[retiredPath]
	switch {
	case strings.TrimSpace(tomb) == "":
		t.Errorf("%s was emptied rather than tombstoned", retiredPath)
	case tomb == before[retiredPath]:
		t.Errorf("%s is unchanged, so the transcript that ended it changed nothing:\n%s",
			retiredPath, tomb)
	case !strings.Contains(strings.ToLower(tomb), "finance"):
		t.Errorf("%s does not name its successor:\n%s", retiredPath, tomb)
	}
	// The successor fact reached the store, in the tombstone or beside it.
	if len(mentionsOutsideIndex(after, "finance.internal")) == 0 {
		t.Errorf("no memory states the new place expenses are filed: %v", memoryPaths(after))
	}
	// The index carries the retirement where the caller will look for it, and
	// says which memory it means. Searching for the word alone would pass an
	// index whose removal section reads "nothing to remove", which is the one
	// thing this run must not produce.
	if index, ok := after["/MEMORY.md"]; !ok {
		t.Errorf("the store has no /MEMORY.md index: %v", memoryPaths(after))
	} else {
		lower := strings.ToLower(index)
		if !strings.Contains(lower, "remove") {
			t.Errorf("/MEMORY.md has no section listing what the caller should remove:\n%s", index)
		}
		if !strings.Contains(lower, strings.TrimPrefix(retiredPath, "/")) {
			t.Errorf("/MEMORY.md does not name %s, the memory it retired:\n%s", retiredPath, index)
		}
	}
	// The memory nothing spoke to is byte-identical, in place as much as in a
	// clone.
	if after[untouchedPath] != before[untouchedPath] {
		t.Errorf("the untouched memory changed:\n before: %q\n  after: %q",
			before[untouchedPath], after[untouchedPath])
	}
}

// memoryPaths is the store's paths alone, for a failure message that should
// not print a whole store's content.
func memoryPaths(memories map[string]string) []string {
	paths := make([]string, 0, len(memories))
	for path := range memories {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// nonEmptyLines counts the lines of an index that say something.
func nonEmptyLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
