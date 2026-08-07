package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
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
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	h := newHarnessWith(t, provider, Config{Image: testImage})
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

	// A reason flip re-emits: the client learns the failure changed.
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
	const blocked = "/workspace/blocked"
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
	if len(errs) != 1 {
		t.Fatalf("recorded %d clone errors, want exactly 1 (%v)", len(errs), errs)
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
	dir := t.TempDir()
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
// Create-time validation already refuses all of these, so reaching the executor
// means the stored row was tampered with — which is exactly when the second
// check earns its place, because the operation on the other side is
// destructive and irreversible. `/workspace/repo/..` cleans to `/workspace` and
// would delete the whole workspace; `/tmp` is the platform's own sandbox
// staging root and would take every in-flight tar with it.
func TestRepoRefusesADestructiveMountPath(t *testing.T) {
	// An empty mount_path is not here: a resource missing one is malformed
	// rather than dangerous, and materializeRepos' own filter drops it with the
	// entries missing an id or a url, before any of this.
	for _, mount := range []string{
		"/workspace/repo/..", "/tmp", "/tmp/", "/", "relative/path",
		// The sandbox workdir and the skills tree materialized into it a few
		// lines earlier in the same pass. `/workspace/skills` is what a
		// repository *named* skills derives as its default mount, so this one
		// needs no tampering at all.
		"/workspace", "/workspace/skills",
		// Another repository's staging path, which this one's extraction
		// removes twice.
		"/workspace/.x.map-repo-staging",
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
	s.cmds = append(s.cmds, req.Command)
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

	_, err := h.exec.materializeRepo(ctx, sb, h.sid, repoRef{
		ID: "sesrsc_sweep", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
	})
	if err == nil {
		t.Fatal("the materialization reported success after its context was cancelled")
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
// passes create-time validation — and go-git's CheckoutOptions.Validate quietly
// substitutes `master` for a zero hash rather than failing. The clone would
// then report success while the mount holds a branch nobody asked for, and the
// session's own resource keeps advertising the sha. (GitHub's webhook payloads
// carry 40 zeros in `before`/`after` on branch create and delete, so a
// forwarding app reaches this without inventing anything.)
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
// The three re-checks that bound those phases are deliberately redundant — each
// one downstream catches what the one before it would have — so removing any
// single check leaves the outcome unchanged, and only their removal *as a bound*
// is observable. That is what this row pins: with all three gone the clone below
// runs to completion and hands back a tar.
func TestRepoCloneStopsOnASpentBudgetAfterTheFetch(t *testing.T) {
	fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
	ctx := spentAfterFetch{Context: context.Background(), fetched: &fx.clones}

	tarPath, size, cleanup, err := cloneToTar(ctx, repoRef{URL: fx.url()}, "ghp_fixture", 1<<30)
	defer cleanup()
	if err == nil {
		t.Fatalf("a clone whose budget expired after the fetch ran to completion, packing %s (%d bytes)",
			tarPath, size)
	}
	if got := cloneReason(err); got != repoOutcomeTimeout {
		t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeTimeout, err)
	}
}

// spentAfterFetch is live until the fixture has been asked for a pack and spent
// from then on — the deadline that survives the fetch and expires in the phases
// go-git leaves unbounded. Its Done channel never fires, so the transport itself
// completes normally and nothing but an explicit ctx.Err() check can notice.
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
	t.Run("malformed branch name", func(t *testing.T) {
		fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
		_, _, cleanup, err := cloneToTar(context.Background(),
			repoRef{URL: fx.url(), Checkout: &repoCheckout{Type: "branch", Name: "foo:bar"}},
			"ghp_fixture", 1<<30)
		defer cleanup()
		if err == nil {
			t.Fatal("a branch name with a refspec separator cloned successfully")
		}
		if got := cloneReason(err); got != repoOutcomeCheckout {
			t.Errorf("cloneReason = %q, want %q — the ref is the caller's, not ours: %v",
				got, repoOutcomeCheckout, err)
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
