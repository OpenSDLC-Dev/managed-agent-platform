package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Unit M of plan 25 (docs/plan/25_git-repo-mounting.md, "Verification"): the
// materialization surface of github_repository resources, driven through a real
// work item against a real git remote. Rows that assert what the agent's tools
// see (S2, the sandbox filesystem) need the real Docker sandbox — the fake
// never runs tar, so a clone could never make <mount>/.git appear there. Rows
// that assert an absent mount and a surfaced error (S3, the event log) are
// provable on the fake and run without Docker's cost.

const repoMount = "/workspace/fixture"

// dockerRepoHarness is the real-sandbox harness the S2 rows share: a Docker
// provider, a session already running, and the container torn down afterwards.
func dockerRepoHarness(t *testing.T) (*harness, *docker.Provider) {
	t.Helper()
	return wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider { return p })
}

// wrappedRepoHarness is dockerRepoHarness with the executor's provider wrapped,
// so a test can stand between the executor and the sandbox; the Docker
// provider comes back unwrapped, for reading what a pass left.
func wrappedRepoHarness(t *testing.T, wrap func(sandbox.Provider) sandbox.Provider) (*harness, *docker.Provider) {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	h := newHarnessWith(t, wrap(provider), Config{Image: testImage})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})
	return h, provider
}

// adopt re-provisions the session's live sandbox so a test can read what the
// agent's tools would see (Provision is idempotent).
func adopt(t *testing.T, provider *docker.Provider, h *harness) sandbox.Sandbox {
	t.Helper()
	sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
	if err != nil {
		t.Fatalf("adopt the sandbox: %v", err)
	}
	return sb
}

func readSandboxFile(t *testing.T, sb sandbox.Sandbox, path string) string {
	t.Helper()
	b, err := sb.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestRepoCloneDefaultBranch is m-clone-default: a repository with no checkout
// lands its default branch, tree and .git alike, at the mount path.
func TestRepoCloneDefaultBranch(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "fixture repo\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_default", fx.url(), repoMount, "ghp_fixture", nil)

	h.runPass(t)

	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, repoMount+"/README.md"); got != "fixture repo\n" {
		t.Errorf("README.md = %q, want the fixture's content", got)
	}
	head := readSandboxFile(t, sb, repoMount+"/.git/HEAD")
	if !strings.HasPrefix(head, "ref: refs/heads/") {
		t.Errorf(".git/HEAD = %q, want a branch ref (the remote's default)", head)
	}
}

// TestRepoCloneBranch is m-clone-branch: a branch checkout lands that branch's
// tip, and .git/HEAD names the branch.
func TestRepoCloneBranch(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "base\n"})
	fx.branch(t, "feature", map[string]string{"feature.txt": "on the branch\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_branch", fx.url(), repoMount, "ghp_fixture",
		map[string]any{"type": "branch", "name": "feature"})

	h.runPass(t)

	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, repoMount+"/feature.txt"); got != "on the branch\n" {
		t.Errorf("feature.txt = %q, want the branch tip's content", got)
	}
	if got := readSandboxFile(t, sb, repoMount+"/.git/HEAD"); got != "ref: refs/heads/feature\n" {
		t.Errorf(".git/HEAD = %q, want the feature branch ref", got)
	}
}

// TestRepoCloneCommit is m-clone-commit: a commit checkout detaches at that
// SHA, so .git/HEAD carries the bare hash and the later commit's file is gone.
func TestRepoCloneCommit(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"first.txt": "one\n"})
	first, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	firstSHA := first.Hash().String()
	fx.commit(t, "second", map[string]string{"second.txt": "two\n"})

	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_commit", fx.url(), repoMount, "ghp_fixture",
		map[string]any{"type": "commit", "sha": firstSHA})

	h.runPass(t)

	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, repoMount+"/.git/HEAD"); strings.TrimSpace(got) != firstSHA {
		t.Errorf(".git/HEAD = %q, want the bare SHA %s (detached)", got, firstSHA)
	}
	if _, err := sb.ReadFile(context.Background(), repoMount+"/second.txt"); err == nil {
		t.Error("second.txt is present, but the checkout is at the first commit")
	}
}

// TestRepoTokenNeverEntersTheSandbox is m-token-absent 🔍 — the load-bearing
// security probe. After a successful clone the sandbox is swept for the token:
// the environment, the repository's own .git/config, and the two credential
// files git would write if the token had ridden the URL. Any hit is a failure.
func TestRepoTokenNeverEntersTheSandbox(t *testing.T) {
	const token = "ghp_SWEEP-TOKEN-IN-SANDBOX-4a91"
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	fx.wantAuth.Store("Basic " + base64.StdEncoding.EncodeToString([]byte(tokenUsername+":"+token)))
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_sweep", fx.url(), repoMount, token, nil)

	h.runPass(t)

	sb := adopt(t, provider, h)
	// The clone must have happened, or the sweep proves nothing.
	readSandboxFile(t, sb, repoMount+"/README.md")
	// The fixture demanded the token as a header, so a successful clone is
	// itself the proof it travelled there and nowhere else.
	if got, _ := fx.sawAuth.Load().(string); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("fixture saw Authorization %q, want the token as a basic-auth header", got)
	}

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: strings.Join([]string{
		"env",
		"cat " + shellQuote(repoMount+"/.git/config"),
		"cat ~/.git-credentials 2>/dev/null",
		"cat ~/.netrc 2>/dev/null",
		"grep -ra " + shellQuote(token) + " " + shellQuote(repoMount) + " 2>/dev/null",
		"true",
	}, "; ")})
	if err != nil {
		t.Fatalf("sweep the sandbox: %v", err)
	}
	if strings.Contains(res.Stdout, token) || strings.Contains(res.Stderr, token) {
		t.Errorf("the token reached the sandbox:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
}

// TestRepoIdempotenceAndTamper is m-idempotence 🔍 and m-tamper 🔍: a second
// pass over a present checkout clones nothing and leaves the agent's own work
// alone, while a checkout the agent removed is re-cloned fresh on the next pass.
func TestRepoIdempotenceAndTamper(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_idem", fx.url(), repoMount, "ghp_fixture", nil)

	h.runPass(t)
	after := fx.clones.Load()
	if after == 0 {
		t.Fatal("the first pass did not clone")
	}

	// The agent works inside the checkout between passes.
	sb := adopt(t, provider, h)
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "echo mine > " + shellQuote(repoMount+"/agent.txt")}); err != nil {
		t.Fatalf("write the agent's file: %v", err)
	}

	h.runPass(t)
	if got := fx.clones.Load(); got != after {
		t.Errorf("clones = %d, want %d — the second pass re-cloned a present checkout", got, after)
	}
	if got := readSandboxFile(t, sb, repoMount+"/agent.txt"); got != "mine\n" {
		t.Errorf("agent.txt = %q, want the agent's own file to survive", got)
	}

	// m-tamper: the agent removes the checkout; the next pass re-clones fresh.
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "rm -rf " + shellQuote(repoMount)}); err != nil {
		t.Fatalf("remove the checkout: %v", err)
	}
	h.runPass(t)
	if got := fx.clones.Load(); got <= after {
		t.Errorf("clones = %d, want more than %d — a removed checkout must re-clone", got, after)
	}
	readSandboxFile(t, sb, repoMount+"/README.md")
	if _, err := sb.ReadFile(context.Background(), repoMount+"/agent.txt"); err == nil {
		t.Error("agent.txt survived a re-clone, but a re-clone is fresh, never merged")
	}
}

// floodingRepoHarness is dockerRepoHarness over a provider whose sandboxes'
// presence probes can be made not to answer (floodingProvider).
func floodingRepoHarness(t *testing.T) (*harness, *docker.Provider, *floodingProvider) {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	flooding := &floodingProvider{Provider: provider}
	h := newHarnessWith(t, flooding, Config{Image: testImage})
	t.Cleanup(func() {
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})
	return h, provider, flooding
}

// cloneErrors is the session's repository clone errors.
func (h *harness) cloneErrors(t *testing.T) int {
	t.Helper()
	n := 0
	for _, ev := range h.types(t, "session.error") {
		if strings.Contains(string(ev.Body), repoCloneErrorType) {
			n++
		}
	}
	return n
}

// TestRepoExtractionTheStartupKeptFromAnsweringLands: an extraction whose exit
// status the image's startup kept from the platform — here by ending the
// shell that reads it (sandbox.StartupExitError, #942) — is read instead from
// the stat of the .git it lands, as a flood's is: the checkout counts as
// landed, and no clone error is said.
func TestRepoExtractionTheStartupKeptFromAnsweringLands(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider, flooding := floodingRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_exit", fx.url(), repoMount, "ghp_fixture", nil)
	flooding.failExtract.Store(1)

	h.runPass(t)
	if got := flooding.failExtract.Load(); got != 0 {
		t.Fatalf("failExtract = %d, want the extraction to have met the startup's exit", got)
	}
	if n := h.cloneErrors(t); n != 0 {
		t.Errorf("clone errors = %d, want none: the extraction landed", n)
	}
	if got := readSandboxFile(t, adopt(t, provider, h), repoMount+"/README.md"); got != "x\n" {
		t.Errorf("README.md = %q, want the checkout", got)
	}
}

// TestRepoUnansweredProbeKeepsTheCheckout: a presence probe that does not
// answer — neither the probe exec, its answer an image's startup pushed out of
// the output (sandbox.StartupOutputError), nor the stat after it, twice —
// says nothing of whether the checkout has gone. Taken for "gone", it
// re-cloned on every pass, and the clone's rm -rf of the mount took the
// agent's work with it (#860); the checkout stays, and no clone error is said.
func TestRepoUnansweredProbeKeepsTheCheckout(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider, flooding := floodingRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_flood", fx.url(), repoMount, "ghp_fixture", nil)

	h.runPass(t)
	after := fx.clones.Load()
	if after == 0 {
		t.Fatal("the first pass did not clone")
	}
	sb := adopt(t, provider, h)
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "echo mine > " + shellQuote(repoMount+"/agent.txt")}); err != nil {
		t.Fatalf("write the agent's file: %v", err)
	}

	flooding.failExec.Store(1000)
	flooding.failStat.Store(1000)
	h.runPass(t)
	if got := fx.clones.Load(); got != after {
		t.Errorf("clones = %d, want %d — a probe that did not answer re-cloned", got, after)
	}
	if got := readSandboxFile(t, sb, repoMount+"/agent.txt"); got != "mine\n" {
		t.Errorf("agent.txt = %q, want the agent's work kept", got)
	}
	if n := h.cloneErrors(t); n != 0 {
		t.Errorf("clone errors = %d, want none: an unanswered probe says nothing is missing", n)
	}
}

// TestRepoProbeAsksAgainBeforeItSkips: a probe that does not answer once —
// a transient exec failure, a lost exit stream (#105) — on a sandbox never
// cloned into is asked again, and the repository lands in the same pass.
// Asked once and left unanswered, it would have started the agent's run with
// the repository silently missing.
func TestRepoProbeAsksAgainBeforeItSkips(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider, flooding := floodingRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_retry", fx.url(), repoMount, "ghp_fixture", nil)

	flooding.failExec.Store(1)
	flooding.failStat.Store(1)
	h.runPass(t)
	if got := fx.clones.Load(); got != 1 {
		t.Fatalf("clones = %d, want 1: the retry answers absent", got)
	}
	readSandboxFile(t, adopt(t, provider, h), repoMount+"/README.md")
}

