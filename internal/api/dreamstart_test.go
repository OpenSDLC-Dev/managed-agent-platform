package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// The start arm (plan 41 §4.2): the claim, the unlocked render, and the write
// transaction that commits `running` only if the row is still `pending`.

// The whole start, asserted where it lands: the clone, the hidden pair, the
// transcript rows and their mounts, and the session's overridden snapshot.
func TestDreamStartLandsEverythingInOneCommit(t *testing.T) {
	s := newTestServer(t)
	storeID := createMemoryStore(t, s, "preferences")
	createMemory(t, s, storeID, "/a.md", "alpha")
	createMemory(t, s, storeID, "/b/c.md", "beta")
	agentID, envID := fixture(t, s)
	first := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	second := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	// A transcript with content, so the render is exercised rather than only
	// the header — and a secret in it, so the redaction is on the path a
	// dream's bytes actually take.
	appendEvent(t, s, first, "user.message",
		`{"content":[{"type":"text","text":"deploy with token sk-abcdefghijklmnopqr"}]}`)

	body := map[string]any{
		"inputs":       dreamInputs(storeID, []any{first, second}),
		"model":        map[string]any{"id": "claude-opus-4-8", "speed": "fast"},
		"instructions": "keep the deployment notes",
	}
	created := createDream(t, s, body)
	dreamID := created["id"].(string)

	tick(t, s)

	d := getDream(t, s, dreamID)
	if d["status"] != "running" || d["session_id"] == nil {
		t.Fatalf("dream is %v with session %v, want running with a session", d["status"], d["session_id"])
	}
	sessionID := d["session_id"].(string)
	if stage, attempts, closedAt := dreamInternals(t, s, dreamID); stage != 1 || attempts != 1 || closedAt != nil {
		t.Errorf("stage/attempts/closed_at = %d/%d/%v, want 1/1/nil", stage, attempts, closedAt)
	}

	// The clone: a distinct name, the description copied, empty metadata, and
	// one `created` version per memory attributed to the pipeline session.
	cloneID := dreamOutputStore(t, s, dreamID)
	if cloneID == storeID {
		t.Fatal("create_new consolidated into the input store")
	}
	var name, description string
	var metadata []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT name, description, metadata FROM memory_stores WHERE id = $1`, cloneID).
		Scan(&name, &description, &metadata); err != nil {
		t.Fatalf("read the clone: %v", err)
	}
	_, token, _ := strings.Cut(dreamID, "_")
	if want := "preferences (dream " + token + ")"; name != want {
		t.Errorf("clone name = %q, want %q", name, want)
	}
	if string(metadata) != "{}" {
		t.Errorf("clone metadata = %s, want {}", metadata)
	}
	if got := clonePaths(t, s, cloneID); len(got) != 2 || got["/a.md"] != "alpha" || got["/b/c.md"] != "beta" {
		t.Errorf("clone holds %v, want the input's two memories verbatim", got)
	}
	assertCloneVersions(t, s, cloneID, sessionID, 2)

	// The hidden pair, written through the create handlers' own insert bodies.
	dreamAgent, dreamEnv := api.DreamInternalIDsForTest()
	assertInternalRow(t, s, "agents", dreamAgent)
	assertInternalRow(t, s, "environments", dreamEnv)
	var versions int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_versions WHERE agent_id = $1 AND version = 1`, dreamAgent).
		Scan(&versions); err != nil {
		t.Fatalf("read agent_versions: %v", err)
	}
	if versions != 1 {
		t.Errorf("agent_versions holds %d version-1 rows for the internal agent, want 1", versions)
	}

	// The transcript rows: their filenames name the dream, their objects are
	// in place, and the redaction ran before the bytes were stored.
	files := dreamFiles(t, s, dreamID)
	if len(files) != 3 {
		t.Fatalf("the dream owns %d files, want two transcripts and INDEX.md", len(files))
	}
	wantNames := []string{
		"dream/" + dreamID + "/1-" + first + ".md",
		"dream/" + dreamID + "/2-" + second + ".md",
		"dream/" + dreamID + "/INDEX.md",
	}
	for i, want := range wantNames {
		if files[i].filename != want {
			t.Errorf("file %d is named %q, want %q", i, files[i].filename, want)
		}
	}
	transcriptBody := blobText(t, s, files[0].id)
	if strings.Contains(transcriptBody, "sk-abcdefghijklmnopqr") {
		t.Error("the stored transcript carries the secret the renderer must redact")
	}
	if !strings.Contains(transcriptBody, "[REDACTED_SECRET]") {
		t.Errorf("the stored transcript shows no redaction marker:\n%s", transcriptBody)
	}
	index := blobText(t, s, files[2].id)
	for _, want := range []string{first, second, "1 · ", "2 · "} {
		if !strings.Contains(index, want) {
			t.Errorf("INDEX.md is missing %q:\n%s", want, index)
		}
	}

	// The session's resources: the store read_write plus one file mount each,
	// short paths under the uploads root.
	mounts := sessionMountPaths(t, s, sessionID)
	for _, want := range []string{
		"/mnt/session/uploads/dream/transcripts/1-" + first + ".md",
		"/mnt/session/uploads/dream/transcripts/2-" + second + ".md",
		"/mnt/session/uploads/dream/INDEX.md",
	} {
		if !mounts[want] {
			t.Errorf("no session resource mounts at %s (have %v)", want, mounts)
		}
	}

	// The overridden snapshot: the dream's model verbatim, the prompt naming
	// the store's own mount, and a roster whose single member is this agent.
	agent := resolvedAgent(t, s, sessionID)
	model := agent["model"].(map[string]any)
	if model["id"] != "claude-opus-4-8" || model["speed"] != "fast" {
		t.Errorf("session model = %v, want the dream's verbatim", model)
	}
	system, _ := agent["system"].(string)
	storeMount := memoryMount(t, s, sessionID)
	if !strings.Contains(system, storeMount) {
		t.Errorf("the system prompt does not name the store mount %s:\n%s", storeMount, system)
	}
	roster, ok := agent["multiagent"].(map[string]any)
	if !ok {
		t.Fatalf("the session's agent carries no roster: %v", agent["multiagent"])
	}
	members := roster["agents"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["id"] != dreamAgent {
		t.Errorf("roster = %v, want the self member naming %s", members, dreamAgent)
	}

	// The stage message the session was born with carries the caller's
	// steering and the transcript count.
	opening := firstUserMessage(t, s, sessionID)
	for _, want := range []string{"2 transcripts", "<steering>", "keep the deployment notes"} {
		if !strings.Contains(opening, want) {
			t.Errorf("the stage message is missing %q:\n%s", want, opening)
		}
	}

	// The rows the start created are the dream's while it runs: the gate the
	// previous slice installed now has something to refuse.
	status, res := s.do(http.MethodDelete, "/v1/files/"+files[0].id, nil)
	if status != http.StatusBadRequest {
		t.Errorf("DELETE a transcript while the dream runs: status %d (%v), want 400", status, res)
	}
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/archive", nil)
	if status != http.StatusBadRequest {
		t.Errorf("archive the pipeline session while the dream runs: status %d (%v), want 400", status, res)
	}
}

