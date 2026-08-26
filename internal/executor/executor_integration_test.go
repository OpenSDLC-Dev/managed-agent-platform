package executor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// testImage matches the sandbox contract: /bin/bash at that path plus a POSIX
// userland. The `bash` official image does not qualify — its bash lives
// elsewhere — which is the assumption the contract pins.
const testImage = "debian:stable-slim"

// TestClosedLoopRealSandbox drives one bash tool the whole way through a real
// container: the brain's suspend (a tool_use plus one tool_exec item), then the
// executor claims it, runs the command in a Docker sandbox, appends the result,
// and schedules the model_turn that resumes the brain. A missing daemon is a
// hard failure, not a skip — a skipped test silently hollows out the gate.
func TestClosedLoopRealSandbox(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	h := newHarnessWith(t, provider, Config{Image: testImage})
	t.Cleanup(func() {
		// Adopt the running container (Provision is idempotent) and tear it down.
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "echo closed-loop-ok"},
	})
	h.suspend(t, string(bash))

	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !worked {
		t.Fatal("step found no work")
	}

	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		IsError bool `json:"is_error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	if body.IsError {
		t.Errorf("bash echo returned an error result: %+v", body)
	}
	if len(body.Content) == 0 || !strings.Contains(body.Content[0].Text, "closed-loop-ok") {
		t.Errorf("result content = %+v, want it to contain the echoed text", body.Content)
	}

	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (resume)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (completed)", got)
	}
}

// TestHarvestRealSandbox is plan 21's Decision 8 verify line at the executor
// level: a file the agent's bash tool writes under /mnt/session/outputs/ in a
// real Docker container ends up in the files registry, its blob byte-identical
// to the container's copy — binary bytes included — with the grading turn
// chained behind the snapshot.
func TestHarvestRealSandbox(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	h := newHarnessWith(t, provider, Config{Image: testImage})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	// The agent's work: one bash tool leaves a text deliverable and a nested
	// binary one (random bytes, so identity is a real check, not luck).
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "mkdir -p /mnt/session/outputs/sub" +
			" && printf 'npv 42\\n' > /mnt/session/outputs/report.txt" +
			" && head -c 300 /dev/urandom > /mnt/session/outputs/sub/model.bin"},
	})
	h.suspend(t, string(bash))
	h.stepOnce(t)

	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 2 || rows[0].filename != "report.txt" || rows[1].filename != "sub/model.bin" {
		t.Fatalf("rows = %+v, want report.txt and sub/model.bin", rows)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
	if err != nil {
		t.Fatalf("adopt sandbox: %v", err)
	}
	for _, r := range rows {
		want, err := sb.ReadFile(context.Background(), outputsDir+"/"+r.filename)
		if err != nil {
			t.Fatalf("read back %s: %v", r.filename, err)
		}
		if got := h.blobBytes(t, r.id); got != string(want) {
			t.Errorf("%s: blob differs from the container's bytes (%d vs %d bytes)", r.filename, len(got), len(want))
		}
		if r.size != int64(len(want)) {
			t.Errorf("%s: size = %d, want %d", r.filename, r.size, len(want))
		}
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading chained)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

// TestHarvestTruncatedListingRealSandbox drives the truncation degradation
// through the real stack: enough real files that the listing script's output
// overflows the exec cap in a real Docker container (Truncated set by the
// backend, not a fake), and the harvest still publishes the sorted prefix and
// chains grading instead of faulting — the wedge the /code-review pass on
// PR #260 flagged.
func TestHarvestTruncatedListingRealSandbox(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	oldFiles := harvestSessionCapFiles
	harvestSessionCapFiles = 2 // stage only the first two — the point is the listing, not 5400 reads
	t.Cleanup(func() { harvestSessionCapFiles = oldFiles })

	h := newHarnessWith(t, provider, Config{Image: testImage})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	// 5400 files with 199-char names: the NUL-separated listing is ~1.08 MiB,
	// past sandbox.MaxOutputBytes (1 MiB), so the real exec truncates it.
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "mkdir -p /mnt/session/outputs && cd /mnt/session/outputs" +
			` && pad=$(printf 'x%.0s' {1..194}) && for i in $(seq -w 5400); do printf d > "f$i$pad"; done`},
	})
	h.suspend(t, string(bash))
	h.stepOnce(t)

	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	if faulted != nil {
		t.Fatalf("harvest faulted on the truncated listing: %v", faulted)
	}
	rows := h.fileRows(t)
	pad := strings.Repeat("x", 194)
	if len(rows) != 2 || rows[0].filename != "f0001"+pad || rows[1].filename != "f0002"+pad {
		t.Fatalf("rows = %d, want the two lexicographically first files", len(rows))
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading chained)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed, not left faulting)", got)
	}
}

// TestMemoryRoundTripRealSandboxAsNonRoot is plan 36 slice 4's deferred
// integration row (#488): the whole memory path through a real container the
// agent does not own. The store is materialized by the daemon, so its files
// land root-owned; the tools then run as SANDBOX_RUN_AS_USER's uid and append
// to one in place with `>>`, which is the case decision 10's 0666 mode exists
// for — a root-owned 0644 file would refuse that while the file tools'
// rename-over would still succeed, so nothing else in this suite would notice
// the mode going back. The run-end sync then pushes what the unprivileged
// shell wrote.
func TestMemoryRoundTripRealSandboxAsNonRoot(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	// nobody — the uid a deployment setting this knob most plausibly picks,
	// and not the gate's own, which Hardening.Validate refuses for a gated
	// sandbox.
	uid := int64(65534)
	h := newHarnessWith(t, provider, Config{Image: testImage, Hardening: sandbox.Hardening{RunAsUser: &uid}})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/notes.md", "hello\n")
	h.refMemory(t, memStoreID, memMount, "read_write")

	// `id -u` in the same command as the append: a run that silently landed as
	// root would prove nothing, so the uid is read off the result rather than
	// trusted from the Spec.
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{
			"command": "echo appended >> " + memMount + "/notes.md && echo ran-as-$(id -u)"},
	})
	h.suspend(t, string(bash))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}

	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		IsError bool `json:"is_error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	text := ""
	if len(body.Content) > 0 {
		text = body.Content[0].Text
	}
	if body.IsError {
		t.Fatalf("the append was refused: %q", text)
	}
	if !strings.Contains(text, "ran-as-65534") {
		t.Fatalf("the tool ran as %q, want uid 65534 — the scenario needs an unprivileged shell", text)
	}

	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "hello\nappended\n" {
		t.Errorf("the store's /notes.md = %q, want the appended line pushed", got)
	}
	if got := h.versionsOf(t, memStoreID, "/notes.md"); !slices.Equal(got, []string{"created/none", "modified/session_actor"}) {
		t.Errorf("versions of /notes.md = %v, want the seed plus the session's write", got)
	}
}
