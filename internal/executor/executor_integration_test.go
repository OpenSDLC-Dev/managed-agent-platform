package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
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

// TestAStartupFloodIsOneToolErrorRealSandbox drives the bash tool through a
// real Kubernetes pod whose image's startup file prints 1.2 MB in every shell,
// past the output cap: the command runs, its exit record is pushed out of the
// output (sandbox.StartupOutputError), and the item commits one tool error
// saying so and schedules the model — no fault, so no reclaim to run the
// command again, forever (#860). A missing cluster is a hard failure.
func TestAStartupFloodIsOneToolErrorRealSandbox(t *testing.T) {
	provider, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("integration test requires a Kubernetes cluster: %v", err)
	}
	image := hookedtest.Image(t, "yes | head -c 1200000\n")
	h := newHarnessWith(t, provider, Config{Image: image})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})
	var faults []error
	h.exec.onFault = func(_ *queue.Item, err error) { faults = append(faults, err) }
	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "echo ran > /tmp/ran"},
	})
	h.suspend(t, string(bash))

	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step = %v, %v; want one item worked", worked, err)
	}
	if len(faults) != 0 {
		t.Fatalf("faults = %v, want none", faults)
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
	if !body.IsError || len(body.Content) == 0 || !strings.HasPrefix(body.Content[0].Text, "bash: the command's exit record did not reach the output") ||
		!strings.Contains(body.Content[0].Text, "The command ran") {
		t.Errorf("result = %+v, want the bash tool's error naming the image's startup", body)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0: nothing left to reclaim", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (resume)", got)
	}
	if worked, err := h.exec.step(context.Background()); err != nil || worked {
		t.Errorf("a second step = %v, %v; want no work", worked, err)
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

// TestHarvestUnderAStartupFloodRealSandbox runs a grading harvest on a real
// sandbox of each backend whose image's startup file prints 1.2 MB in every
// shell, past the output cap: the listing is pushed out — on Kubernetes with
// the exec's exit record, on Docker as a stdout the cap cut before any begin
// line — and the harvest settles as one that read no sandbox, grading chained
// and the item completed, rather than faulting into a reclaim that meets the
// same, forever (#860).
func TestHarvestUnderAStartupFloodRealSandbox(t *testing.T) {
	image := hookedtest.Image(t, "yes | head -c 1200000\n")
	dp, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	kp, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("integration test requires a Kubernetes cluster: %v", err)
	}
	for _, b := range []struct {
		name     string
		provider sandbox.Provider
	}{{"docker", dp}, {"k8s", kp}} {
		t.Run(b.name, func(t *testing.T) {
			h := newHarnessWith(t, b.provider, Config{Image: image})
			t.Cleanup(func() {
				sb, err := b.provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
				if err == nil {
					_ = sb.Destroy(context.Background())
				}
			})
			var faults []error
			h.exec.onFault = func(_ *queue.Item, err error) { faults = append(faults, err) }
			h.seedOutcome(t, domain.OutcomeResultEvaluating)
			h.enqueueHarvest(t)
			h.stepOnce(t)

			if len(faults) != 0 {
				t.Fatalf("faults = %v, want none", faults)
			}
			if rows := h.fileRows(t); len(rows) != 0 {
				t.Errorf("rows = %+v, want none", rows)
			}
			if got := h.liveOf(t, queue.ModelTurn); got != 1 {
				t.Errorf("model_turn live = %d, want 1 (grading chained)", got)
			}
			if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
				t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
			}
		})
	}
}