// TestRepoUnansweredOnAFreshSandboxClonesNextPass: a sandbox that answers
// neither probe, twice, gets no clone and no clone error that pass — nothing
// says the repository is missing — and the next pass, asked again, clones.
func TestRepoUnansweredOnAFreshSandboxClonesNextPass(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider, flooding := floodingRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_next", fx.url(), repoMount, "ghp_fixture", nil)

	flooding.failExec.Store(2)
	flooding.failStat.Store(2)
	h.runPass(t)
	if got := fx.clones.Load(); got != 0 {
		t.Fatalf("clones = %d, want 0: no probe answered", got)
	}
	if n := h.cloneErrors(t); n != 0 {
		t.Errorf("clone errors = %d, want none", n)
	}
	h.runPass(t)
	if got := fx.clones.Load(); got != 1 {
		t.Errorf("clones = %d, want 1 on the next pass", got)
	}
	readSandboxFile(t, adopt(t, provider, h), repoMount+"/README.md")
}

// floodingProvider hands out its sandboxes with the next failExec presence
// execs answering an image's startup flood (sandbox.StartupOutputError), as
// one that prints past the cap does on Kubernetes, the next failStat stats of
// a .git (sandbox.StatPresence) failing as a transient exec does, the next
// failVacant of mountOccupied's probes answering the flood too, and the next
// failExtract extractions running and then answering the startup's exit
// (sandbox.StartupExitError), as one whose startup ends the exit record's
// reader does on Kubernetes.
type floodingProvider struct {
	sandbox.Provider
	failExec, failStat, failVacant, failExtract atomic.Int32
}

func (p *floodingProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &floodingSandbox{Sandbox: sb, p: p}, nil
}

// isGitProbe reports whether command is the repository presence probe: a
// batch of sandbox.ProbeEach's asking after a .git alone.
func isGitProbe(command string) bool {
	script, _, framed := sandboxtest.Unwrap(command)
	if !framed {
		return false
	}
	paths, ok := sandboxtest.ProbeEachPaths(script)
	return ok && len(paths) == 1 && strings.HasSuffix(paths[0], "/.git")
}

type floodingSandbox struct {
	sandbox.Sandbox
	p *floodingProvider
}

func (s *floodingSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if isGitProbe(req.Command) && s.p.failExec.Add(-1) >= 0 {
		_, _ = s.Sandbox.Exec(ctx, req)
		return sandbox.ExecResult{}, &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
	}
	if strings.Contains(req.Command, "; then echo vacant; else echo occupied; fi") && s.p.failVacant.Add(-1) >= 0 {
		_, _ = s.Sandbox.Exec(ctx, req)
		return sandbox.ExecResult{}, &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
	}
	if strings.Contains(req.Command, " && tar -xf ") && s.p.failExtract.Add(-1) >= 0 {
		_, _ = s.Sandbox.Exec(ctx, req)
		return sandbox.ExecResult{}, &sandbox.StartupExitError{What: "the command's exit record", MayHaveRun: true}
	}
	return s.Sandbox.Exec(ctx, req)
}

func (s *floodingSandbox) ReadFileStream(ctx context.Context, p string, maxBytes int64) (io.ReadCloser, int64, error) {
	if maxBytes == 0 && strings.HasSuffix(p, "/.git") && s.p.failStat.Add(-1) >= 0 {
		return nil, 0, errors.New("k8s: read: exec stream lost")
	}
	return s.Sandbox.ReadFileStream(ctx, p, maxBytes)
}

// TestRepoMetacharMountPath is m-metachar-mount 🔍: a mount path carrying a
// space and a single quote materializes at that literal path, and the shell
// injection the same characters would allow does not happen.
func TestRepoMetacharMountPath(t *testing.T) {
	const mount = "/workspace/it's a repo"
	fx := newGitFixture(t, map[string]string{"README.md": "quoted\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_quote", fx.url(), mount, "ghp_fixture", nil)

	h.runPass(t)

	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, mount+"/README.md"); got != "quoted\n" {
		t.Errorf("README.md = %q, want the fixture's content at the literal path", got)
	}
}

// TestRepoOverlayOrdering is m-overlay: a file mounted inside a checkout
// survives the pass, which is only true because repositories materialize
// before files.
func TestRepoOverlayOrdering(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "base\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_overlay", fx.url(), repoMount, "ghp_fixture", nil)

	h.seedFile(t, "file_overlay", "overlaid\n")
	h.appendResource(t, map[string]any{
		"id": "sesrsc_overlayfile", "type": "file", "file_id": "file_overlay",
		"mount_path": repoMount + "/overlay.txt",
		"created_at": time.Unix(1700000000, 0).UTC(), "updated_at": time.Unix(1700000000, 0).UTC(),
	})

	h.runPass(t)

	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, repoMount+"/README.md"); got != "base\n" {
		t.Errorf("README.md = %q, want the checkout's own file", got)
	}
	if got := readSandboxFile(t, sb, repoMount+"/overlay.txt"); got != "overlaid\n" {
		t.Errorf("overlay.txt = %q, want the file mount overlaid into the checkout", got)
	}
}

// TestRepoCloneFailuresSurface is m-bad-token 🔍, m-repo-gone 🔍, m-bad-sha 🔍
// and m-stall 🔍: each failure leaves the mount absent, surfaces exactly one
// session.error naming its reason, and lets the work item complete.
func TestRepoCloneFailuresSurface(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		arrange      func(t *testing.T, fx *gitFixture, h *harness)
	}{
		{"bad token", repoOutcomeAuth, func(t *testing.T, fx *gitFixture, h *harness) {
			fx.status.Store(http.StatusUnauthorized)
			h.seedRepoResource(t, "sesrsc_fail", fx.url(), repoMount, "ghp_wrong", nil)
		}},
		{"repo gone", repoOutcomeNotFound, func(t *testing.T, fx *gitFixture, h *harness) {
			fx.status.Store(http.StatusNotFound)
			h.seedRepoResource(t, "sesrsc_fail", fx.url(), repoMount, "ghp_fixture", nil)
		}},
		{"bad sha", repoOutcomeCheckout, func(t *testing.T, fx *gitFixture, h *harness) {
			h.seedRepoResource(t, "sesrsc_fail", fx.url(), repoMount, "ghp_fixture",
				map[string]any{"type": "commit", "sha": strings.Repeat("a", 40)})
		}},
		{"stall", repoOutcomeTimeout, func(t *testing.T, fx *gitFixture, h *harness) {
			fx.stall.Store(int64(2 * time.Second))
			h.exec.cfg.RepoCloneTimeout = 200 * time.Millisecond
			h.seedRepoResource(t, "sesrsc_fail", fx.url(), repoMount, "ghp_fixture", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			tc.arrange(t, fx, h)

			h.runPass(t)

			errs := h.repoErrors(t)
			if len(errs) != 1 {
				t.Fatalf("recorded %d clone errors, want exactly 1 (%v)", len(errs), errs)
			}
			if errs[0]["reason"] != tc.reason {
				t.Errorf("reason = %v, want %q", errs[0]["reason"], tc.reason)
			}
			if errs[0]["resource_id"] != "sesrsc_fail" || errs[0]["url"] != fx.url() {
				t.Errorf("payload = %v, want it to name the resource and its url", errs[0])
			}
			// Every other session.error this platform writes carries a
			// retry_status, and the SDK's union makes it required on all of
			// them; a variant of ours that omitted it would be the one error a
			// client switching over the union could not read uniformly.
			rs, _ := errs[0]["retry_status"].(map[string]any)
			if rs["type"] != "retrying" {
				t.Errorf("retry_status = %v, want {type: retrying} — the next work "+
					"item re-probes and clones again", errs[0]["retry_status"])
			}
			if _, ok := sb.files[repoMount]; ok {
				t.Error("the mount exists, but the clone failed")
			}
			// The run continues: the tool still answered.
			if got := h.types(t, "agent.tool_result"); len(got) == 0 {
				t.Error("no tool result — a failed clone must not fail the run")
			}
		})
	}
}

// TestRepoCloneErrorDedupe is m-error-dedupe 🔍: two passes failing the same
// way record one event, and a different reason records a second.
func TestRepoCloneErrorDedupe(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	fx.status.Store(http.StatusUnauthorized)
	h := newHarness(t, &fakeSandbox{})
	h.seedRepoResource(t, "sesrsc_dedupe", fx.url(), repoMount, "ghp_wrong", nil)

	h.runPass(t)
	h.runPass(t)
	if errs := h.repoErrors(t); len(errs) != 1 {
		t.Fatalf("recorded %d clone errors after two identical failures, want 1 (%v)", len(errs), errs)
	}

	// A flip to a reason the resource has not had re-emits: the client learns
	// the failure changed.
	fx.status.Store(http.StatusNotFound)
	h.runPass(t)
	errs := h.repoErrors(t)
	if len(errs) != 2 {
		t.Fatalf("recorded %d clone errors after a reason flip, want 2 (%v)", len(errs), errs)
	}
	if errs[0]["reason"] != repoOutcomeAuth || errs[1]["reason"] != repoOutcomeNotFound {
		t.Errorf("reasons = %v/%v, want auth then not_found", errs[0]["reason"], errs[1]["reason"])
	}
}

// TestRepoCloneErrorDedupeKeysOnTheMessage: one reason with two messages is
// two events, so an occupied mount path's refusal reaches the client after a
// generic failure with the same reason. A return to the generic failure is
// not emitted again: the dedupe spans the history, which bounds the log.
func TestRepoCloneErrorDedupeKeysOnTheMessage(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	m := repoRef{ID: "sesrsc_flip", MountPath: "/opt/repo", Type: "github_repository", URL: "https://example.invalid/o/r.git"}
	for _, message := range []string{repoCloneFailedMessage, repoCloneFailedMessage, repoMountOccupiedMessage, repoCloneFailedMessage} {
		h.exec.emitRepoCloneError(context.Background(), h.sid, m, repoOutcomeInternal, message)
	}
	var got []string
	for _, e := range h.repoErrors(t) {
		message, _ := e["message"].(string)
		got = append(got, message)
	}
	if want := []string{repoCloneFailedMessage, repoMountOccupiedMessage}; !slices.Equal(got, want) {
		t.Errorf("clone error messages = %q, want %q", got, want)
	}
}

