package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
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

// aptStubPath is where the stub below is planted. /usr/local/sbin precedes
// /usr/bin on the default PATH of a Debian container, so the pass's
// `command -v apt-get` and the install itself both find the stub rather than
// the real apt-get — which is what lets this row drive the whole seam without
// reaching a package mirror.
const aptStubPath = "/usr/local/sbin/apt-get"

// aptStub records the argv it is handed, one invocation per line, and succeeds.
const aptStub = `#!/bin/sh
echo "$*" >> /tmp/apt-get.argv
exit 0
`

// TestPackagesRealSandboxWithAStubbedApt drives plan 40's install pass end to
// end through a real Docker container: a real provision, a real Exec of the
// real command string, a real sentinel written by the real WriteFile — with
// only apt-get itself replaced, so the row needs no network at all. That
// matters twice over: `make test` must not gain a dependency on a public
// package mirror, and no other test under internal/ covers this seam whole.
//
// It also pins the skip, which is the half a unit test on a fake sandbox
// cannot claim about a real one: the second item's provision finds the
// sentinel this one left and runs nothing.
func TestPackagesRealSandboxWithAStubbedApt(t *testing.T) {
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

	// Pre-provision through the same provider and a matching spec, so the
	// executor's own provision adopts this container and finds the stub.
	ctx := context.Background()
	sb, err := provider.Provision(ctx, sandbox.Spec{SessionID: h.sid, Image: testImage})
	if err != nil {
		t.Fatalf("pre-provision the sandbox: %v", err)
	}
	if err := sb.WriteFiles(ctx, []sandbox.FileWrite{
		{Path: aptStubPath, Data: []byte(aptStub), Mode: 0o755},
	}); err != nil {
		t.Fatalf("plant the apt-get stub: %v", err)
	}

	h.setPackages(t, map[string][]string{"apt": {"jq", "curl"}})
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "cat /tmp/apt-get.argv; cat " + packagesSentinelPath},
	})
	h.suspend(t, string(bash))
	h.stepOnce(t)

	text := lastResultText(t, h)
	for _, want := range []string{"update -q", "install -y -q jq curl"} {
		if !strings.Contains(text, want) {
			t.Errorf("the stub recorded %q, want a line %q", text, want)
		}
	}
	var recs map[string]packageRecord
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &recs); err != nil {
				t.Fatalf("decode the sentinel %q: %v", line, err)
			}
		}
	}
	if rec := recs["apt"]; !rec.Installed || rec.Attempts != 1 || !slices.Equal(rec.Packages, []string{"jq", "curl"}) {
		t.Errorf("sentinel apt = %+v, want the list installed in one attempt (from %q)", rec, text)
	}

	// A second item on the same list must add nothing to the stub's record.
	count, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "wc -l < /tmp/apt-get.argv"},
	})
	h.suspend(t, string(count))
	h.stepOnce(t)
	if got := strings.TrimSpace(lastResultText(t, h)); got != "2" {
		t.Errorf("the stub was called %s times in total, want 2 — the second item must install nothing", got)
	}
}

// TestLivePackageInstallRealApt is the acceptance #353 asks for, behind its own
// consent variable: a real `apt-get install jq` into a real sandbox, then a
// `bash` tool call proving jq answers. It is the only tier that reaches a
// public package mirror (deb.debian.org), which is why it is opt-in rather
// than part of the gate.
//
// TierEnabled rather than Endpoint: this tier calls no model, so demanding the
// MODEL_* configuration Endpoint resolves would fail a correctly configured
// opt-in.
func TestLivePackageInstallRealApt(t *testing.T) {
	if !modeltest.TierEnabled("RUN_LIVE_PACKAGE_TESTS") {
		t.Skip("RUN_LIVE_PACKAGE_TESTS is not set: skipping the real apt-get install (no package mirror is reached)")
	}
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

	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "jq --version"},
	})
	h.suspend(t, string(bash))
	h.stepOnce(t)

	if errs := h.packageErrors(t); len(errs) != 0 {
		t.Fatalf("the install reported %+v, want none", errs)
	}
	results := h.types(t, "agent.tool_result")
	var body struct {
		IsError bool `json:"is_error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(results[len(results)-1].Body, &body)
	if body.IsError {
		t.Fatalf("`jq --version` failed after the install: %+v", body)
	}
	if len(body.Content) == 0 || !strings.Contains(body.Content[0].Text, "jq-") {
		t.Errorf("result = %+v, want jq's own version banner", body.Content)
	}
}

// lastResultText is the text of the most recent tool result — the shape both
// package rows above read their evidence out of.
func lastResultText(t *testing.T, h *harness) string {
	t.Helper()
	results := h.types(t, "agent.tool_result")
	if len(results) == 0 {
		t.Fatal("no tool result was appended")
	}
	var body struct {
		IsError bool `json:"is_error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(results[len(results)-1].Body, &body); err != nil {
		t.Fatalf("decode the tool result: %v", err)
	}
	if len(body.Content) == 0 {
		t.Fatalf("the tool result carried no content: %+v", body)
	}
	if body.IsError {
		t.Fatalf("the tool call failed: %q", body.Content[0].Text)
	}
	return body.Content[0].Text
}