// The in-place start (§5.3): step 4 skipped outright — no clone written, the
// caller's own store mounted read_write, and outputs[] naming it.
func TestDreamStartInPlaceConsolidatesTheInputStore(t *testing.T) {
	s := newTestServer(t)
	storeID, body := seededDreamBody(t, s)
	dreamID := createDreamInPlace(t, s, body, storeID)["id"].(string)
	stores, versions := memoryStoreCount(t, s), storeVersionCount(t, s, storeID)

	tick(t, s)

	d := getDream(t, s, dreamID)
	if d["status"] != "running" {
		t.Fatalf("dream is %v, want running (%v)", d["status"], d["error"])
	}
	if got := dreamOutputStore(t, s, dreamID); got != storeID {
		t.Errorf("outputs[0] names %s, want the input store %s", got, storeID)
	}
	if got := memoryStoreCount(t, s); got != stores {
		t.Errorf("the platform holds %d memory stores, want the %d it started with — an in-place dream clones nothing", got, stores)
	}
	if got := storeVersionCount(t, s, storeID); got != versions {
		t.Errorf("the input store carries %d versions, want the %d it started with", got, versions)
	}

	// The session mounts that same store, writable: the consolidation lands in
	// the caller's own memory, which is what update_existing means.
	sessionID := d["session_id"].(string)
	mount := ""
	for _, r := range sessionResources(t, s, sessionID) {
		if r["type"] != "memory_store" {
			continue
		}
		if r["memory_store_id"] != storeID || r["access"] != "read_write" {
			t.Errorf("the session mounts %v %v, want %s read_write", r["memory_store_id"], r["access"], storeID)
		}
		mount = r["mount_path"].(string)
	}
	if mount == "" {
		t.Fatal("the in-place session mounts no memory store")
	}
	if system, _ := resolvedAgent(t, s, sessionID)["system"].(string); !strings.Contains(system, mount) {
		t.Errorf("the system prompt does not name the mounted store %s:\n%s", mount, system)
	}
}

// The toolset half of the same boundary (§4.3): an in-place session is offered
// no bash, so the file tools are the only writers left and the memory they may
// write is the mounted store alone (internal/toolset's unwritable, pinned by
// TestMemoryRootsGuardTheFileTools). The refusal of a call to a name the model
// was not offered is internal/brain's, over this very set.
func TestDreamStartInPlaceRunsWithoutBash(t *testing.T) {
	s := newTestServer(t)
	storeID, body := seededDreamBody(t, s)
	dreamID := createDreamInPlace(t, s, body, storeID)["id"].(string)
	tick(t, s)
	inPlace := getDream(t, s, dreamID)["session_id"].(string)

	_, cloning := startedDream(t, s, body)

	want := []string{"read", "write", "edit", "glob", "grep"}
	own, member := offeredTools(t, s, inPlace)
	if !slices.Equal(own, want) {
		t.Errorf("the in-place session is offered %v, want %v — bash is what update_existing takes away", own, want)
	}
	// The roster's self member is the same resolution, so the digest threads
	// the coordinator spawns inherit the jail rather than escaping it.
	if !slices.Equal(member, want) {
		t.Errorf("the in-place roster's self member is offered %v, want %v", member, want)
	}
	// The create_new side is asserted whole rather than for bash alone, because
	// that is what keeps the two constants honest in both directions: a tool
	// disabled in dreamAgentBody and not mirrored into dreamNoBashTools would
	// drop out here, where a search for bash would never look.
	withBash := append([]string{"bash"}, want...)
	if got, _ := offeredTools(t, s, cloning); !slices.Equal(got, withBash) {
		t.Errorf("a create_new session is offered %v, want %v", got, withBash)
	}
}