// TestRepoPartialFailure is m-partial 🔍: one repository failing leaves the
// others and the file mounts to materialize normally.
func TestRepoPartialFailure(t *testing.T) {
	good := newGitFixture(t, map[string]string{"README.md": "good\n"})
	bad := newGitFixture(t, map[string]string{"README.md": "bad\n"})
	bad.status.Store(http.StatusNotFound)

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedRepoResource(t, "sesrsc_bad", bad.url(), "/workspace/bad", "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_good", good.url(), "/workspace/good", "ghp_fixture", nil)
	h.seedFile(t, "file_notes", "kept\n")
	h.appendResource(t, map[string]any{
		"id": "sesrsc_file", "type": "file", "file_id": "file_notes", "mount_path": "/workspace/notes.txt",
		"created_at": time.Unix(1700000000, 0).UTC(), "updated_at": time.Unix(1700000000, 0).UTC(),
	})

	h.runPass(t)

	errs := h.repoErrors(t)
	if len(errs) != 1 || errs[0]["resource_id"] != "sesrsc_bad" {
		t.Fatalf("clone errors = %v, want exactly the failing repository", errs)
	}
	// The fake sandbox cannot extract a tar, so the surviving repository is
	// proved by its shipped staging tar rather than by a landed tree.
	if _, ok := sb.files["/tmp/repo-sesrsc_good.tar"]; !ok {
		t.Errorf("the good repository shipped nothing; files = %v", sbPaths(sb))
	}
	if got := sb.files["/workspace/notes.txt"]; got != "kept\n" {
		t.Errorf("notes.txt = %q, want the file mount to materialize anyway", got)
	}
}

// TestRepoOversizeAbortsMidClone is m-oversize 🔍: a repository past the byte
// budget is refused with `too_large` and ships nothing into the sandbox.
//
// It does NOT prove the abort was mid-clone — a meter that measured after the
// fact would satisfy every assertion here, as its mutation confirms. The two
// properties that make the meter in-flight are pinned directly by
// TestMeteredFSRefusesTheWriteThatCrosses and TestMeteredFSChrootSharesTheBudget.
func TestRepoOversizeAbortsMidClone(t *testing.T) {
	big := strings.Repeat("payload\n", 200_000) // ~1.6 MB, compressible but real
	fx := newGitFixture(t, map[string]string{"big.txt": big})
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.exec.cfg.RepoCloneMaxBytes = 64 << 10 // 64 KiB, far below the repository
	h.seedRepoResource(t, "sesrsc_big", fx.url(), repoMount, "ghp_fixture", nil)

	h.runPass(t)

	errs := h.repoErrors(t)
	if len(errs) != 1 || errs[0]["reason"] != repoOutcomeTooLarge {
		t.Fatalf("clone errors = %v, want exactly one too_large", errs)
	}
	if _, ok := sb.files["/tmp/repo-sesrsc_big.tar"]; ok {
		t.Error("a tar was shipped, but the clone was abandoned over budget")
	}
}

// TestRepoWithoutCipher 🔍: an executor with no secrets cipher cannot open a
// sealed token, so the repository does not clone and the failure surfaces
// rather than passing silently.
func TestRepoWithoutCipher(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedRepoResource(t, "sesrsc_nocipher", fx.url(), repoMount, "ghp_fixture", nil)
	h.exec.cipher = nil

	h.runPass(t)

	errs := h.repoErrors(t)
	if len(errs) != 1 || errs[0]["reason"] != repoOutcomeInternal {
		t.Fatalf("clone errors = %v, want exactly one internal", errs)
	}
	if fx.clones.Load() != 0 {
		t.Error("the fixture was cloned, but no token could be opened")
	}
}

// TestRepoWithoutCipherSpareAlreadyMaterialized 🔍: config drift must not
// manufacture a failure for a repository that is already on disk.
//
// A cipher-less executor cannot clone, but it also has nothing to clone for a
// mount that already carries a `.git` — a restored checkpoint, or a pass by a
// correctly-configured executor before the drift. Reporting that repository as
// failed would tell the client a checkout is missing while the agent is reading
// it, so the presence probe is asked first and only a repository that genuinely
// needs a clone is refused.
func TestRepoWithoutCipherSparesAlreadyMaterialized(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	sb := &fakeSandbox{files: map[string]string{repoMount + "/.git": "already here"}}
	h := newHarness(t, sb)
	h.seedRepoResource(t, "sesrsc_present", fx.url(), repoMount, "ghp_fixture", nil)
	h.exec.cipher = nil

	h.runPass(t)

	if errs := h.repoErrors(t); len(errs) != 0 {
		t.Errorf("clone errors = %v, want none — the repository is already materialized", errs)
	}
}

// TestRepoInterruptedExtractMidTarLeavesNoTree is m-interrupted-extract 🔍:
// an extraction that dies **part way through the tar** leaves no `<mount>/.git`
// behind — which is the whole point of staging into a sibling and renaming,
// since a partial tree with a `.git` in it is precisely what the idempotence
// probe would later trust and skip.
//
// The failure must be forced *during* the tar and identically for any candidate
// implementation, which rules out planting an obstruction at a path: staging and
// the mount are different paths, so an obstruction at either one would only ever
// trip one of the two. What both share is the tar binary itself, so the fixture
// shadows it with a script that extracts a `.git` and then dies — the shape a
// truncated archive or a killed sandbox produces. Whatever `-C` names ends up
// holding a half-tree, and the assertion is that the mount is not that thing.
func TestRepoInterruptedExtractMidTarLeavesNoTree(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_midtar", fx.url(), repoMount, "ghp_fixture", nil)

	sb := adopt(t, provider, h)
	installFailingTar(t, sb)

	h.runPass(t)

	if errs := h.repoErrors(t); len(errs) != 1 {
		t.Fatalf("recorded %d clone errors, want exactly 1 (%v)", len(errs), errs)
	}
	// The load-bearing assertion. The half-tree exists somewhere — the point is
	// that it is not at the mount, where the idempotence probe would find it.
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "ls -a " + shellQuote(path.Dir(repoMount)) + "; test ! -e " + shellQuote(repoMount+"/.git")})
	if err != nil {
		t.Fatalf("inspect the mount: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("a partial tree carrying .git survived a mid-tar failure; %s holds: %s",
			path.Dir(repoMount), res.Stdout)
	}
	if strings.Contains(res.Stdout, "map-repo-staging") {
		t.Errorf("staging survived a failed extraction: %s", res.Stdout)
	}

	// With a working tar the next pass materializes — which it can only do
	// because the failed attempt left no `.git` for the probe to trust.
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "rm -f /usr/local/bin/tar"}); err != nil {
		t.Fatalf("restore the real tar: %v", err)
	}
	h.runPass(t)
	if got := readSandboxFile(t, sb, repoMount+"/README.md"); got != "x\n" {
		t.Errorf("README.md = %q, want the retry to have materialized", got)
	}
}

// installFailingTar shadows the container's tar with one that extracts a `.git`
// into whatever `-C` names and then exits non-zero. It reads `-C` from the
// argument list rather than a fixed position so it fails the same way for any
// extraction command, not just the one this package happens to build today.
// WriteFile lands the bytes over Docker's archive API, so installing the fake
// does not itself depend on the binary being replaced.
func installFailingTar(t *testing.T, sb sandbox.Sandbox) {
	t.Helper()
	const script = `#!/bin/sh
d=.
while [ $# -gt 0 ]; do
	if [ "$1" = "-C" ]; then d=$2; fi
	shift
done
mkdir -p "$d/.git"
echo partial > "$d/.git/HEAD"
echo 'tar: unexpected end of file' >&2
exit 2
`
	ctx := context.Background()
	if err := sb.WriteFile(ctx, "/usr/local/bin/tar", []byte(script)); err != nil {
		t.Fatalf("install the failing tar: %v", err)
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "chmod 0755 /usr/local/bin/tar && command -v tar"})
	if err != nil {
		t.Fatalf("chmod the failing tar: %v", err)
	}
	// It has to be the one bash resolves, or the row proves nothing.
	if got := strings.TrimSpace(res.Stdout); got != "/usr/local/bin/tar" {
		t.Fatalf("tar resolves to %q, want the shadow at /usr/local/bin/tar (exit %d, %s)",
			got, res.ExitCode, res.Stderr)
	}
}

// TestRepoExtractFailureIsTolerated 🔍 is the same tolerance from the other
// direction: an extraction that cannot even begin — the mount's parent is a
// regular file, so the staging directory cannot be created — surfaces as a
// clone error, cleans up, and leaves the run to continue.
func TestRepoExtractFailureIsTolerated(t *testing.T) {
	// A path the network arm's text match would read as the git host's
	// failure, were the extraction's error not classified before it.
	const blocked = "/workspace/EOF"
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_partial", fx.url(), blocked+"/repo", "ghp_fixture", nil)

	// Provision first so the blocking file exists before the pass runs.
	sb := adopt(t, provider, h)
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "echo blocker > " + shellQuote(blocked)}); err != nil {
		t.Fatalf("plant the blocking file: %v", err)
	}

	h.runPass(t)

	errs := h.repoErrors(t)
	if len(errs) != 1 || errs[0]["reason"] != repoOutcomeInternal {
		t.Fatalf("clone errors = %v, want exactly 1, with reason %s", errs, repoOutcomeInternal)
	}
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "ls -a " + shellQuote("/workspace") + "; test ! -e " + shellQuote(blocked+"/repo")})
	if err != nil {
		t.Fatalf("inspect the workspace: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("a mount exists after a failed extraction; /workspace holds: %s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "map-repo-staging") {
		t.Errorf("staging survived a failed extraction: %s", res.Stdout)
	}

	// The next pass, with the obstruction gone, materializes normally.
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "rm -f " + shellQuote(blocked)}); err != nil {
		t.Fatalf("remove the blocking file: %v", err)
	}
	h.runPass(t)
	if got := readSandboxFile(t, sb, blocked+"/repo/README.md"); got != "x\n" {
		t.Errorf("README.md = %q, want the retry to have materialized", got)
	}
}