// TestFilesUnderAStartupFloodRealSandbox mounts a file into a real Kubernetes
// pod whose image's startup prints 1.2 MB in every shell: the first pass lands
// it (the sentinel says nothing has yet), and on the next, whose presence probe
// the startup pushes out of the output (sandbox.StartupOutputError), the
// agent's edit to the mount stays rather than being re-streamed over (#860).
func TestFilesUnderAStartupFloodRealSandbox(t *testing.T) {
	provider, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("integration test requires a Kubernetes cluster: %v", err)
	}
	image := hookedtest.Image(t, "yes | head -c 1200000\n")
	h := newHarnessWith(t, provider, Config{Image: image})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})
	const mount = "/mnt/session/uploads/flood.txt"
	h.seedFile(t, "file_flood", "as uploaded")
	h.refFiles(t, [2]string{"file_flood", mount})
	h.suspend(t, writeUse("a.txt", "x"))
	h.stepOnce(t)

	sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
	if err != nil {
		t.Fatalf("adopt the sandbox: %v", err)
	}
	if got, err := sb.ReadFile(context.Background(), mount); err != nil || string(got) != "as uploaded" {
		t.Fatalf("first pass mount = %q, %v; want it landed", got, err)
	}
	if err := sb.WriteFile(context.Background(), mount, []byte("the agent's edit")); err != nil {
		t.Fatalf("edit the mount: %v", err)
	}
	h.suspend(t, writeUse("b.txt", "y"))
	h.stepOnce(t)
	if got, err := sb.ReadFile(context.Background(), mount); err != nil || string(got) != "the agent's edit" {
		t.Errorf("mount = %q, %v after a pass whose probe the startup pushed out; want the agent's edit kept", got, err)
	}
}