// createDreamInPlace creates an in-place dream the way a caller does — an
// update_existing output_behavior naming the body's own memory_store input —
// so the tests above drive the whole chain, create body to start arm, rather
// than writing the column the runner reads and proving only its second half.
// The body is copied because its callers reuse it for a second, cloning dream,
// which the hold would refuse if it inherited the target.
func createDreamInPlace(t *testing.T, s *tserver, body map[string]any, storeID string) map[string]any {
	t.Helper()
	in := maps.Clone(body)
	in["output_behavior"] = map[string]any{"type": "update_existing", "memory_store_id": storeID}
	return createDream(t, s, in)
}

func memoryStoreCount(t *testing.T, s *tserver) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memory_stores`).Scan(&n); err != nil {
		t.Fatalf("count memory stores: %v", err)
	}
	return n
}

func storeVersionCount(t *testing.T, s *tserver, storeID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memory_versions WHERE memory_store_id = $1`, storeID).Scan(&n); err != nil {
		t.Fatalf("count versions of %s: %v", storeID, err)
	}
	return n
}

// offeredTools resolves the built-in tools a session's stored snapshot offers,
// through the same toolset.Tools the brain builds a turn's tool list from: the
// session's own agent, and the roster's self member the coordinator's spawned
// threads run on.
func offeredTools(t *testing.T, s *tserver, sessionID string) (own, member []string) {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT resolved_agent FROM sessions WHERE id = $1`, sessionID).Scan(&raw); err != nil {
		t.Fatalf("read resolved_agent: %v", err)
	}
	var snap struct {
		Tools      []json.RawMessage `json:"tools"`
		Multiagent struct {
			Agents []struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"agents"`
		} `json:"multiagent"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode resolved_agent: %v", err)
	}
	if len(snap.Multiagent.Agents) != 1 {
		t.Fatalf("the snapshot carries %d roster members, want the one self member",
			len(snap.Multiagent.Agents))
	}
	return toolNames(t, snap.Tools), toolNames(t, snap.Multiagent.Agents[0].Tools)
}

func toolNames(t *testing.T, tools []json.RawMessage) []string {
	t.Helper()
	if len(tools) != 1 {
		t.Fatalf("the snapshot carries %d tools entries, want the one toolset", len(tools))
	}
	defs, err := toolset.Tools(tools[0])
	if err != nil {
		t.Fatalf("resolve the snapshot's toolset: %v", err)
	}
	var names []string
	for _, def := range defs {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(def, &d); err != nil {
			t.Fatalf("decode a tool definition: %v", err)
		}
		names = append(names, d.Name)
	}
	return names
}

// A second dream on a fresh platform finds the hidden pair rather than writing
// it again — the runner's steady state.
func TestDreamStartReusesTheInternalPair(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	startedDream(t, s, body)
	startedDream(t, s, body)

	dreamAgent, dreamEnv := api.DreamInternalIDsForTest()
	for _, q := range []struct{ table, id string }{{"agents", dreamAgent}, {"environments", dreamEnv}} {
		var n int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM `+q.table+` WHERE id = $1`, q.id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.table, err)
		}
		if n != 1 {
			t.Errorf("%s holds %d rows for %s, want 1", q.table, n, q.id)
		}
	}
}

// The runner's stored agent is the create handler's own normalization: the
// same request body through POST /v1/agents produces the same spec, modulo the
// self member's pinned id.
func TestDreamInternalAgentMatchesTheHandlersOwnWrite(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	startedDream(t, s, body)

	agentBody, envBody := api.DreamInternalBodiesForTest()
	var request map[string]any
	if err := json.Unmarshal([]byte(agentBody), &request); err != nil {
		t.Fatalf("decode the runner's agent body: %v", err)
	}
	public := createAgent(t, s, request)["id"].(string)
	dreamAgent, dreamEnv := api.DreamInternalIDsForTest()

	runnerSpec := storedSpec(t, s, "agents", dreamAgent)
	publicSpec := strings.ReplaceAll(storedSpec(t, s, "agents", public), public, dreamAgent)
	if runnerSpec != publicSpec {
		t.Errorf("the runner's agent spec differs from the handler's own write:\n%s\n%s", runnerSpec, publicSpec)
	}

	var envRequest map[string]any
	if err := json.Unmarshal([]byte(envBody), &envRequest); err != nil {
		t.Fatalf("decode the runner's environment body: %v", err)
	}
	publicEnv := createEnvironment(t, s, envRequest)["id"].(string)
	if got, want := storedConfig(t, s, dreamEnv), storedConfig(t, s, publicEnv); got != want {
		t.Errorf("the runner's environment config differs from the handler's own write:\n%s\n%s", got, want)
	}
}