// nonRootUID and nonRootImage are the sandbox-image half of the run below.
// `debian:stable-slim` cannot serve it: SANDBOX_RUN_AS_USER moves the uid but
// gives it nothing, and on this backend the image still decides what that uid
// owns (sandbox.Hardening's RunAsUser doc; docs/self-hosted-security.md §2 for
// the workdir, §4 for the other two paths) — so the platform's own
// `mkdir -p /mnt/memory` and the persistent shell's state directory are both
// refused before any memory is reached. The three paths handed over are what
// the platform itself writes: the workdir, the shell state root, and the
// resource root the mounts land under.
//
// The image deliberately keeps root as its own default user: with a `USER`
// line the run would be unprivileged whether or not the knob reached the
// container, and the uid the tool reports below would prove nothing about it.
const nonRootUID = 10001

const nonRootImage = `FROM debian:stable-slim
RUN useradd -m -u 10001 app \
 && mkdir -p /workspace /var/lib/map-shell /mnt \
 && chown app:app /workspace /var/lib/map-shell /mnt
`

// TestMemoryRoundTripRealSandboxAsNonRoot is plan 36 slice 4's deferred
// integration row (#488): the whole memory path through a real container the
// agent does not own. The store is materialized by the daemon, so its files
// land root-owned; the tools then run as SANDBOX_RUN_AS_USER's uid, append to
// one in place with `>>` and create another beside it with `>`. The append is
// the case decision 10's 0666 mode exists for — a root-owned 0644 file would
// refuse it while the file tools' rename-over would still succeed. Two rows
// already pin that constant's *value* (TestMaterializesMemoryStore and the
// sandbox contract suite), which a maintainer flipping it would update in
// lockstep; this is the first that shows why it has to be 0666.
func TestMemoryRoundTripRealSandboxAsNonRoot(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	image := "map-nonroot-memory-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(nonRootImage)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the non-root sandbox image: %v\n%s", err, out)
	}
	uid := int64(nonRootUID)
	h := newHarnessWith(t, provider, Config{Image: image, Hardening: sandbox.Hardening{RunAsUser: &uid}})
	t.Cleanup(func() {
		// Attach rather than Provision — the read-only half, so a cleanup can
		// never create the container it is here to remove — and both failures
		// are reported: a leaked sandbox poisons the next run on this daemon.
		sb, err := provider.Attach(context.Background(), h.sid)
		if errors.Is(err, sandbox.ErrNotFound) {
			return
		}
		if err != nil {
			t.Errorf("attach for teardown: %v", err)
			return
		}
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})

	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/notes.md", "hello\n")
	h.refMemory(t, memStoreID, memMount, "read_write")

	// `id -u` in the same command as the writes: the image's own default user
	// is root, so this is what says the knob reached the container — and a run
	// that silently landed as root would prove nothing about the mode.
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{
			"command": "echo appended >> " + memMount + "/notes.md" +
				" && echo fresh > " + memMount + "/new.md" +
				" && echo ran-as-$(id -u)"},
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
		t.Fatalf("a write into the materialized store was refused: %q", text)
	}
	if want := fmt.Sprintf("ran-as-%d", nonRootUID); !strings.Contains(text, want) {
		t.Fatalf("the tool ran as %q, want %q — the scenario needs an unprivileged shell", text, want)
	}

	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "hello\nappended\n" {
		t.Errorf("the store's /notes.md = %q, want the appended line pushed", got)
	}
	if got, _ := h.memoryContent(t, memStoreID, "/new.md"); got != "fresh\n" {
		t.Errorf("the store's /new.md = %q, want the created file pushed", got)
	}
	if got := h.versionsOf(t, memStoreID, "/notes.md"); !slices.Equal(got, []string{"created/none", "modified/session_actor"}) {
		t.Errorf("versions of /notes.md = %v, want the seed plus the session's write", got)
	}
	if got := h.versionsOf(t, memStoreID, "/new.md"); !slices.Equal(got, []string{"created/session_actor"}) {
		t.Errorf("versions of /new.md = %v, want one created by the session", got)
	}
}