// TestReposUnderAStartupFloodRealSandbox clones a repository into a real
// Kubernetes pod whose image's startup prints 1.2 MB in every shell. The
// presence exec's answer and the extraction's exit status are both pushed out
// of the output (sandbox.StartupOutputError), and each is read instead from
// the file API's stat, whose answer is the read exec's own exit status: the
// first pass clones, with no clone error, and the next keeps the checkout and
// the agent's file in it rather than re-cloning over them (#860).
func TestReposUnderAStartupFloodRealSandbox(t *testing.T) {
	provider, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("integration test requires a Kubernetes cluster: %v", err)
	}
	fx := newGitFixture(t, map[string]string{"README.md": "flooded\n"})
	image := hookedtest.Image(t, "yes | head -c 1200000\n")
	h := newHarnessWith(t, provider, Config{Image: image})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})
	h.seedRepoResource(t, "sesrsc_flood", fx.url(), repoMount, "ghp_fixture", nil)
	h.runPass(t)
	if got := fx.clones.Load(); got != 1 {
		t.Fatalf("clones = %d, want 1", got)
	}
	if n := h.cloneErrors(t); n != 0 {
		t.Errorf("clone errors = %d, want none: the extraction landed", n)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: image})
	if err != nil {
		t.Fatalf("adopt the sandbox: %v", err)
	}
	if got, err := sb.ReadFile(context.Background(), repoMount+"/README.md"); err != nil || string(got) != "flooded\n" {
		t.Fatalf("README.md = %q, %v; want the checkout", got, err)
	}
	if err := sb.WriteFile(context.Background(), repoMount+"/agent.txt", []byte("mine")); err != nil {
		t.Fatalf("write the agent's file: %v", err)
	}
	h.runPass(t)
	if got := fx.clones.Load(); got != 1 {
		t.Errorf("clones = %d, want 1: the second pass re-cloned", got)
	}
	if got, err := sb.ReadFile(context.Background(), repoMount+"/agent.txt"); err != nil || string(got) != "mine" {
		t.Errorf("agent.txt = %q, %v; want the agent's file kept", got, err)
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
	h := newHarnessWith(t, provider, Config{Image: testImage, Hardening: defaultHardening()})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	// Pre-provision through the same provider and a matching spec, so the
	// executor's own provision adopts this container and finds the stub.
	// Hardening is bound here, at create, and never re-applied by the
	// adoption — so it is the platform's default one, the same as the live
	// row below runs under.
	ctx := context.Background()
	sb, err := provider.Provision(ctx, sandbox.Spec{SessionID: h.sid, Image: testImage, Hardening: defaultHardening()})
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
	if rec := recs["apt"]; !rec.Installed || rec.Attempts != 1 || rec.Digest != packagesDigest([]string{"jq", "curl"}) {
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
	// Under the platform's DEFAULT capability drop, not the zero Hardening the
	// other rows run with: DefaultCapDrop takes CAP_SETUID and CAP_SETGID, the
	// two apt's own privilege drop to `_apt` needs, so a zero Hardening here
	// would pass an apt command that fails on every real deployment (the
	// compose acceptance of plan 40 found exactly that).
	h := newHarnessWith(t, provider, Config{Image: testImage, Hardening: defaultHardening()})
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
	if text := lastResultText(t, h); !strings.Contains(text, "jq-") {
		t.Errorf("result = %q, want jq's own version banner", text)
	}
}

// defaultHardening carries the default capability posture — sandbox.DefaultCapDrop,
// the drop HardeningFromEnv applies when SANDBOX_CAP_DROP is unset — which is what
// the package rows must prove the install under (apt needs the SETUID/SETGID this
// drops). It sets only the cap posture, not HardeningFromEnv's pid/CPU defaults,
// which do not bear on whether an install succeeds.
func defaultHardening() sandbox.Hardening {
	return sandbox.Hardening{CapDrop: slices.Clone(sandbox.DefaultCapDrop)}
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
	// One daemon for the image and the sandbox run from it (docker.DaemonHost).
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	image := dockertest.ImageFrom(t, "nonroot-memory", nonRootImage, "--host", docker.DaemonHost())
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

// TestSkillsAndMemoryMaterializeOnAReadOnlyRootRealSandbox is #859's
// executor-level row: under SANDBOX_READONLY_ROOTFS the docker backend's bulk
// write extracted every archive at `/`, which the daemon refuses on a read-only
// root, so neither a skill nor a memory store ever reached the sandbox — logged
// and skipped, the agent running on without them. Both go through
// Sandbox.WriteFiles, into the workdir and under /mnt; the tool reads them back
// in the same call that shows the root really is read-only, and the memory sync
// that follows writes its baseline through the same path, which is read back
// last.
func TestSkillsAndMemoryMaterializeOnAReadOnlyRootRealSandbox(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	hardening := defaultHardening()
	hardening.ReadOnlyRootfs = true
	h := newHarnessWith(t, provider, Config{Image: testImage, Hardening: hardening})
	t.Cleanup(func() {
		// Attach, the read-only half, as the non-root row above does: a
		// cleanup must never create the container it is here to remove.
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

	h.seedSkill(t, "skill_ro_root", "100", "ro-notes", map[string]string{
		"SKILL.md":            "# read-only root\n",
		"scripts/deep/run.sh": "echo ran-from-skill\n",
	})
	h.refSkills(t, [2]string{"skill_ro_root", "latest"})
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/notes.md", "hello\n")
	h.refMemory(t, memStoreID, memMount, "read_write")

	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{
			"command": "cat skills/ro-notes/SKILL.md; sh skills/ro-notes/scripts/deep/run.sh; cat " + memMount + "/notes.md; " +
				"echo appended >> " + memMount + "/notes.md; " +
				"touch /etc/map-859-probe 2>/dev/null && echo root-writable || echo root-read-only"},
	})
	h.suspend(t, string(bash))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}
	text := lastResultText(t, h)
	for _, want := range []string{"# read-only root", "ran-from-skill", "hello", "root-read-only"} {
		if !strings.Contains(text, want) {
			t.Errorf("the tool saw %q, want it to contain %q", text, want)
		}
	}

	// The sync pushed the append and wrote its new baseline back into the
	// sandbox — the second bulk write of the run, at /mnt/memory/.sync.
	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "hello\nappended\n" {
		t.Errorf("the store's /notes.md = %q, want the appended line pushed", got)
	}
	sb, err := provider.Attach(context.Background(), h.sid)
	if err != nil {
		t.Fatalf("attach to read the baseline: %v", err)
	}
	raw, err := sb.ReadFile(context.Background(), baselinePath(memStoreID))
	if err != nil {
		t.Fatalf("read the sync's baseline: %v", err)
	}
	if !strings.Contains(string(raw), sha256hex([]byte("hello\nappended\n"))) {
		t.Errorf("the baseline is %s, want it to record the pushed content's digest", raw)
	}
}