// The three classified failures settle the dream in one commit — failed,
// ended and closed, with no session and no partial write behind them.
func TestDreamStartClassifiedFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, s *tserver, storeID, dreamID string) api.DreamRunnerConfig
		errType string
	}{
		{
			name: "the input store archived",
			arrange: func(t *testing.T, s *tserver, storeID, _ string) api.DreamRunnerConfig {
				archiveMemoryStore(t, s, storeID)
				return dreamCfg()
			},
			errType: "input_memory_store_unavailable",
		},
		{
			name: "the input store over the byte cap",
			arrange: func(t *testing.T, s *tserver, _, _ string) api.DreamRunnerConfig {
				cfg := dreamCfg()
				cfg.MaxInputBytes = 1
				return cfg
			},
			errType: "input_memory_store_too_large",
		},
		{
			name: "an input session gone",
			arrange: func(t *testing.T, s *tserver, _, dreamID string) api.DreamRunnerConfig {
				var ids []string
				if err := s.pool.QueryRow(context.Background(),
					`SELECT input_session_ids FROM dreams WHERE id = $1`, dreamID).Scan(&ids); err != nil {
					t.Fatalf("read input sessions: %v", err)
				}
				if _, err := s.pool.Exec(context.Background(),
					`DELETE FROM sessions WHERE id = $1`, ids[0]); err != nil {
					t.Fatalf("delete input session: %v", err)
				}
				return dreamCfg()
			},
			errType: "input_session_unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			storeID, body := seededDreamBody(t, s)
			dreamID := createDream(t, s, body)["id"].(string)
			cfg := tc.arrange(t, s, storeID, dreamID)

			tickAt(t, s, dbNow(t, s), cfg)

			d := getDream(t, s, dreamID)
			if got, msg := dreamError(t, d); d["status"] != "failed" || got != tc.errType {
				t.Fatalf("dream is %v/%v/%q, want failed/%s", d["status"], got, msg, tc.errType)
			}
			if d["session_id"] != nil {
				t.Errorf("a classified start failure created a session: %v", d["session_id"])
			}
			if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
				t.Error("a classified start failure did not close the dream in the same commit")
			}
			if n := s.blobs.Len(); n != 0 {
				t.Errorf("%d rendered objects survive a classified failure", n)
			}
		})
	}
}

// An unclassified failure rolls the write back: the claim's attempt stands,
// the objects go, and the lease keeps the dream out of the scan until it ages
// — after which a later tick retries it and succeeds.
func TestDreamStartUnclassifiedRollbackAndTheLease(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	restore := api.SetDreamStartHookAfterRenderForTest(func() error {
		return errors.New("the write could not be reached")
	})
	tick(t, s)
	restore()

	if d := getDream(t, s, dreamID); d["status"] != "pending" {
		t.Fatalf("an unclassified start failure moved the dream to %v", d["status"])
	}
	if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
		t.Errorf("attempts = %d after one failed claim, want 1", attempts)
	}
	if n := s.blobs.Len(); n != 0 {
		t.Errorf("%d rendered objects survive the rollback", n)
	}

	// Inside the lease the dream is nobody's candidate...
	tick(t, s)
	if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
		t.Fatalf("attempts = %d; the claim's soft lease did not hold the dream out of the scan", attempts)
	}
	// ...and past it, a later tick starts it.
	tickAt(t, s, dbNow(t, s).Add(6*time.Minute), dreamCfg())
	if d := getDream(t, s, dreamID); d["status"] != "running" {
		t.Fatalf("the dream is %v once the lease aged out, want running (%v)", d["status"], d["error"])
	}
}

// The claim that exhausts dreamStartAttempts settles the dream with the last
// error's text rather than retrying forever.
func TestDreamStartAttemptsExhausted(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(api.SetDreamStartAttemptsForTest(2))
	t.Cleanup(api.SetDreamStartLeaseForTest(time.Millisecond))
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	restore := api.SetDreamStartHookAfterRenderForTest(func() error {
		return errors.New("the object store answered nothing")
	})
	defer restore()
	tickAt(t, s, dbNow(t, s), dreamCfg())
	tickAt(t, s, dbNow(t, s).Add(time.Second), dreamCfg())

	d := getDream(t, s, dreamID)
	got, msg := dreamError(t, d)
	if d["status"] != "failed" || got != "internal_error" {
		t.Fatalf("dream is %v/%v, want failed/internal_error", d["status"], d["error"])
	}
	if !strings.Contains(msg, "the object store answered nothing") {
		t.Errorf("the settle does not carry the last error's text: %q", msg)
	}
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Error("the exhausted start did not close the dream")
	}
}

// The claim that dies before it can report back is the one settleExhaustedDream
// never sees: every attempt is spent and nothing settled the dream, so the next
// tick past the lease must end it here rather than hand out one claim more.
func TestDreamClaimAtTheCapSettlesInsteadOfClaiming(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(api.SetDreamStartAttemptsForTest(2))
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)
	// The row a claimant that crashed after its last claim leaves behind.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE dreams SET attempts = 2, updated_at = now() - interval '1 hour' WHERE id = $1`,
		dreamID); err != nil {
		t.Fatalf("age the exhausted claim: %v", err)
	}

	tick(t, s)

	d := getDream(t, s, dreamID)
	_, attempts, closedAt := dreamInternals(t, s, dreamID)
	if d["status"] != "failed" {
		t.Fatalf("dream is %v with session %v after %d attempts; a dream at the cap must be "+
			"settled, not claimed again", d["status"], d["session_id"], attempts)
	}
	got, msg := dreamError(t, d)
	if got != "internal_error" {
		t.Fatalf("error.type = %q (%q), want internal_error", got, msg)
	}
	if !strings.Contains(msg, "the last claim did not complete") {
		t.Errorf("the settle does not name the claim that never reported back: %q", msg)
	}
	if d["session_id"] != nil {
		t.Errorf("a dream past its claim cap was started anyway: session %v", d["session_id"])
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want the cap unchanged: the settle claims nothing", attempts)
	}
	if closedAt == nil {
		t.Error("the settle left the dream open; it has no session to wind down")
	}
}