// TestRepoKeepsWhatALegacyMountPathHolds 🔍: a repository stored before #922
// keeps its literal path (#930), and a clone there must not take what the
// image ships at it. At /usr on a merged-/usr image, that is the shell itself.
// Outside the platform's directories a clone takes only an absent path or an
// empty directory. At a path that holds files, at a file, or at a symlink it
// fails as a clone error, fetches nothing and removes nothing. An empty
// directory whose fetch fails is left as it was. Inside the platform's
// directories, where the files and skills steps write too, a tree without its
// .git is still replaced whole.
func TestRepoKeepsWhatALegacyMountPathHolds(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	missing := newGitFixture(t, map[string]string{"README.md": "x\n"})
	missing.status.Store(http.StatusNotFound)
	h, provider := dockerRepoHarness(t)
	sb := adopt(t, provider, h)
	plant, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: strings.Join([]string{
		"mkdir -p /opt/EOF /opt/empty /opt/etarget /opt/gone /workspace/stale /tmp/stale /mnt/session/uploads/repo",
		"echo image > /opt/EOF/keep",
		"echo image > /opt/file",
		"ln -s /usr/share /opt/link",
		"ln -s /nonexistent /opt/dangling",
		"ln -s /opt/etarget /opt/elink",
		"echo old > /workspace/stale/old",
		"echo old > /tmp/stale/old",
		"echo old > /mnt/session/uploads/repo/old",
	}, " && ")})
	if err != nil || plant.ExitCode != 0 {
		t.Fatalf("plant the paths: %v %+v", err, plant)
	}
	// /opt/EOF is a name the network arm's text match reads as the git host's
	// failure. The refusal quotes no path, so the name guards only a refusal
	// that would.
	kept := []string{"/opt/EOF", "/opt/dangling", "/opt/elink", "/opt/file", "/opt/link"}
	cloned := []string{"/mnt/session/uploads/repo", "/opt/absent", "/opt/empty", "/tmp/stale", "/workspace/stale"}
	for i, mount := range append(append([]string(nil), kept...), cloned...) {
		h.seedRepoResource(t, "sesrsc_legacy"+string(rune('a'+i)), fx.url(), mount, "ghp_fixture", nil)
	}
	h.seedRepoResource(t, "sesrsc_legacygone", missing.url(), "/opt/gone", "ghp_fixture", nil)

	h.runPass(t)

	want := map[string]any{"/opt/gone": repoOutcomeNotFound}
	for _, mount := range kept {
		want[mount] = repoOutcomeInternal
	}
	got := map[string]any{}
	for _, e := range h.repoErrors(t) {
		got[e["mount_path"].(string)] = e["reason"]
		if e["reason"] == repoOutcomeInternal && e["message"] != repoMountOccupiedMessage {
			t.Errorf("%s: message = %q, want the mount path's", e["mount_path"], e["message"])
		}
	}
	if !maps.Equal(got, want) {
		t.Errorf("clone errors (mount: reason) = %v, want %v", got, want)
	}
	if n := fx.clones.Load(); n != int64(len(cloned)) {
		t.Errorf("clones = %d, want %d — a refused path still fetched the repository", n, len(cloned))
	}
	for _, mount := range cloned {
		if got := readSandboxFile(t, sb, mount+"/README.md"); got != "x\n" {
			t.Errorf("%s/README.md = %q, want the checkout", mount, got)
		}
	}
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "cat /opt/EOF/keep /opt/file; readlink /opt/link /opt/dangling /opt/elink; test -d /opt/gone && echo gone-kept; " +
			"ls -A /opt /workspace /workspace/stale /tmp /tmp/stale /mnt/session/uploads /mnt/session/uploads/repo"})
	if err != nil {
		t.Fatalf("inspect the sandbox: %v", err)
	}
	if want := "image\nimage\n/usr/share\n/nonexistent\n/opt/etarget\ngone-kept\n"; !strings.HasPrefix(res.Stdout, want) {
		t.Errorf("what the refused paths held = %q, want it to start %q", res.Stdout, want)
	}
	for _, gone := range []string{"old", "map-repo-staging", "repo-sesrsc_legacy"} {
		if strings.Contains(res.Stdout, gone) {
			t.Errorf("%q is left in the sandbox:\n%s", gone, res.Stdout)
		}
	}
}

// TestRepoProbesThroughAFloodedStatus: an image's startup can push
// mountOccupied's framed answer, and the probe exec's exit record with it,
// out of the output (#860). That is no answer, so the clone goes ahead, and the extraction judges the path itself: an
// empty directory clones, and one that holds a file is refused and kept, as
// is a dangling symlink. Each refusal reads as the probe's would, with the
// mount path's message.
func TestRepoProbesThroughAFloodedStatus(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider, flooding := floodingRepoHarness(t)
	sb := adopt(t, provider, h)
	if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Command: "mkdir -p /opt/EOF /opt/empty && echo image > /opt/EOF/keep && ln -s /nonexistent /opt/dangling"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("plant the paths: %v %+v", err, res)
	}
	h.seedRepoResource(t, "sesrsc_vfull", fx.url(), "/opt/EOF", "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_vempty", fx.url(), "/opt/empty", "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_vlink", fx.url(), "/opt/dangling", "ghp_fixture", nil)

	flooding.failVacant.Store(3)
	h.runPass(t)

	if n := flooding.failVacant.Load(); n > 0 {
		t.Fatalf("%d flooded probes left unused: the probe never ran through the flood", n)
	}
	got := map[string]bool{}
	for _, e := range h.repoErrors(t) {
		got[e["mount_path"].(string)] = e["reason"] == repoOutcomeInternal &&
			e["message"] == repoMountOccupiedMessage
	}
	if want := map[string]bool{"/opt/EOF": true, "/opt/dangling": true}; !maps.Equal(got, want) {
		t.Fatalf("clone errors (mount: reason %s with the mount path's message) = %v, want %v",
			repoOutcomeInternal, got, want)
	}
	if got := readSandboxFile(t, sb, "/opt/empty/README.md"); got != "x\n" {
		t.Errorf("/opt/empty/README.md = %q, want the checkout", got)
	}
	if got := readSandboxFile(t, sb, "/opt/EOF/keep"); got != "image\n" {
		t.Errorf("/opt/EOF/keep = %q, want what the image put there", got)
	}
	if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "readlink /opt/dangling"}); err != nil ||
		res.Stdout != "/nonexistent\n" {
		t.Errorf("readlink /opt/dangling = %+v %v, want the link kept", res, err)
	}
}

// hookedProvider provisions sandboxes that run hook just before each
// repository extraction, as an image's startup hook runs before every
// script (#860), after mountOccupied has looked at the mount. startup, when
// set, is put just after the first sandbox.ScriptPreamble in every platform
// script, which a framed script carries too, in that script's own shell: a
// stand-in for what an image's BASH_ENV leaves there.
type hookedProvider struct {
	sandbox.Provider
	hook, startup string
	// startupOn, when set, runs startup only in a command it accepts, as a
	// startup file that fails now and then does.
	startupOn func(command string) bool
	// streamErr and extractErr, when set, fail the tar's upload and the
	// extraction's exec, as a sandbox whose transport drops does.
	streamErr, extractErr error
}

func (p *hookedProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &hookedSandbox{Sandbox: sb, p: p}, nil
}

type hookedSandbox struct {
	sandbox.Sandbox
	p *hookedProvider
}

func (s *hookedSandbox) WriteFileStream(ctx context.Context, p string, src io.Reader, size int64) error {
	if s.p.streamErr != nil {
		return s.p.streamErr
	}
	return s.Sandbox.WriteFileStream(ctx, p, src, size)
}

func (s *hookedSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if s.p.startup != "" && (s.p.startupOn == nil || s.p.startupOn(req.Command)) {
		req.Command = strings.Replace(req.Command, sandbox.ScriptPreamble, sandbox.ScriptPreamble+s.p.startup, 1)
	}
	if s.p.extractErr != nil && strings.Contains(req.Command, " && tar -xf ") {
		return sandbox.ExecResult{}, s.p.extractErr
	}
	if s.p.hook != "" && strings.Contains(req.Command, " && tar -xf ") {
		if res, err := s.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: s.p.hook}); err != nil || res.ExitCode != 0 {
			return sandbox.ExecResult{}, errors.Join(err, errors.New("the hook did not run: "+res.Stderr))
		}
	}
	return s.Sandbox.Exec(ctx, req)
}

// TestRepoVacatesAgainAtTheExtraction: a hook that makes the mount again
// between mountOccupied's look and the extraction is judged by the extraction
// itself. Without that, `mv` puts the checkout inside the directory and
// reports success, and every later pass refuses the path.
func TestRepoVacatesAgainAtTheExtraction(t *testing.T) {
	for _, tc := range []struct {
		name, hook string
		lands      bool
	}{
		{"AnEmptyDirectoryIsReplaced", "mkdir -p /opt/hooked", true},
		{"AFileInItIsKept", "mkdir -p /opt/hooked && echo hook > /opt/hooked/keep", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			h, provider := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
				return &hookedProvider{Provider: p, hook: tc.hook}
			})
			sb := adopt(t, provider, h)
			h.seedRepoResource(t, "sesrsc_hooked", fx.url(), "/opt/hooked", "ghp_fixture", nil)

			h.runPass(t)

			errs := h.repoErrors(t)
			res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "ls -A /opt /opt/hooked"})
			if err != nil {
				t.Fatalf("inspect the sandbox: %v", err)
			}
			if strings.Contains(res.Stdout, "map-repo-staging") {
				t.Errorf("staging is left in the sandbox:\n%s", res.Stdout)
			}
			if tc.lands {
				if len(errs) != 0 {
					t.Errorf("clone errors = %v, want none", errs)
				}
				if got := readSandboxFile(t, sb, "/opt/hooked/README.md"); got != "x\n" {
					t.Errorf("/opt/hooked/README.md = %q, want the checkout at the mount", got)
				}
				return
			}
			if len(errs) != 1 || errs[0]["message"] != repoMountOccupiedMessage {
				t.Errorf("clone errors = %v, want exactly one, with the mount path's message", errs)
			}
			if got := readSandboxFile(t, sb, "/opt/hooked/keep"); got != "hook\n" {
				t.Errorf("/opt/hooked/keep = %q, want what the hook put there", got)
			}
		})
	}
}

// TestRepoProbeThatCannotAnswerGoesAhead: a listing can fail, as on a
// directory the sandbox user may not read, and an image's startup can leave
// attributes on any variable name. Neither says the path holds something, so
// the clone goes ahead and the extraction's rmdir decides. Here the startup
// makes `ls` fail and leaves a readonly `e` in every platform script: an
// empty directory clones, and one holding a file is refused and kept.
func TestRepoProbeThatCannotAnswerGoesAhead(t *testing.T) {
	for _, tc := range []struct {
		name, plant string
		lands       bool
	}{
		{"AnEmptyDirectoryClones", "mkdir -p /opt/unlisted", true},
		{"AFileInItIsKept", "mkdir -p /opt/unlisted && echo image > /opt/unlisted/keep", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			h, provider := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
				return &hookedProvider{Provider: p, startup: "ls() { return 2; }; readonly e=hook; "}
			})
			sb := adopt(t, provider, h)
			if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: tc.plant}); err != nil || res.ExitCode != 0 {
				t.Fatalf("plant the directory: %v %+v", err, res)
			}
			h.seedRepoResource(t, "sesrsc_unlisted", fx.url(), "/opt/unlisted", "ghp_fixture", nil)

			h.runPass(t)

			errs := h.repoErrors(t)
			if tc.lands {
				if len(errs) != 0 {
					t.Errorf("clone errors = %v, want none: the probe gave no answer", errs)
				}
				if got := readSandboxFile(t, sb, "/opt/unlisted/README.md"); got != "x\n" {
					t.Errorf("/opt/unlisted/README.md = %q, want the checkout", got)
				}
				return
			}
			if len(errs) != 1 || errs[0]["message"] != repoMountOccupiedMessage {
				t.Errorf("clone errors = %v, want exactly one, with the mount path's message", errs)
			}
			if got := readSandboxFile(t, sb, "/opt/unlisted/keep"); got != "image\n" {
				t.Errorf("/opt/unlisted/keep = %q, want what the image put there", got)
			}
		})
	}
}