// The candidate list is advisory and the locked re-read decides: a list taken
// before a claim, replayed after it, claims nothing — the claim's own soft
// lease excludes the row on both sides of the tick (§4.1).
func TestDreamStaleCandidateIsRefusedByTheLockedReRead(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	ids, err := api.DreamCandidatesForTest(context.Background(), s.pool, dbNow(t, s))
	if err != nil {
		t.Fatalf("scan candidates: %v", err)
	}
	if len(ids) != 1 || ids[0] != dreamID {
		t.Fatalf("candidates = %v, want the one pending dream", ids)
	}

	// A claim that burns an attempt and rolls its start back leaves the row
	// `pending` — the shape the stale list still names, and the one only the
	// lease keeps out of a second claim.
	restore := api.SetDreamStartHookAfterRenderForTest(func() error {
		return errors.New("the write could not be reached")
	})
	tick(t, s)
	restore()
	if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
		t.Fatalf("attempts = %d after the claim, want 1", attempts)
	}

	if err := api.DreamArmForTest(context.Background(), s.pool, s.blobs, ids[0],
		dbNow(t, s), dreamCfg()); err != nil {
		t.Fatalf("arm on the stale candidate: %v", err)
	}

	if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
		t.Errorf("attempts = %d: the stale candidate was claimed a second time", attempts)
	}
	if d := getDream(t, s, dreamID); d["status"] != "pending" || d["session_id"] != nil {
		t.Errorf("dream is %v with session %v, want the pending row the claim left",
			d["status"], d["session_id"])
	}
}

// A cancel arriving while the start's write transaction holds the row waits at
// the row for lock_timeout and then fails the request (§4.1): a failed request
// is what the SDK retries, and the retry lands against whatever the write left
// — here a running dream, which it cancels.
func TestDreamCancelWhileTheStartWriteHoldsTheRow(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(api.SetDreamLockWaitForTest(250 * time.Millisecond))
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseWrite := func() { once.Do(func() { close(release) }) }
	defer releaseWrite()
	defer api.SetDreamStartHookInWriteForTest(func() {
		close(held)
		<-release
	})()

	now := dbNow(t, s)
	ticked := make(chan error, 1)
	go func() {
		ticked <- api.DreamTickForTest(context.Background(), s.pool, s.blobs, now, dreamCfg())
	}()
	<-held

	canceled := make(chan int, 1)
	go func() {
		status, _ := s.do(http.MethodPost, "/v1/dreams/"+dreamID+"/cancel", nil)
		canceled <- status
	}()
	select {
	case status := <-canceled:
		// 55P03 is unmapped, so the request fails rather than hangs.
		if status != http.StatusInternalServerError {
			t.Errorf("cancel against the held row: status %d, want 500", status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the cancel never returned: it waited on the start's row lock with no bound")
	}

	releaseWrite()
	if err := <-ticked; err != nil {
		t.Fatalf("dream tick: %v", err)
	}
	d := getDream(t, s, dreamID)
	if d["status"] != "running" || d["session_id"] == nil {
		t.Fatalf("the released write left the dream %v with session %v, want running with one",
			d["status"], d["session_id"])
	}
	sessionID := d["session_id"].(string)

	status, res := s.do(http.MethodPost, "/v1/dreams/"+dreamID+"/cancel", nil)
	if status != http.StatusOK || res["status"] != "canceled" {
		t.Fatalf("the retried cancel: status %d (%v)", status, res)
	}
	if !sessionInterrupted(t, s, sessionID) {
		t.Error("the retried cancel did not interrupt the now-running dream's session")
	}
	tick(t, s)
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Error("the closing arm did not finish the canceled dream")
	}
}

// A cancel landing during the unlocked render wins: the write transaction
// finds the dream no longer pending, writes nothing and drops the objects.
func TestDreamCancelDuringTheRender(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	restore := api.SetDreamStartHookAfterRenderForTest(func() error {
		if status, res := s.do(http.MethodPost, "/v1/dreams/"+dreamID+"/cancel", nil); status != http.StatusOK {
			return fmt.Errorf("cancel during the render: status %d (%v)", status, res)
		}
		return nil
	})
	tick(t, s)
	restore()

	d := getDream(t, s, dreamID)
	if d["status"] != "canceled" || d["session_id"] != nil {
		t.Fatalf("dream is %v with session %v, want canceled with none", d["status"], d["session_id"])
	}
	if n := s.blobs.Len(); n != 0 {
		t.Errorf("%d rendered objects survive a cancel during the render", n)
	}
	if len(dreamFileIDs(t, s, dreamID)) != 0 {
		t.Error("the write transaction inserted file rows for a canceled dream")
	}
}

// A replica whose tick runs after another's claim starts nothing: the claim's
// exclusion is one predicate the candidate scan and the locked read both
// carry, so neither a fresh list nor one taken before the claim can re-start
// the dream. A single process cannot say which of the two excluded the row —
// both run under one `now` — so the assertion is the outcome the pair exists
// to guarantee.
func TestDreamStaleCandidateClaimsNothing(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	// The first replica's tick claims and starts it.
	tick(t, s)
	before := getDream(t, s, dreamID)["session_id"]

	tickAt(t, s, dbNow(t, s), dreamCfg())
	if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
		t.Errorf("attempts = %d, want 1: a second replica re-claimed a started dream", attempts)
	}
	if after := getDream(t, s, dreamID)["session_id"]; after != before {
		t.Errorf("session_id moved from %v to %v; the dream was started twice", before, after)
	}
}