// TestRepoProbeAnswersOnlyInItsFrame: the probe reads a path as occupied only
// when its framed answer says so. A shell that an image's startup ends first
// gives no answer, whatever its exit status, the extraction's own refusal
// status included, and whatever it printed before the frame. The probe names
// no variable, so a readonly name the startup leaves does not silence it.
func TestRepoProbeAnswersOnlyInItsFrame(t *testing.T) {
	h, provider := dockerRepoHarness(t)
	sb := adopt(t, provider, h)
	ctx := context.Background()
	if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "echo image > /opt/file && mkdir -p /opt/full && echo image > /opt/full/keep"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("plant the paths: %v %+v", err, res)
	}
	for _, mount := range []string{"/opt/file", "/opt/full"} {
		if !mountOccupied(ctx, sb, mount) {
			t.Errorf("the probe read %s, which holds something, as vacant", mount)
		}
	}
	for _, startup := range []string{"exit 3; ", "exit " + strconv.Itoa(occupiedStatus) + "; ", "echo occupied; exit 3; "} {
		failing := &hookedSandbox{Sandbox: sb, p: &hookedProvider{startup: startup}}
		for _, mount := range []string{"/opt/absent", "/opt/file"} {
			if mountOccupied(ctx, failing, mount) {
				t.Errorf("a probe whose startup ran %q read %s as occupied", startup, mount)
			}
		}
	}
	readonlyE := &hookedSandbox{Sandbox: sb, p: &hookedProvider{startup: "readonly e=hook; "}}
	if !mountOccupied(ctx, readonlyE, "/opt/full") {
		t.Error("a startup's readonly e silenced the probe: /opt/full read as vacant")
	}
}

// TestRepoVacancyProbeIsBounded: the probe runs before the clone's deadline is
// set, so it carries a timeout of its own, and an answer from a probe that
// timed out is no answer.
func TestRepoVacancyProbeIsBounded(t *testing.T) {
	if repoVacancyTimeout <= 0 {
		t.Fatalf("repoVacancyTimeout = %s, want a bound: zero means no limit", repoVacancyTimeout)
	}
	for _, timedOut := range []bool{false, true} {
		sb := &fakeSandbox{execHook: func(sandbox.ExecRequest) *sandbox.ExecResult {
			return &sandbox.ExecResult{Stdout: "occupied\n", TimedOut: timedOut}
		}}
		if got := mountOccupied(context.Background(), sb, "/opt/repo"); got == timedOut {
			t.Errorf("timed out %v: occupied = %v, want %v", timedOut, got, !timedOut)
		}
		if len(sb.execTimeouts) != 1 || sb.execTimeouts[0] != repoVacancyTimeout {
			t.Errorf("probe timeouts = %v, want [%s]", sb.execTimeouts, repoVacancyTimeout)
		}
	}
}

// TestRepoProbeWithNoAnswerFetches: under an image's startup that ends every
// shell first, the probe gives no answer, so an absent path is fetched and
// the extraction's failure is reported with the generic message, not refused
// before the fetch as an occupied path.
func TestRepoProbeWithNoAnswerFetches(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
		return &hookedProvider{Provider: p, startup: "exit 3; "}
	})
	h.seedRepoResource(t, "sesrsc_noanswer", fx.url(), "/opt/absent", "ghp_fixture", nil)

	h.runPass(t)

	if errs := h.repoErrors(t); len(errs) != 1 || errs[0]["reason"] != repoOutcomeInternal || errs[0]["message"] != repoCloneFailedMessage {
		t.Errorf("clone errors = %v, want exactly one, with reason %s and the generic message", errs, repoOutcomeInternal)
	}
	if n := fx.clones.Load(); n != 1 {
		t.Errorf("clones = %d, want 1: a probe with no answer must not refuse before the fetch", n)
	}
}

// TestRepoProbeAStartupEndsKeepsTheCheckout 🔍: the repository probe read an
// exit of 1 as `test -e` saying the .git is not there. An image's startup
// that ends the shell first with 1 — a `set -e` startup whose command failed
// — says nothing of the path, but read as absent: the pass cloned again and
// the extraction replaced the checkout, the agent's edits with it.
func TestRepoProbeAStartupEndsKeepsTheCheckout(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	prov := &hookedProvider{}
	h, provider := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
		prov.Provider = p
		return prov
	})
	h.seedRepoResource(t, "sesrsc_kept", fx.url(), repoMount, "ghp_fixture", nil)
	h.runPass(t)
	sb := adopt(t, provider, h)
	if err := sb.WriteFile(context.Background(), repoMount+"/edit.txt", []byte("mine")); err != nil {
		t.Fatalf("the agent's edit: %v", err)
	}

	// Only the shell of the .git's probe ends, whatever shape the probe
	// takes: the extraction would run whole.
	var ended atomic.Int32
	prov.startup = "exit 1; "
	prov.startupOn = func(command string) bool {
		if !strings.Contains(command, "test -e '"+repoMount+"/.git'") {
			return false
		}
		ended.Add(1)
		return true
	}
	h.runPass(t)
	if ended.Load() == 0 {
		t.Fatal("the startup ended no probe: the test asked nothing")
	}

	if got, err := sb.ReadFile(context.Background(), repoMount+"/edit.txt"); err != nil || string(got) != "mine" {
		t.Errorf("the agent's edit = %q, %v; want %q: the checkout was replaced", got, err, "mine")
	}
	if n := fx.clones.Load(); n != 1 {
		t.Errorf("clones = %d, want 1: a probe the startup ended must not read as absent", n)
	}
	if errs := h.repoErrors(t); len(errs) != 0 {
		t.Errorf("clone errors = %v, want none", errs)
	}
}

// TestRepoSandboxFailureIsNotTheHosts: a failure after the fetch is the
// sandbox's, never the git host's, though its text can read like one: a
// sandbox transport that drops reports an unexpected EOF, whether it was
// shipping the tar or running the extraction. A deadline that ends the
// extraction is still a timeout.
func TestRepoSandboxFailureIsNotTheHosts(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		prov       hookedProvider
	}{
		{"ShippingTheTar", repoOutcomeInternal, hookedProvider{streamErr: io.ErrUnexpectedEOF}},
		{"RunningTheExtraction", repoOutcomeInternal, hookedProvider{extractErr: io.ErrUnexpectedEOF}},
		{"TimingOutInTheExtraction", repoOutcomeTimeout, hookedProvider{extractErr: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
				prov := tc.prov
				prov.Provider = p
				return &prov
			})
			h.seedRepoResource(t, "sesrsc_transport", fx.url(), repoMount, "ghp_fixture", nil)

			h.runPass(t)

			if errs := h.repoErrors(t); len(errs) != 1 || errs[0]["reason"] != tc.want || errs[0]["message"] != repoCloneFailedMessage {
				t.Errorf("clone errors = %v, want exactly one, with reason %s and the generic message", errs, tc.want)
			}
		})
	}
}

// TestMeteredFSChrootSharesTheBudget 🔍 covers the one property of the byte
// budget that the integration row above cannot see.
//
// TestRepoOversizeAbortsMidClone proves a repository past the budget is refused
// with `too_large` and ships no tar — but it would prove that just as well if
// the meter were bolted on after the clone, and just as well if Chroot handed
// back an unmetered filesystem: its fixture's oversize file lands in the
// worktree, which is metered either way. The two properties that make the meter
// what its comment claims are that a write is refused *at* the boundary rather
// than after it, and that the .git chroot go-git takes counts against the same
// budget as the tree — otherwise the objects, which are the bulk of a clone,
// escape the meter entirely and only the checkout is bounded.
func TestMeteredFSChrootSharesTheBudget(t *testing.T) {
	var spent atomic.Int64
	const limit = 100
	root := newMeteredFS(osfs.New(t.TempDir()), &spent, limit)

	sub, err := root.Chroot("objects")
	if err != nil {
		t.Fatalf("Chroot: %v", err)
	}
	if _, err := writeTo(t, sub, "pack", 80); err != nil {
		t.Fatalf("an 80-byte write inside the budget failed: %v", err)
	}
	// 80 already spent beneath the chroot, so 40 more anywhere must not fit.
	if _, err := writeTo(t, root, "tree-file", 40); !errors.Is(err, errCloneTooLarge) {
		t.Errorf("writing 40 bytes after 80 gave %v, want errCloneTooLarge — "+
			"the chroot is spending a budget of its own", err)
	}
	if got := spent.Load(); got != 120 {
		t.Errorf("spent = %d, want 120 (both writes counted against one budget)", got)
	}
}

// TestCloneSpoolSymlinks 🔍 covers the two things a symlink does that a regular
// file does not, both of which the meter and the checkout got wrong.
//
// A git tree may hold symlinks, and go-git checks a mode-120000 entry out by
// calling the filesystem's Symlink directly — not Create, so the byte meter
// never saw one: a repository of hundreds of thousands of entries pointing at
// one stored blob costs almost nothing in objects and lands gigabytes of link
// data in the spool, past a budget that never counted it.
//
// And go-billy's default (deprecated) ChrootOS *rewrites* an absolute target to
// sit under its own root, so `hostfile -> /etc/hosts` would reach the sandbox
// pointing at the executor's temporary spool path — a dangling link inside a
// clone that reported success, which `.git` then makes permanently trusted by
// the idempotence probe. A checkout must carry a symlink's target verbatim and
// let the sandbox resolve it in its own namespace, which is what git does.
func TestCloneSpoolSymlinks(t *testing.T) {
	var spent atomic.Int64
	dir, err := filepath.EvalSymlinks(t.TempDir()) // newCloneFS's root holds no symlink
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	fs := newCloneFS(dir, &spent, 1<<20)

	const target = "/etc/hosts"
	if err := fs.Symlink(target, "hostfile"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dir, "hostfile"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != target {
		t.Errorf("the checkout's symlink points at %q, want %q verbatim — an "+
			"executor-local path would reach the sandbox dangling", got, target)
	}
	if spent.Load() < int64(len(target)) {
		t.Errorf("spent = %d after a %d-byte symlink target; symlink data escapes the budget",
			spent.Load(), len(target))
	}
	// And the budget is enforced on them, not merely counted.
	big := "/" + strings.Repeat("p", 1<<20)
	if err := fs.Symlink(big, "huge"); !errors.Is(err, errCloneTooLarge) {
		t.Errorf("a symlink past the budget gave %v, want errCloneTooLarge", err)
	}
}

// TestMeteredFSRefusesTheWriteThatCrosses 🔍 is the in-flight half: the write
// that would cross the budget is refused and its bytes never reach the disk, so
// a repository far past the cap costs the cap and not its own size.
func TestMeteredFSRefusesTheWriteThatCrosses(t *testing.T) {
	var spent atomic.Int64
	dir := t.TempDir()
	fs := newMeteredFS(osfs.New(dir), &spent, 100)

	name, err := writeTo(t, fs, "big", 500)
	if !errors.Is(err, errCloneTooLarge) {
		t.Fatalf("a 500-byte write against a 100-byte budget gave %v, want errCloneTooLarge", err)
	}
	st, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("stat the refused file: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("the refused write left %d bytes on disk; the budget is measured "+
			"as bytes land, so nothing past it may be written", st.Size())
	}
}

// writeTo creates name on fs and writes n bytes to it in one call, returning the
// name so the caller can inspect what actually landed.
func writeTo(t *testing.T, fs billy.Filesystem, name string, n int) (string, error) {
	t.Helper()
	f, err := fs.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer f.Close()
	_, err = f.Write(make([]byte, n))
	return name, err
}

// TestRepoRefusesADestructiveMountPath 🔍: the executor validates the mount
// path itself before building a command that runs `rm -rf` on it.
//
// Create-time validation never yields the first six, since it roots every
// path under /workspace (#922): reaching the executor, they come from another
// route or a tampered row. The staging and sentinel paths are ones create
// accepts and only the executor knows to refuse. The platform directories,
// and the directories holding them, are ones a repository stored before
// #922 may still name (`/mnt/memory` only one stored before plan 36). In
// every case the second check earns its place,
// because the operation on the other side is destructive and irreversible.
// `/workspace/repo/..` cleans to `/workspace` and would delete the whole
// workspace; `/tmp` is the platform's own sandbox staging root and would
// take every in-flight tar with it.
func TestRepoRefusesADestructiveMountPath(t *testing.T) {
	// An empty mount_path is not here: a resource missing one is malformed
	// rather than dangerous, and materializeRepos' own filter drops it with the
	// entries missing an id or a url, before any of this.
	for _, mount := range []string{
		"/workspace/repo/..", "/tmp", "/tmp/", "/", "relative/path",
		// The sandbox workdir, which holds every other mount.
		"/workspace",
		// Another repository's staging path, or a path below one, which this
		// one's extraction removes twice.
		"/workspace/.x.map-repo-staging", "/workspace/.x.map-repo-staging/work",
		// The materialization sentinels, which the skills and files steps
		// write as files.
		"/workspace/skills/.materialized", "/workspace/skills/.materialized/x",
		"/workspace/.files_materialized", "/workspace/.files_materialized/x",
		// A directory the platform keeps, or one holding it, which a
		// repository stored before #922 may still name.
		"/mnt", "/mnt/session", "/mnt/session/uploads", "/mnt/session/outputs", "/mnt/memory",
		"/var/lib", "/var/lib/map-shell",
	} {
		t.Run(mount, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			h.seedRepoResource(t, "sesrsc_evil", fx.url(), mount, "ghp_fixture", nil)

			h.runPass(t)

			errs := h.repoErrors(t)
			if len(errs) != 1 || errs[0]["reason"] != repoOutcomeInternal {
				t.Fatalf("clone errors = %v, want exactly one internal for %q", errs, mount)
			}
			// Refused before any work: no clone, so extractRepo — the only
			// thing that builds the `rm -rf` — is never reached.
			if fx.clones.Load() != 0 {
				t.Error("the repository was cloned before its mount path was judged")
			}
		})
	}
}

// TestRepoAdmitsAPathBesideAPlatformDirectory: the platform-directory
// refusal is for a directory the platform keeps and the directories holding
// it, by whole path components. A path beside one, or below one, passes the
// guard.
func TestRepoAdmitsAPathBesideAPlatformDirectory(t *testing.T) {
	for _, mount := range []string{
		"/mnt/sess", "/mnt/data", "/mnt/session/uploads/repo", "/mnt/session/outputs/report",
		"/mnt/memory/x", "/var/lib/map", "/opt/repo", "/workspace/repo",
	} {
		if err := (&Executor{}).safeRepoMount(mount); err != nil {
			t.Errorf("safeRepoMount(%q) = %v, want it admitted", mount, err)
		}
	}
}

// TestRepoRefusesTheRepositoryRootBesideAWorkdir: under a workdir outside
// /workspace, the workdir check does not cover /workspace, where every other
// repository mounts. A repository stored before #922 at that literal path
// would remove them all.
func TestRepoRefusesTheRepositoryRootBesideAWorkdir(t *testing.T) {
	e := &Executor{cfg: Config{Workdir: "/srv/work"}}
	if err := e.safeRepoMount("/workspace"); err == nil {
		t.Error("safeRepoMount(/workspace) admitted it under a workdir at /srv/work")
	}
	if err := e.safeRepoMount("/workspace/repo"); err != nil {
		t.Errorf("safeRepoMount(/workspace/repo) = %v, want it admitted", err)
	}
}

// TestRepoSharesTheSkillsDirectory 🔍: a repository may share the skills
// directory with the run's skills, as the reference was recorded doing (#922,
// plan 64). Repositories are cloned before skills are written, so a clone,
// which replaces its mount whole, never removes a skill landed in the same
// pass; and a clone at or below the directory drops the skills sentinel, so a
// re-clone in a later pass has the skills written again. The `pdf` mount is
// the case the sentinel's own SKILL.md probe misses: the checkout brings a
// SKILL.md of its own there. Where both write one file, the skill's wins.
func TestRepoSharesTheSkillsDirectory(t *testing.T) {
	for _, mount := range []string{"/workspace/skills", "/workspace/skills/pdf"} {
		t.Run(mount, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "repo\n", "SKILL.md": "the checkout's\n"})
			h, provider := dockerRepoHarness(t)
			h.seedSkill(t, "skill_root", "1", "pdf", map[string]string{"SKILL.md": "# pdf"})
			h.refSkills(t, [2]string{"skill_root", "1"})
			h.seedRepoResource(t, "sesrsc_skills", fx.url(), mount, "ghp_fixture", nil)

			h.runPass(t)
			sb := adopt(t, provider, h)
			both := func(when string) {
				t.Helper()
				if got := readSandboxFile(t, sb, "/workspace/skills/pdf/SKILL.md"); got != "# pdf" {
					t.Errorf("%s: skills/pdf/SKILL.md = %q, want the skill's", when, got)
				}
				if got := readSandboxFile(t, sb, mount+"/README.md"); got != "repo\n" {
					t.Errorf("%s: README.md = %q, want the checkout's", when, got)
				}
			}
			both("first pass")

			// The agent removes the checkout; the next pass clones it again,
			// over the skill.
			if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{
				Command: "rm -rf " + shellQuote(mount)}); err != nil {
				t.Fatalf("remove the checkout: %v", err)
			}
			h.runPass(t)
			if got := fx.clones.Load(); got != 2 {
				t.Fatalf("clones = %d, want 2: the removed checkout cloned again", got)
			}
			both("after a re-clone")
		})
	}
}

// TestRepoDropsTheSkillsSentinelWithTheTreeItReplaces 🔍: a clone at or below
// the skills directory drops the skills sentinel inside the extraction that
// replaces the old tree, just before the old tree goes (#922). So a removal
// that fails stops the extraction with nothing lost, and the client sees a
// clone failure; a clone that fails before the extraction leaves the sentinel,
// and the agent's edits to its skills, alone; a checkout's own
// `.materialized` never lands as the marker; and a run with no skills drops
// nothing.
func TestRepoDropsTheSkillsSentinelWithTheTreeItReplaces(t *testing.T) {
	setup := func(t *testing.T, skill, repo map[string]string, mount string, checkout map[string]any) (*harness, sandbox.Sandbox, *sentinelProvider, *gitFixture) {
		t.Helper()
		wrapped := &sentinelProvider{}
		h, provider := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
			wrapped.Provider = p
			return wrapped
		})
		if skill != nil {
			h.seedSkill(t, "skill_root", "1", "pdf", skill)
			h.refSkills(t, [2]string{"skill_root", "1"})
		}
		fx := newGitFixture(t, repo)
		h.seedRepoResource(t, "sesrsc_skills", fx.url(), mount, "ghp_fixture", checkout)
		h.runPass(t)
		return h, adopt(t, provider, h), wrapped, fx
	}
	exec := func(t *testing.T, sb sandbox.Sandbox, command string) {
		t.Helper()
		if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: command}); err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %+v, %v", command, res, err)
		}
	}
	pdf := map[string]string{"SKILL.md": "# pdf", "ref.md": "ref"}
	checkout := map[string]string{"README.md": "repo\n", "SKILL.md": "the checkout's\n"}

	t.Run("a refused drop", func(t *testing.T) {
		const mount = "/workspace/skills/pdf"
		h, sb, wrapped, _ := setup(t, pdf, checkout, mount, nil)
		// The agent edits the tree and removes only its .git; the re-clone's
		// drop fails, which must leave the old tree as it was.
		exec(t, sb, "echo edited > "+shellQuote(mount+"/README.md")+" && rm -rf "+shellQuote(mount+"/.git"))
		wrapped.refuse.Store(true)
		h.runPass(t)
		if n := h.cloneErrors(t); n != 1 {
			t.Fatalf("clone errors = %d, want 1: a drop that fails fails the clone where the client sees it", n)
		}
		if got := readSandboxFile(t, sb, mount+"/README.md"); got != "edited\n" {
			t.Errorf("README.md = %q, want the agent's edit: a failed drop removes nothing", got)
		}
		wrapped.refuse.Store(false)
		h.runPass(t)
		for file, want := range map[string]string{"SKILL.md": "# pdf", "ref.md": "ref", "README.md": "repo\n"} {
			if got := readSandboxFile(t, sb, mount+"/"+file); got != want {
				t.Errorf("%s = %q, want %q", file, got, want)
			}
		}
	})

	// The drop waits for the new tree to be staged: an unpack that fails keeps
	// the sentinel, and so the agent's edits to its skills.
	t.Run("a failed unpack", func(t *testing.T) {
		const mount, edited = "/workspace/skills/other", "/workspace/skills/pdf/SKILL.md"
		h, sb, wrapped, _ := setup(t, pdf, checkout, mount, nil)
		exec(t, sb, "echo edited > "+shellQuote(edited)+" && rm -rf "+shellQuote(mount+"/.git"))
		wrapped.failUnpack.Store(true)
		h.runPass(t)
		if n := h.cloneErrors(t); n != 1 {
			t.Fatalf("clone errors = %d, want 1 from the failed unpack", n)
		}
		if got := readSandboxFile(t, sb, edited); got != "edited\n" {
			t.Errorf("SKILL.md = %q, want the agent's edit: an unpack that fails drops nothing", got)
		}
	})

	t.Run("a clone that fails first", func(t *testing.T) {
		const edited = "/workspace/skills/pdf/SKILL.md"
		h, sb, _, _ := setup(t, pdf, checkout, "/workspace/skills/other",
			map[string]any{"type": "branch", "name": "no-such-branch"})
		if n := h.cloneErrors(t); n != 1 {
			t.Fatalf("clone errors = %d, want 1 from the missing branch", n)
		}
		exec(t, sb, "echo edited > "+shellQuote(edited))
		h.runPass(t)
		if got := readSandboxFile(t, sb, edited); got != "edited\n" {
			t.Errorf("SKILL.md = %q, want the agent's edit: a clone that never reached the sandbox drops nothing", got)
		}
	})

	t.Run("a checkout's own marker", func(t *testing.T) {
		forged := map[string]string{
			"README.md":     "repo\n",
			".materialized": string(skills.Sentinel([]skills.Resolved{{ID: "skill_root", Version: "1"}})),
			"pdf/SKILL.md":  "the checkout's\n",
		}
		_, sb, _, _ := setup(t, pdf, forged, "/workspace/skills", nil)
		for file, want := range map[string]string{"pdf/SKILL.md": "# pdf", "pdf/ref.md": "ref", "README.md": "repo\n"} {
			if got := readSandboxFile(t, sb, "/workspace/skills/"+file); got != want {
				t.Errorf("%s = %q, want %q", file, got, want)
			}
		}
	})

	// In a run without skills nothing drops a checkout's own file at the
	// sentinel's path as it lands. Only an idle checkpoint strips it.
	t.Run("a skill-free checkout's own marker", func(t *testing.T) {
		tracked := map[string]string{"README.md": "repo\n", ".materialized": "tracked\n"}
		_, sb, _, _ := setup(t, nil, tracked, "/workspace/skills", nil)
		if got := readSandboxFile(t, sb, "/workspace/skills/.materialized"); got != "tracked\n" {
			t.Errorf(".materialized = %q, want the checkout's own file", got)
		}
	})

	t.Run("skill-free", func(t *testing.T) {
		_, _, wrapped, fx := setup(t, nil, checkout, "/workspace/skills/pdf", nil)
		if fx.clones.Load() != 1 {
			t.Fatal("the repository was not cloned")
		}
		if n := wrapped.drops.Load(); n != 0 {
			t.Errorf("sentinel drops = %d, want none in a run with no skills", n)
		}
	})
}