// An archived hidden row is neither classified nor retried to exhaustion: the
// runner logs it and the dream stays pending for a human to un-archive.
func TestDreamStartLeavesTheDreamPendingOnAnArchivedInternalRow(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(api.SetDreamStartAttemptsForTest(1))
	t.Cleanup(api.SetDreamStartLeaseForTest(time.Millisecond))
	_, body := seededDreamBody(t, s)
	// One start writes the pair through the handlers' own bodies; then a
	// database operator archives one of them, which nothing in the platform
	// can undo and ON CONFLICT DO NOTHING cannot replace.
	startedDream(t, s, body)
	_, dreamEnv := api.DreamInternalIDsForTest()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE environments SET archived_at = now() WHERE id = $1`, dreamEnv); err != nil {
		t.Fatalf("archive the internal environment: %v", err)
	}

	dreamID := createDream(t, s, body)["id"].(string)
	tickAt(t, s, dbNow(t, s).Add(time.Second), dreamCfg())

	if d := getDream(t, s, dreamID); d["status"] != "pending" {
		t.Fatalf("dream is %v, want pending (%v)", d["status"], d["error"])
	}
	if n := dreamBlobCount(t, s, dreamID); n != 0 {
		t.Errorf("%d rendered objects survive the blocked start", n)
	}
}

// dreamBlobCount counts the objects the store holds beyond the ones the dreams
// already running own, so a case that starts a dream first can still assert
// that a blocked start left nothing behind.
func dreamBlobCount(t *testing.T, s *tserver, dreamID string) int {
	t.Helper()
	live := 0
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE dream_id IS NOT NULL AND dream_id <> $1`,
		dreamID).Scan(&live); err != nil {
		t.Fatalf("count live transcript rows: %v", err)
	}
	return s.blobs.Len() - live
}

// --- readers over what the start wrote ------------------------------------

type dreamFileRow struct{ id, filename string }

func dreamFiles(t *testing.T, s *tserver, dreamID string) []dreamFileRow {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, filename FROM files WHERE dream_id = $1 ORDER BY filename`, dreamID)
	if err != nil {
		t.Fatalf("read dream files: %v", err)
	}
	defer rows.Close()
	var out []dreamFileRow
	for rows.Next() {
		var f dreamFileRow
		if err := rows.Scan(&f.id, &f.filename); err != nil {
			t.Fatalf("scan file row: %v", err)
		}
		out = append(out, f)
	}
	return out
}

func blobText(t *testing.T, s *tserver, fileID string) string {
	t.Helper()
	rc, _, err := s.blobs.Get(context.Background(), blob.FilesKey(fileID))
	if err != nil {
		t.Fatalf("read the object for %s: %v", fileID, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read the object body for %s: %v", fileID, err)
	}
	return string(data)
}

func clonePaths(t *testing.T, s *tserver, storeID string) map[string]string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT path, content FROM memories WHERE memory_store_id = $1`, storeID)
	if err != nil {
		t.Fatalf("read the clone's memories: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			t.Fatalf("scan memory: %v", err)
		}
		out[path] = content
	}
	return out
}

// assertCloneVersions pins the attribution §4.2 step 4 fixes: one `created`
// version per memory, under a session_actor naming the session created after
// it, stamped no later than that session's own row.
func assertCloneVersions(t *testing.T, s *tserver, storeID, sessionID string, want int) {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT operation, created_by::text, created_at FROM memory_versions WHERE memory_store_id = $1`, storeID)
	if err != nil {
		t.Fatalf("read the clone's versions: %v", err)
	}
	defer rows.Close()
	n := 0
	var latest time.Time
	for rows.Next() {
		var operation, actor string
		var createdAt time.Time
		if err := rows.Scan(&operation, &actor, &createdAt); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		n++
		if operation != "created" {
			t.Errorf("clone version operation = %q, want created", operation)
		}
		if !strings.Contains(actor, `"session_actor"`) || !strings.Contains(actor, sessionID) {
			t.Errorf("clone version actor = %s, want a session_actor naming %s", actor, sessionID)
		}
		if createdAt.After(latest) {
			latest = createdAt
		}
	}
	if n != want {
		t.Errorf("the clone carries %d versions, want %d", n, want)
	}
	var sessionCreatedAt time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT created_at FROM sessions WHERE id = $1`, sessionID).Scan(&sessionCreatedAt); err != nil {
		t.Fatalf("read the session's created_at: %v", err)
	}
	if latest.After(sessionCreatedAt) {
		t.Errorf("a cloned version (%s) is later than the session row (%s), so the completion "+
			"scan would read the clone back", latest, sessionCreatedAt)
	}
}

// assertCloneVersionPerMemory pins the pairing the batched insert makes by
// parameter number alone: every cloned memory has exactly one version, that
// version is its head pointer, and the two carry the same path and content.
// A numbering slip inside a batch crosses these without changing any count.
func assertCloneVersionPerMemory(t *testing.T, s *tserver, storeID string, want int) {
	t.Helper()
	var paired int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM memories m
		  JOIN memory_versions v
		    ON v.memory_id = m.id AND v.memory_store_id = m.memory_store_id
		 WHERE m.memory_store_id = $1 AND m.memory_version_id = v.id
		   AND v.operation = 'created' AND v.path = m.path AND v.content = m.content
		   AND v.content_sha256 = m.content_sha256
		   AND v.content_size_bytes = m.content_size_bytes`, storeID).Scan(&paired); err != nil {
		t.Fatalf("join the clone's memories to their versions: %v", err)
	}
	if paired != want {
		t.Errorf("%d cloned memories are paired with their own created version, want %d", paired, want)
	}
}

func assertInternalRow(t *testing.T, s *tserver, table, id string) {
	t.Helper()
	var internal bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT internal FROM `+table+` WHERE id = $1`, id).Scan(&internal); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	if !internal {
		t.Errorf("%s %s is not marked internal", table, id)
	}
}

func storedSpec(t *testing.T, s *tserver, table, id string) string {
	t.Helper()
	var spec string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT spec::text FROM `+table+` WHERE id = $1`, id).Scan(&spec); err != nil {
		t.Fatalf("read %s spec: %v", id, err)
	}
	return spec
}

func storedConfig(t *testing.T, s *tserver, id string) string {
	t.Helper()
	var config string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT config::text FROM environments WHERE id = $1`, id).Scan(&config); err != nil {
		t.Fatalf("read environment config: %v", err)
	}
	return config
}

func sessionResources(t *testing.T, s *tserver, sessionID string) []map[string]any {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT resources FROM sessions WHERE id = $1`, sessionID).Scan(&raw); err != nil {
		t.Fatalf("read session resources: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode session resources: %v", err)
	}
	return out
}

func sessionMountPaths(t *testing.T, s *tserver, sessionID string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range sessionResources(t, s, sessionID) {
		if p, ok := r["mount_path"].(string); ok {
			out[p] = true
		}
	}
	return out
}

// memoryMount is the store resource's own mount — the path the prompt must
// spell, taken from the created session rather than recomputed.
func memoryMount(t *testing.T, s *tserver, sessionID string) string {
	t.Helper()
	for _, r := range sessionResources(t, s, sessionID) {
		if r["type"] == "memory_store" {
			return r["mount_path"].(string)
		}
	}
	t.Fatalf("session %s mounts no memory store", sessionID)
	return ""
}

func resolvedAgent(t *testing.T, s *tserver, sessionID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT resolved_agent FROM sessions WHERE id = $1`, sessionID).Scan(&raw); err != nil {
		t.Fatalf("read resolved_agent: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode resolved_agent: %v", err)
	}
	return out
}