// sentinelProvider's sandboxes count the commands that name the skills
// sentinel and, while refuse is set, run each with its removal replaced by
// `false`, as a removal that fails would. While failUnpack is set, a clone's
// extraction fails at its unpack.
type sentinelProvider struct {
	sandbox.Provider
	refuse, failUnpack atomic.Bool
	drops              atomic.Int32
}

func (p *sentinelProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &sentinelSandbox{Sandbox: sb, p: p}, nil
}

type sentinelSandbox struct {
	sandbox.Sandbox
	p *sentinelProvider
}

func (s *sentinelSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	sentinel := shellQuote("/workspace/skills/.materialized")
	drop := "rm -rf " + sentinel
	if strings.Contains(req.Command, sentinel) {
		s.p.drops.Add(1)
		if s.p.refuse.Load() {
			req.Command = strings.Replace(req.Command, drop, "false", 1)
		}
	}
	if s.p.failUnpack.Load() && strings.Contains(req.Command, stagingSuffix) {
		req.Command = strings.Replace(req.Command, " && tar -xf ", " && false && tar -xf ", 1)
	}
	return s.Sandbox.Exec(ctx, req)
}

// TestRepoRefusesADirectoryHoldingTheWorkdir 🔍: under a configured workdir
// the control plane cannot see, a mount above it would remove the workdir —
// skills and every other mount — with it, so the executor refuses it too.
func TestRepoRefusesADirectoryHoldingTheWorkdir(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{Workdir: "/srv/work"})
	h.prov = prov
	h.seedRepoResource(t, "sesrsc_above", fx.url(), "/srv", "ghp_fixture", nil)

	h.runPass(t)

	if errs := h.repoErrors(t); len(errs) != 1 || errs[0]["reason"] != repoOutcomeInternal {
		t.Fatalf("clone errors = %v, want exactly one internal for /srv", errs)
	}
	if fx.clones.Load() != 0 {
		t.Error("the repository was cloned before its mount path was judged")
	}
}

// sweepSandbox records the commands the executor issues and can cancel the run
// at the one moment that matters: after the tar has landed in the sandbox and
// before the extraction that would remove it.
type sweepSandbox struct {
	*fakeSandbox
	mu      sync.Mutex
	cmds    []string
	onWrite func()
}

// Exec records only what it actually runs. The distinction is the row: a real
// sandbox refuses a command whose context is already done, so a sweep handed the
// cancelled context issues nothing — and a stub that recorded the attempt would
// call that a pass.
func (s *sweepSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.ExecResult{}, err
	}
	s.mu.Lock()
	s.cmds = append(s.cmds, strings.TrimPrefix(req.Command, sandbox.ScriptPreamble))
	s.mu.Unlock()
	return s.fakeSandbox.Exec(ctx, req)
}

func (s *sweepSandbox) WriteFileStream(ctx context.Context, p string, src io.Reader, size int64) error {
	err := s.fakeSandbox.WriteFileStream(ctx, p, src, size)
	if s.onWrite != nil {
		s.onWrite()
	}
	return err
}

func (s *sweepSandbox) issued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// TestRepoCancelledCloneSweepsTheSandbox 🔍: from the moment the tar lands, the
// sandbox holds up to the whole byte budget, and the only thing that removes it
// is the extraction command's own cleanup tail. A context cancelled in between —
// the clone deadline, or a lost lease — makes Exec return without ever running
// that tail, so a file the size of the repository would sit in the session's
// /tmp for the rest of its life. The sweep runs on a context the cancellation
// cannot reach, which is the whole point: the obvious implementation, reusing
// the cancelled one, cleans up nothing.
func TestRepoCancelledCloneSweepsTheSandbox(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	// The harness supplies the session, its sealed token and the cipher; the
	// sandbox under test is passed to materializeRepo directly, because the row
	// is about one repository's own cancellation path and not about a whole pass.
	sb := &sweepSandbox{fakeSandbox: &fakeSandbox{}}
	h := newHarness(t, &fakeSandbox{})
	h.seedRepoResource(t, "sesrsc_sweep", fx.url(), repoMount, "ghp_fixture", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb.onWrite = cancel

	// The credential read and the cipher's decrypt are their own round trips, and
	// the clone below is what the stall floor is measured against — so this row
	// also pins the report between them (#383).
	var reports int
	_, err := h.exec.materializeRepo(ctx, sb, h.sid, repoRef{
		ID: "sesrsc_sweep", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
	}, "", func() { reports++ })
	if err == nil {
		t.Fatal("the materialization reported success after its context was cancelled")
	}
	if reports != 1 {
		t.Errorf("progress reports = %d, want 1 (the sealed token resolved, before the clone that the floor is measured against)", reports)
	}

	const tar = repoStagingRoot + "/repo-sesrsc_sweep.tar"
	var swept bool
	for _, c := range sb.issued() {
		if strings.HasPrefix(c, "rm -rf ") && strings.Contains(c, tar) {
			swept = true
		}
	}
	if !swept {
		t.Errorf("the shipped tar %s was left in the sandbox; commands issued: %v", tar, sb.issued())
	}
}

// TestRepoZeroShaIsRefused 🔍: the null object id is 40 hex characters, so it
// passes create-time validation. go-git's CheckoutOptions.Validate quietly
// substitutes `master` for a zero hash rather than failing, which once mounted
// a branch nobody asked for under a resource advertising the sha; the remote
// refuses a want for it too. It is refused before the remote is asked, so the
// reason rests on neither. (GitHub's webhook payloads carry 40 zeros in
// `before`/`after` on branch create and delete, so a forwarding app reaches
// this without inventing anything.)
func TestRepoZeroShaIsRefused(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	_, _, cleanup, err := cloneToTar(context.Background(),
		repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "commit", Sha: strings.Repeat("0", 40)}},
		"ghp_fixture", 1<<30)
	defer cleanup()
	if err == nil {
		t.Fatal("a zero commit sha cloned successfully; the mount would hold a substituted branch")
	}
	if got := cloneReason(err); got != repoOutcomeCheckout {
		t.Errorf("cloneReason = %q, want %q — the sha is the caller's: %v", got, repoOutcomeCheckout, err)
	}
	// Refused before the remote is asked: a fetch of the null id fails there
	// too, but costs a round trip and rests on the host's answer.
	if n := fx.requests.Load(); n != 0 {
		t.Errorf("the null commit id was asked of the remote %d time(s)", n)
	}
}

// TestRepoNonCommitShaBlamesTheCheckout 🔍: the remote serves a tree or a blob
// by its id as readily as a commit, and go-git's checkout then fails with an
// error of its own. The id is the caller's, and names no commit.
func TestRepoNonCommitShaBlamesTheCheckout(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	head, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	commit, err := fx.repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	file, err := commit.File("README.md")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	ids := map[string]string{"tree": commit.TreeHash.String(), "blob": file.Hash.String()}
	// An annotated tag can point at a tree or a blob too, and names no commit
	// either; go-git's checkout refuses it with the same error of its own.
	for _, kind := range []string{"tree", "blob"} {
		tag, err := fx.repo.CreateTag("of-a-"+kind, plumbing.NewHash(ids[kind]), &git.CreateTagOptions{Message: kind,
			Tagger: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)}})
		if err != nil {
			t.Fatalf("tag of a %s: %v", kind, err)
		}
		ids["tag of a "+kind] = tag.Hash().String()
	}
	outer, err := fx.repo.CreateTag("of-a-tag-of-a-tree", plumbing.NewHash(ids["tag of a tree"]), &git.CreateTagOptions{
		Message: "tag of a tag", Tagger: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)}})
	if err != nil {
		t.Fatalf("tag of a tag of a tree: %v", err)
	}
	ids["tag of a tag of a tree"] = outer.Hash().String()
	for kind, id := range ids {
		t.Run(kind, func(t *testing.T) {
			_, _, cleanup, err := cloneToTar(context.Background(),
				repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "commit", Sha: id}}, "ghp_fixture", 1<<30)
			defer cleanup()
			if got := cloneReason(err); err == nil || got != repoOutcomeCheckout {
				t.Errorf("cloneReason = %q, want %q — a %s id names no commit: %v", got, repoOutcomeCheckout, kind, err)
			}
		})
	}
}

// TestRepoMalformedShaIsRefused 🔍: create admits only forty hex characters,
// but a stored row is read back here, and the commit fetch puts its sha into a
// refspec. Anything else would reach the remote as a branch name (`abc`) or
// break the refspec (`a:b`), so it is refused first, as the caller's.
func TestRepoMalformedShaIsRefused(t *testing.T) {
	for _, sha := range []string{"abc", strings.Repeat("a", 39), strings.Repeat("g", 40), "a:" + strings.Repeat("b", 38), strings.Repeat("c", 39) + "*"} {
		t.Run(sha, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			_, _, cleanup, err := cloneToTar(context.Background(),
				repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "commit", Sha: sha}}, "ghp_fixture", 1<<30)
			defer cleanup()
			if got := cloneReason(err); err == nil || got != repoOutcomeCheckout {
				t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeCheckout, err)
			}
			if n := fx.requests.Load(); n != 0 {
				t.Errorf("the remote was asked %d time(s)", n)
			}
		})
	}
}