// firstUserMessage is the stage message the session was created with.
func firstUserMessage(t *testing.T, s *tserver, sessionID string) string {
	t.Helper()
	var payload []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT payload FROM events WHERE session_id = $1 AND type = $2 ORDER BY seq LIMIT 1`,
		sessionID, string(domain.EventUserMessage)).Scan(&payload); err != nil {
		t.Fatalf("read the opening user.message: %v", err)
	}
	var p struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || len(p.Content) == 0 {
		t.Fatalf("decode the opening user.message %s: %v", payload, err)
	}
	return p.Content[0].Text
}

// TestDreamRunnerPassesBeforeItsFirstTick: the pass runs before the wait. The
// interval here is an hour, so a dream that starts inside this test was started
// by a pass taken at startup. What a missed first tick costs here is not
// latency: dreamStep measures the timeout from created_at and its timeout arm
// precedes its start arm, so a dream left pending with less than a tick of
// budget is failed as `timeout` by the pass that would otherwise have started
// it (#699).
func TestDreamRunnerPassesBeforeItsFirstTick(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	cfg := dreamCfg()
	cfg.TickInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); api.StartDreamRunner(ctx, s.pool, s.blobs, nil, cfg) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(30 * time.Second)
	for getDream(t, s, dreamID)["status"] != "running" {
		if time.Now().After(deadline) {
			t.Fatal("the loop is waiting out a first tick an hour away, so a dream created just under its timeout is failed unstarted by the pass that follows")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And then it waits. A second dream, created once the first is running, must
	// stay pending until the tick an hour away — a loop that passed without ever
	// waiting would start it too and look identical from the status above.
	_, otherBody := seededDreamBody(t, s)
	other := createDream(t, s, otherBody)["id"].(string)
	time.Sleep(500 * time.Millisecond)
	if got := getDream(t, s, other)["status"]; got != "pending" {
		t.Errorf("a dream created after the startup pass is %v within the interval, want pending: the loop is not waiting between passes", got)
	}
}

// The loop around the tick: it sweeps on its own interval until its context
// ends, and stops when it does.
//
// It takes two dreams to show the interval half. The loop passes once before
// its first wait (#699), so the dream that is pending at startup proves only
// that the loop ran; a loop that passed at boot and then never consumed its
// ticker again would pass with one subject. The second is created once the
// first is running, after the startup pass has scanned, so only a tick reaches
// it.
func TestStartDreamRunnerTicksAndStops(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID := createDream(t, s, body)["id"].(string)

	cfg := dreamCfg()
	cfg.TickInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); api.StartDreamRunner(ctx, s.pool, s.blobs, nil, cfg) }()

	waitForRunning := func(id, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for getDream(t, s, id)["status"] != "running" {
			if time.Now().After(deadline) {
				cancel()
				t.Fatal(what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitForRunning(dreamID, "the runner loop never started the dream that was pending when it started")

	_, secondBody := seededDreamBody(t, s)
	second := createDream(t, s, secondBody)["id"].(string)
	waitForRunning(second, "the ticker never started a dream created after the startup pass, so the loop passes once and then sleeps")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner loop did not stop with its context")
	}
}

// A store already at the name bound still clones under a distinct name: the
// input's portion is truncated so the " (dream <token>)" suffix survives, and
// with it the clone's own slug and mount.
func TestDreamCloneNameKeepsItsSuffixAtTheBound(t *testing.T) {
	s := newTestServer(t)
	storeID := createMemoryStore(t, s, strings.Repeat("n", 255))
	createMemory(t, s, storeID, "/a.md", "alpha")
	agentID, envID := fixture(t, s)
	ids := []any{createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"]}
	dreamID, _ := startedDream(t, s,
		map[string]any{"inputs": dreamInputs(storeID, ids), "model": "claude-opus-4-8"})

	var name string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT name FROM memory_stores WHERE id = $1`, dreamOutputStore(t, s, dreamID)).
		Scan(&name); err != nil {
		t.Fatalf("read the clone's name: %v", err)
	}
	_, token, _ := strings.Cut(dreamID, "_")
	if suffix := " (dream " + token + ")"; !strings.HasSuffix(name, suffix) {
		t.Errorf("clone name %q does not end in %q", name, suffix)
	}
	if n := utf8.RuneCountInString(name); n > 255 {
		t.Errorf("clone name is %d characters, over the store-name bound", n)
	}
}

// The clone writes its memories and their versions in batches (§4.2 step 4),
// and at the production width of 500 no test store ever reaches the second
// pass: the loop bound, the per-batch parameter numbering and the pairing of a
// memory with its own version are all decided by code one partial batch never
// exercises. Lowered to 2, five memories are two full batches and a partial,
// with a single full batch and an empty store either side of them.
func TestDreamCloneBatchesEveryMemoryAndItsVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"two full batches and a partial", []string{"/a.md", "/b.md", "/c.md", "/d.md", "/e.md"}},
		{"exactly one full batch", []string{"/a.md", "/b.md"}},
		{"an empty input store", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer api.SetDreamCloneBatchForTest(2)()
			s := newTestServer(t)
			storeID := createMemoryStore(t, s, "preferences")
			want := map[string]string{}
			for i, path := range tc.paths {
				content := fmt.Sprintf("content of %s (%d)", path, i)
				createMemory(t, s, storeID, path, content)
				want[path] = content
			}
			agentID, envID := fixture(t, s)
			ids := []any{createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"]}
			dreamID, sessionID := startedDream(t, s,
				map[string]any{"inputs": dreamInputs(storeID, ids), "model": "claude-opus-4-8"})

			cloneID := dreamOutputStore(t, s, dreamID)
			if got := clonePaths(t, s, cloneID); !maps.Equal(got, want) {
				t.Errorf("clone holds %v, want the input's %v", got, want)
			}
			assertCloneVersions(t, s, cloneID, sessionID, len(tc.paths))
			assertCloneVersionPerMemory(t, s, cloneID, len(tc.paths))
			// The input is the one store the clone must not touch, whichever
			// side of a batch boundary its memories fall.
			if got := clonePaths(t, s, storeID); !maps.Equal(got, want) {
				t.Errorf("the input store holds %v, want its own %v", got, want)
			}
		})
	}
}

// Object storage is what carries a transcript into a sandbox, so a deployment
// without it cannot start a dream: the render refuses, the claim stands, and
// the dream waits rather than running blind. The same path a failing Put takes.
func TestDreamStartNeedsObjectStorage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		blobs blob.Store
	}{
		{"no object storage configured", nil},
		{"the object store refuses the put", failingStore{blobtest.Mem()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			_, body := seededDreamBody(t, s)
			dreamID := createDream(t, s, body)["id"].(string)

			if err := api.DreamTickForTest(context.Background(), s.pool, tc.blobs,
				dbNow(t, s), dreamCfg()); err != nil {
				t.Fatalf("dream tick: %v", err)
			}

			if d := getDream(t, s, dreamID); d["status"] != "pending" {
				t.Fatalf("dream is %v, want pending (%v)", d["status"], d["error"])
			}
			if _, attempts, _ := dreamInternals(t, s, dreamID); attempts != 1 {
				t.Errorf("attempts = %d, want the one claim the tick made", attempts)
			}
			if n := len(dreamFileIDs(t, s, dreamID)); n != 0 {
				t.Errorf("%d transcript rows landed without their objects", n)
			}
		})
	}
}