// TestCloneReasonBlamesOurselvesForCancellation 🔍: nothing but this platform
// cancels a clone's context without a deadline — a lost lease, or a shutting
// down executor. go-git surfaces it as a *url.Error, which the network arm's
// fallback matches, so the platform's own withdrawal was being counted and
// logged as the git host's failure.
func TestCloneReasonBlamesOurselvesForCancellation(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	fx.stall.Store(int64(2 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	_, _, cleanup, err := cloneToTar(ctx, repoRef{URL: fx.url()}, "ghp_fixture", 1<<30)
	defer cleanup()
	if err == nil {
		t.Fatal("the clone survived its context being cancelled")
	}
	if got := cloneReason(err); got != repoOutcomeInternal {
		t.Errorf("cloneReason = %q, want %q — we cancelled it, not the remote: %v",
			got, repoOutcomeInternal, err)
	}
}

// TestRepoCloneStopsOnASpentBudgetAfterTheFetch 🔍: go-git applies a context to
// its transport and to nothing else — the post-fetch worktree reset, the commit
// checkout and the tar walk all run to completion whatever the budget says. Left
// unbounded, a repository that downloads inside its budget and then checks out
// or packs for hours holds the executor for as long as it likes, and
// EXECUTOR_REPO_CLONE_TIMEOUT bounds nothing but the fetch.
//
// The re-checks that bound those phases are deliberately redundant — each one
// downstream catches what the one before it would have — so no single check's
// removal changes the outcome, and what each row pins is the bound rather than
// any one of them. Measured rather than assumed: the default arm passes two,
// the one after the clone and the tar walk's, and goes red with both gone; each
// alone is invisible. The commit arm passes fetchCommit's, before its
// checkout, as well, and goes red with all three gone.
func TestRepoCloneStopsOnASpentBudgetAfterTheFetch(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	head, err := fx.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	for arm, checkout := range map[string]*repoCheckout{
		"default branch": nil,
		"commit":         {Type: "commit", Sha: head.Hash().String()},
	} {
		t.Run(arm, func(t *testing.T) {
			fx.clones.Store(0) // the other row's fetch would spend this one up front
			ctx := spentAfterFetch{Context: context.Background(), fetched: &fx.clones}
			tarPath, size, cleanup, err := cloneToTar(ctx, repoRef{URL: fx.url(), Checkout: checkout}, "ghp_fixture", 1<<30)
			defer cleanup()
			if err == nil {
				t.Fatalf("a clone whose budget expired after the fetch ran to completion, packing %s (%d bytes)",
					tarPath, size)
			}
			if got := cloneReason(err); got != repoOutcomeTimeout {
				t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeTimeout, err)
			}
		})
	}
}

// spentAfterFetch is live until the fixture is asked for a pack and spent from
// then on — a budget that expires around the transport's end rather than strictly
// after it, since the fixture counts the upload-pack POST when it arrives. Close
// enough, and the mutation says why: with the re-checks gone the clone still
// completes its transport and packs a tar, so nothing on this path consults
// ctx.Err() by itself. The Done channel never fires, which is what keeps net/http
// from aborting the request instead.
type spentAfterFetch struct {
	context.Context
	fetched *atomic.Int64
}

func (c spentAfterFetch) Err() error {
	if c.fetched.Load() > 0 {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// TestCloneReasonBlamesTheRightSide 🔍: `internal` means *this platform* broke,
// and it is the reason an operator reads as "go look at your own logs". Two
// reachable failures were landing there while being someone else's fault
// entirely — a caller's malformed branch name, and a remote answering 200 with
// something that is not a git response, which is what a captive portal, an
// outage page, or a proxy that swallowed the repository does.
func TestCloneReasonBlamesTheRightSide(t *testing.T) {
	// A wildcard would build a refspec that fetches every branch's tree before
	// the checkout fails, so each name is refused before the remote is asked.
	for _, name := range []string{"foo:bar", "*", "feature/*"} {
		t.Run("malformed branch name "+name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			_, _, cleanup, err := cloneToTar(context.Background(),
				repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "branch", Name: name}},
				"ghp_fixture", 1<<30)
			defer cleanup()
			if err == nil {
				t.Fatalf("branch %q cloned successfully", name)
			}
			if got := cloneReason(err); got != repoOutcomeCheckout {
				t.Errorf("cloneReason = %q, want %q — the ref is the caller's, not ours: %v",
					got, repoOutcomeCheckout, err)
			}
			if n := fx.requests.Load(); n != 0 {
				t.Errorf("the remote was asked %d time(s) about branch %q", n, name)
			}
		})
	}

	// A name git admits but go-git cannot carry is our limit, not the caller's
	// mistake, so it is refused before the remote is asked, as `internal`.
	// go-git builds a branch's refspec from Short(), which stops at a non-ASCII
	// space and would fetch another branch's tree. `EOF` in a name must not
	// read as the network's.
	nbsp, emsp := string(rune(0x00a0)), string(rune(0x2003))
	for _, name := range []string{"feat" + nbsp + "x", "feat" + emsp + "x", "fix/EOF" + nbsp + "crash"} {
		t.Run("branch name go-git cannot carry "+name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			fx.branch(t, "feat", map[string]string{"feat.txt": "x\n"})
			head, err := fx.repo.Head()
			if err != nil {
				t.Fatalf("Head: %v", err)
			}
			if err := fx.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), head.Hash())); err != nil {
				t.Fatalf("SetReference: %v", err)
			}
			_, _, cleanup, err := cloneToTar(context.Background(),
				repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "branch", Name: name}}, "ghp_fixture", 1<<30)
			defer cleanup()
			if got := cloneReason(err); err == nil || got != repoOutcomeInternal {
				t.Errorf("cloneReason = %q, want %q — the limit is go-git's: %v", got, repoOutcomeInternal, err)
			}
			if n := fx.requests.Load(); n != 0 {
				t.Errorf("the remote was asked %d time(s)", n)
			}
		})
	}
	// The caller named no branch at all, so go-git refusing the remote's
	// default branch name, which git admits, cannot be the caller's checkout.
	t.Run("default branch go-git refuses", func(t *testing.T) {
		fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
		head, err := fx.repo.Head()
		if err != nil {
			t.Fatalf("Head: %v", err)
		}
		at := plumbing.ReferenceName("refs/heads/feature/@/topic")
		for _, ref := range []*plumbing.Reference{
			plumbing.NewHashReference(at, head.Hash()),
			plumbing.NewSymbolicReference(plumbing.HEAD, at),
		} {
			if err := fx.repo.Storer.SetReference(ref); err != nil {
				t.Fatalf("SetReference: %v", err)
			}
		}
		_, _, cleanup, err := cloneToTar(context.Background(), repoRef{URL: fx.url()}, "ghp_fixture", 1<<30)
		defer cleanup()
		if got := cloneReason(err); err == nil || got != repoOutcomeInternal {
			t.Errorf("cloneReason = %q, want %q — no checkout was named: %v", got, repoOutcomeInternal, err)
		}
	})

	t.Run("remote answers 200 with a non-git body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
		}))
		defer srv.Close()

		_, _, cleanup, err := cloneToTar(context.Background(),
			repoRef{URL: srv.URL + "/o/r.git"}, "ghp_fixture", 1<<30)
		defer cleanup()
		if err == nil {
			t.Fatal("an HTML body cloned successfully")
		}
		if got := cloneReason(err); got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q — the remote spoke, just not git: %v",
				got, repoOutcomeNetwork, err)
		}
	})
}

// TestRepoCloneErrorNeverQuotesTheToken 🔍 is the w-token-sweep rule applied to
// the one surface that can still reach a log: a clone's error text.
//
// go-git copies up to 1 KiB of a failing response's body into the error it
// returns, and that error is logged when a repository does not materialize. A
// git host that quotes the credentials it rejected — as sent, or decoded, which
// a host that validates them has already done — therefore hands us the token
// inside an error message. The token is scrubbed in both forms before the error
// leaves the clone, and the classification the caller does over it must survive
// the scrub, or a redacted auth failure would be reported as a network one.
func TestRepoCloneErrorNeverQuotesTheToken(t *testing.T) {
	const token = "ghp_ERROR-ECHO-SWEEP-9f3a"
	basic := base64.StdEncoding.EncodeToString([]byte(tokenUsername + ":" + token))
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	fx.echoAuth.Store(true)

	for _, tc := range []struct {
		name       string
		status     int
		wantReason string
	}{
		// The 401 is the row that exercises the scrub: go-git splices a failing
		// response's body into the error it builds for 401/403/404 and for
		// nothing else — every other status becomes an UnexpectedError whose
		// message renders only the URL and the status code, never the body. So
		// the 500 row cannot see a token to redact; it is here because the
		// scrub must not cost the classification (and because it is the only
		// row covering isRemoteStatusError's arm). Do not drop the 401 row:
		// it is the redaction's only mutation coverage.
		{"server error", http.StatusInternalServerError, "network"},
		{"unauthorized", http.StatusUnauthorized, "auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx.status.Store(int64(tc.status))
			_, _, cleanup, err := cloneToTar(context.Background(),
				repoRef{URL: fx.url()}, token, 1<<30)
			defer cleanup()
			if err == nil {
				t.Fatal("the clone succeeded against a refusing fixture")
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("the clone error quotes the token verbatim: %v", err)
			}
			if strings.Contains(err.Error(), basic) {
				t.Errorf("the clone error quotes the token's basic-auth encoding: %v", err)
			}
			if got := cloneReason(err); got != tc.wantReason {
				t.Errorf("cloneReason = %q, want %q (the scrub must not break classification): %v",
					got, tc.wantReason, err)
			}
		})
	}
}

// TestRepoRestoredCheckpointSkipsClone is m-checkpoint 🔍: a workspace restored
// from a plan-24 checkpoint carries the tree but no marker — exactly the state
// a marker-based scheme would re-clone over — and the probe-only idempotence
// skips it, leaving the agent's restored work alone.
func TestRepoRestoredCheckpointSkipsClone(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	h, provider := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_ckpt", fx.url(), repoMount, "ghp_fixture", nil)

	// A checkpoint holding the checked-out tree and one file the agent wrote
	// inside it; no materialization marker, which capture strips (plan 24 D5).
	setMarker(t, h, "ready", checkpointBlob(t, h, map[string]string{
		"workspace/fixture/.git/HEAD":      "ref: refs/heads/main\n",
		"workspace/fixture/README.md":      "x\n",
		"workspace/fixture/agent-work.txt": "restored\n",
	}))

	h.runPass(t)

	if got := fx.clones.Load(); got != 0 {
		t.Errorf("clones = %d, want 0 — a restored checkout must not be re-cloned", got)
	}
	sb := adopt(t, provider, h)
	if got := readSandboxFile(t, sb, repoMount+"/agent-work.txt"); got != "restored\n" {
		t.Errorf("agent-work.txt = %q, want the restored work to survive", got)
	}
}

// sbPaths lists a fake sandbox's written paths, for failure messages.
func sbPaths(sb *fakeSandbox) []string {
	out := make([]string, 0, len(sb.files))
	for p := range sb.files {
		out = append(out, p)
	}
	return out
}

// TestRepoExtractionTakesNoStatusAStartupChose: an extraction whose shell the
// image's startup ended before it began lands nothing, whatever status the
// startup chose, and fails as the extraction, naming the image's startup.
// Read as the extraction's own, a 0 counted the repository as landed with no
// checkout, and a 75 refused the mount as occupied (#942).
func TestRepoExtractionTakesNoStatusAStartupChose(t *testing.T) {
	for _, status := range []int{0, occupiedStatus} {
		sb := &fakeSandbox{files: map[string]string{}, startupEnds: map[string]int{" && tar -xf ": status}}
		err := extractRepo(context.Background(), sb, "/opt/repo", "/tmp/repo.tar", "", false)
		if !errors.Is(err, errExtraction) || !errors.Is(err, sandbox.ErrImageStartup) || errors.Is(err, errMountOccupied) {
			t.Errorf("status %d: extractRepo = %v; want the extraction's failure, naming the image's startup", status, err)
		}
	}
}
