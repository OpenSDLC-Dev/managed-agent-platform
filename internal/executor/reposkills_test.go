package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// repoSkillsRecord reads the session's recorded repository skills, by
// resource; ok is false while nothing is recorded (the column is NULL).
func (h *harness) repoSkillsRecord(t *testing.T) (recorded map[string][]skills.RepoSkill, ok bool) {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT repo_skills FROM sessions WHERE id = $1`, h.sid.String()).Scan(&raw); err != nil {
		t.Fatalf("read repo_skills: %v", err)
	}
	if raw == nil {
		return nil, false
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("decode repo_skills %s: %v", raw, err)
	}
	return recorded, true
}

// TestRepoSkillsScannedBeforeTheFirstTurn is plan 65 decision 2 and 3 on a real
// checkout: a tool_exec with no tool to run, the shape the brain enqueues when
// it holds a session's first turn, clones the repository, records the skills
// its .claude/skills holds under the resource's id, and wakes the turn. The
// fixture is recording #23's: a skill with no frontmatter and one with no
// description are listed with an empty description, while a lowercase
// skill.md, a SKILL.md two levels down and a hidden directory are not found.
// Of the two links, a linked skill directory is followed and a SKILL.md that
// is itself a link, which a read refuses, is not listed.
func TestRepoSkillsScannedBeforeTheFirstTurn(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		"README.md":                          "fixture\n",
		".claude/skills/alpha/SKILL.md":      "---\nname: alpha\ndescription: Alpha marker.\n---\nbody\n",
		".claude/skills/broken/SKILL.md":     "# no frontmatter\n",
		".claude/skills/nodesc/SKILL.md":     "---\nname: nodesc\n---\n",
		".claude/skills/lower/skill.md":      "---\ndescription: lowercase\n---\n",
		".claude/skills/deep/inner/SKILL.md": "---\ndescription: two levels\n---\n",
		".claude/skills/.hidden/SKILL.md":    "---\ndescription: hidden\n---\n",
	})
	fx.links(t, map[string]string{
		".claude/skills/viadir":          "alpha",
		".claude/skills/linked/SKILL.md": "../alpha/SKILL.md",
	})
	counter := &countListProvider{}
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider { counter.Provider = p; return counter })
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)

	h.suspend(t)
	h.stepOnce(t)

	got, ok := h.repoSkillsRecord(t)
	if !ok {
		t.Fatal("repo_skills is NULL after the run")
	}
	root := repoMount + "/.claude/skills/"
	want := map[string][]skills.RepoSkill{"sesrsc_skills": {
		{Name: "alpha", Description: "Alpha marker.", Location: root + "alpha/SKILL.md"},
		{Name: "broken", Description: "", Location: root + "broken/SKILL.md"},
		{Name: "nodesc", Description: "", Location: root + "nodesc/SKILL.md"},
		{Name: "viadir", Description: "Alpha marker.", Location: root + "viadir/SKILL.md"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("repo_skills = %+v\nwant %+v", got, want)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model turns after the run = %d, want 1: the held turn must wake", n)
	}

	// Once per repository: a recorded scan is not taken again, even when it
	// found nothing and the checkout now holds skills.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET repo_skills = '{"sesrsc_skills":[]}' WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.suspend(t)
	h.stepOnce(t)
	if got, _ := h.repoSkillsRecord(t); len(got["sesrsc_skills"]) != 0 {
		t.Errorf("a second run overwrote the record: %+v", got)
	}
	if n := counter.listings.Load(); n != 1 {
		t.Errorf("listings = %d over two runs, want 1: a recorded scan is not taken again", n)
	}
}

// TestRepoSkillsWithoutARepository: a session with no repository records
// nothing, and its run wakes the turn as any run does.
func TestRepoSkillsWithoutARepository(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t)
	h.stepOnce(t)
	if got, ok := h.repoSkillsRecord(t); ok {
		t.Errorf("repo_skills = %+v, want nothing recorded", got)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model turns = %d, want 1", n)
	}
}

// TestRepoSkillsListingFailureRetried: a checkout whose listing fails costs the
// session nothing else: the healthy checkout's skills are recorded and the
// turn wakes. The failed one has no entry, so the next run lists it again and
// adds it, leaving the healthy one's entry as it was.
func TestRepoSkillsListingFailureRetried(t *testing.T) {
	broken := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	healthy := newGitFixture(t, map[string]string{
		".claude/skills/beta/SKILL.md": "---\ndescription: Beta marker.\n---\n",
	})
	fail := &failListProvider{}
	fail.fail.Store(true)
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider { fail.Provider = p; return fail })
	h.seedRepoResource(t, "sesrsc_broken", broken.url(), repoMount, "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_healthy", healthy.url(), "/workspace/healthy", "ghp_fixture", nil)
	beta := []skills.RepoSkill{{Name: "beta", Description: "Beta marker.", Location: "/workspace/healthy/.claude/skills/beta/SKILL.md"}}

	h.suspend(t)
	h.stepOnce(t)
	got, ok := h.repoSkillsRecord(t)
	if want := map[string][]skills.RepoSkill{"sesrsc_healthy": beta}; !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("repo_skills = %+v (set %v), want %+v", got, ok, want)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model turns = %d, want 1", n)
	}

	fail.fail.Store(false)
	h.suspend(t)
	h.stepOnce(t)
	got, _ = h.repoSkillsRecord(t)
	want := map[string][]skills.RepoSkill{
		"sesrsc_healthy": beta,
		"sesrsc_broken":  {{Name: "alpha", Description: "Alpha marker.", Location: repoMount + "/.claude/skills/alpha/SKILL.md"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after the retry repo_skills = %+v\nwant %+v", got, want)
	}
}

// TestRepoSkillsScannedWhenTheCloneLands: a repository whose clone failed has
// no checkout to scan, so it gets no entry, and its skills are scanned at the
// run whose clone lands rather than recorded as none.
func TestRepoSkillsScannedWhenTheCloneLands(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)

	fx.status.Store(http.StatusServiceUnavailable)
	h.suspend(t)
	h.stepOnce(t)
	if got, ok := h.repoSkillsRecord(t); ok {
		t.Errorf("repo_skills = %+v with the clone failed, want nothing recorded", got)
	}

	fx.status.Store(0)
	h.suspend(t)
	h.stepOnce(t)
	got, _ := h.repoSkillsRecord(t)
	want := map[string][]skills.RepoSkill{"sesrsc_skills": {
		{Name: "alpha", Description: "Alpha marker.", Location: repoMount + "/.claude/skills/alpha/SKILL.md"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after the clone landed repo_skills = %+v\nwant %+v", got, want)
	}
}

// TestRepoSkillsRecordedOnAFaultedRun: a run whose tool hit a backend fault
// settles down the fault path, which commits the results that did run and
// leaves the item live; the scan it took before the tools commits there too,
// so the turn the brain held finds the list when it next runs.
func TestRepoSkillsRecordedOnAFaultedRun(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	h, _ := wrappedRepoHarness(t, func(p sandbox.Provider) sandbox.Provider {
		return &faultMarkerProvider{Provider: p, marker: "# checkpoint-shell template"}
	})
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)

	use, _ := json.Marshal(map[string]any{"name": "bash", "input": map[string]string{"command": "true"}})
	h.suspend(t, string(use))
	h.stepOnce(t)

	got, _ := h.repoSkillsRecord(t)
	want := map[string][]skills.RepoSkill{"sesrsc_skills": {
		{Name: "alpha", Description: "Alpha marker.", Location: repoMount + "/.claude/skills/alpha/SKILL.md"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after a faulted run repo_skills = %+v\nwant %+v", got, want)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec items = %d, want 1: a faulted run leaves its item for the retry", n)
	}
}

// faultMarkerProvider fails, as a lost exec stream would, every exec whose
// command carries marker, and nothing else: the bash tool's own exec, by its
// checkpoint shell's header (internal/sandbox/shell/template.sh).
type faultMarkerProvider struct {
	sandbox.Provider
	marker string
}

func (p *faultMarkerProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &faultMarkerSandbox{Sandbox: sb, marker: p.marker}, nil
}

type faultMarkerSandbox struct {
	sandbox.Sandbox
	marker string
}

func (s *faultMarkerSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if strings.Contains(req.Command, s.marker) {
		return sandbox.ExecResult{}, errors.New("exec stream lost")
	}
	return s.Sandbox.Exec(ctx, req)
}

// TestRepoSkillsCapCountsRecorded: the session's cap counts what earlier runs
// recorded, so a repository scanned once the cap is spent gets an empty entry.
func TestRepoSkillsCapCountsRecorded(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)
	full := make([]skills.RepoSkill, maxRepoSkills)
	for i := range full {
		full[i] = skills.RepoSkill{Name: fmt.Sprintf("s%04d", i), Location: fmt.Sprintf("/workspace/o/.claude/skills/s%04d/SKILL.md", i)}
	}
	raw, err := json.Marshal(map[string][]skills.RepoSkill{"sesrsc_other": full})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET repo_skills = $2 WHERE id = $1`, h.sid.String(), raw); err != nil {
		t.Fatal(err)
	}

	h.suspend(t)
	h.stepOnce(t)
	got, _ := h.repoSkillsRecord(t)
	if list, ok := got["sesrsc_skills"]; !ok || len(list) != 0 {
		t.Errorf("entry over a spent cap = %+v (set %v), want an empty one", list, ok)
	}
}

// TestRepoSkillsRecordFailureWakesNothing: the record commits with the run's
// settlement, so a run that cannot store it commits nothing and wakes nothing:
// the held turn stays held for the retry rather than waking to find no record.
func TestRepoSkillsRecordFailureWakesNothing(t *testing.T) {
	fx := newGitFixture(t, map[string]string{
		".claude/skills/alpha/SKILL.md": "---\ndescription: Alpha marker.\n---\n",
	})
	h, _ := dockerRepoHarness(t)
	h.seedRepoResource(t, "sesrsc_skills", fx.url(), repoMount, "ghp_fixture", nil)
	if _, err := h.pool.Exec(context.Background(),
		`ALTER TABLE sessions ADD CONSTRAINT test_no_repo_skills CHECK (repo_skills IS NULL)`); err != nil {
		t.Fatal(err)
	}
	h.suspend(t)
	h.stepOnce(t)
	if _, ok := h.repoSkillsRecord(t); ok {
		t.Fatal("the record landed despite the constraint")
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model turns = %d, want 0: a run that stored no scan must not wake the turn", n)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec items = %d, want 1 left for the retry", n)
	}
}

// TestRecordRepoSkillsFirstWins pins the write's merge: a repository's first
// entry stands, and an entry for another repository is added beside it.
func TestRecordRepoSkillsFirstWins(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	write := func(found map[string][]skills.RepoSkill) {
		t.Helper()
		tx, err := h.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := recordRepoSkills(ctx, tx, h.sid, found); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	first := []skills.RepoSkill{{Name: "first", Location: "/workspace/r/.claude/skills/first/SKILL.md"}}
	other := []skills.RepoSkill{{Name: "other", Location: "/workspace/o/.claude/skills/other/SKILL.md"}}
	write(map[string][]skills.RepoSkill{"sesrsc_r": first})
	write(map[string][]skills.RepoSkill{"sesrsc_r": {{Name: "second"}}, "sesrsc_o": other})
	write(nil)
	got, _ := h.repoSkillsRecord(t)
	if want := map[string][]skills.RepoSkill{"sesrsc_r": first, "sesrsc_o": other}; !reflect.DeepEqual(got, want) {
		t.Errorf("repo_skills = %+v\nwant %+v", got, want)
	}
}

// countListProvider counts the repository-skills listings its sandboxes run.
type countListProvider struct {
	sandbox.Provider
	listings atomic.Int32
}

func (p *countListProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &countListSandbox{Sandbox: sb, p: p}, nil
}

type countListSandbox struct {
	sandbox.Sandbox
	p *countListProvider
}

func (s *countListSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if strings.Contains(req.Command, "/.claude/skills") {
		s.p.listings.Add(1)
	}
	return s.Sandbox.Exec(ctx, req)
}

// failListProvider fails the repository-skills listing of the checkout at
// repoMount while fail is set, and nothing else.
type failListProvider struct {
	sandbox.Provider
	fail atomic.Bool
}

func (p *failListProvider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := p.Provider.Provision(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &failListSandbox{Sandbox: sb, p: p}, nil
}

type failListSandbox struct {
	sandbox.Sandbox
	p *failListProvider
}

func (s *failListSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if s.p.fail.Load() && strings.Contains(req.Command, repoMount+"/.claude/skills") {
		return sandbox.ExecResult{}, errors.New("exec stream lost")
	}
	return s.Sandbox.Exec(ctx, req)
}

// TestScanRepoSkillsListsOnlyAnnounceableNames pins the scan's own rules on a
// stub listing a real checkout cannot produce: an image's startup banner is no
// name, since the listing is framed; names come back sorted; one holding a
// control character, a newline among them, or one that is not UTF-8 is passed
// over, since it would break the list or name no path; the entry the output
// cap cut short is dropped; and the cap's room is shared across repositories.
func TestScanRepoSkillsListsOnlyAnnounceableNames(t *testing.T) {
	refs := []repoRef{{ID: "sesrsc_a", Type: "github_repository", URL: "https://github.com/o/r", MountPath: "/workspace/r"}}
	sb := &listingSandbox{stdout: "zeta\x00alpha\x00new\nline\x00bad\xff\x00cut-sho", truncated: true}
	got := (&Executor{}).scanRepoSkills(context.Background(), sb, "sesn_x", refs, maxRepoSkills, func() {})
	want := map[string][]skills.RepoSkill{"sesrsc_a": {
		{Name: "alpha", Description: "From alpha.", Location: "/workspace/r/.claude/skills/alpha/SKILL.md"},
		{Name: "zeta", Description: "From zeta.", Location: "/workspace/r/.claude/skills/zeta/SKILL.md"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %+v\nwant %+v", got, want)
	}

	var many strings.Builder
	for i := 0; i < maxRepoSkills+5; i++ {
		fmt.Fprintf(&many, "s%04d\x00", i)
	}
	if got := (&Executor{}).scanRepoSkills(context.Background(), &listingSandbox{stdout: many.String()}, "sesn_x", refs, maxRepoSkills, func() {}); len(got["sesrsc_a"]) != maxRepoSkills {
		t.Errorf("scan kept %d skills, want the cap of %d", len(got["sesrsc_a"]), maxRepoSkills)
	}

	two := append(refs, repoRef{ID: "sesrsc_b", Type: "github_repository", URL: "https://github.com/o/s", MountPath: "/workspace/s"})
	got = (&Executor{}).scanRepoSkills(context.Background(), &listingSandbox{stdout: "a\x00b\x00"}, "sesn_x", two, 3, func() {})
	if n, m := len(got["sesrsc_a"]), len(got["sesrsc_b"]); n != 2 || m != 1 {
		t.Errorf("with room for 3 the repositories kept %d and %d, want 2 and 1", n, m)
	}
}

// TestScanRepoSkillsReadFailures: a SKILL.md the sandbox refuses for good, too
// large, not a regular file, a directory or gone since the listing, is listed
// with an empty description, while a read that failed for a reason the next
// run may not meet leaves the whole repository unscanned, for a later run.
func TestScanRepoSkillsReadFailures(t *testing.T) {
	refs := []repoRef{
		{ID: "sesrsc_a", Type: "github_repository", URL: "https://github.com/o/r", MountPath: "/workspace/r"},
		{ID: "sesrsc_b", Type: "github_repository", URL: "https://github.com/o/s", MountPath: "/workspace/s"},
	}
	sb := &listingSandbox{stdout: "big\x00dir\x00gone\x00ok\x00special\x00", readErr: map[string]error{
		"/workspace/r/.claude/skills/big/SKILL.md":     fmt.Errorf("big: %w", sandbox.ErrFileTooLarge),
		"/workspace/r/.claude/skills/dir/SKILL.md":     fmt.Errorf("dir: %w", sandbox.ErrIsDirectory),
		"/workspace/r/.claude/skills/gone/SKILL.md":    fmt.Errorf("gone: %w", sandbox.ErrFileNotExist),
		"/workspace/r/.claude/skills/special/SKILL.md": fmt.Errorf("special: %w", sandbox.ErrNotRegularFile),
		"/workspace/s/.claude/skills/ok/SKILL.md":      errors.New("exec stream lost"),
	}}
	got := (&Executor{}).scanRepoSkills(context.Background(), sb, "sesn_x", refs, maxRepoSkills, func() {})
	want := map[string][]skills.RepoSkill{"sesrsc_a": {
		{Name: "big", Description: "", Location: "/workspace/r/.claude/skills/big/SKILL.md"},
		{Name: "dir", Description: "", Location: "/workspace/r/.claude/skills/dir/SKILL.md"},
		{Name: "gone", Description: "", Location: "/workspace/r/.claude/skills/gone/SKILL.md"},
		{Name: "ok", Description: "From ok.", Location: "/workspace/r/.claude/skills/ok/SKILL.md"},
		{Name: "special", Description: "", Location: "/workspace/r/.claude/skills/special/SKILL.md"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %+v\nwant %+v", got, want)
	}
}

// TestListRepoSkillsScriptUnderImageShellOptions runs the listing as a sandbox
// exec would, framed and under bash, with a BASH_ENV file that sets what an
// image's startup may leave on: noglob would list nothing, GLOBIGNORE would
// hide "a", dotglob would list a hidden directory, failglob would fail an
// empty skills directory, and a locale's collation would put "a" before "B".
// The listing undoes each, so it names the same skills, in byte order,
// whatever the image set.
func TestListRepoSkillsScriptUnderImageShellOptions(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the listing runs under bash in a sandbox: %v", err)
	}
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte(sandboxtest.BannerHook+
		"set -f\nGLOBIGNORE=a\nshopt -s dotglob failglob nocaseglob\nexport LC_ALL=en_US.UTF-8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "skills")
	for _, d := range []string{".hidden", "a", "B"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "SKILL.md"), []byte("---\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	list := func(root string) sandbox.ExecResult {
		t.Helper()
		f := sandbox.NewFrame("reposkills")
		cmd := exec.Command(bash, "-c", f.Wrap(listRepoSkillsScript(root)))
		cmd.Env = append(os.Environ(), "BASH_ENV="+hook)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run the listing: %v", err)
			}
			code = exitErr.ExitCode()
		}
		res, framed := f.Unframe(sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code})
		if !framed {
			t.Fatalf("the listing was not framed: %q", stdout.String())
		}
		return res
	}
	if res := list(root); res.ExitCode != 0 || res.Stdout != "B\x00a\x00" {
		t.Errorf("listing = %q (exit %d), want %q", res.Stdout, res.ExitCode, "B\x00a\x00")
	}
	if res := list(empty); res.ExitCode != 0 || res.Stdout != "" {
		t.Errorf("empty listing = %q (exit %d), want nothing and exit 0", res.Stdout, res.ExitCode)
	}
}

// listingSandbox answers the skills listing with a fixed output, behind an
// image's startup banner that ends no line, and each SKILL.md read with a
// description naming its directory, or with readErr's error for its path. A
// framed listing gets its answer framed, so the banner falls outside it; an
// unframed one gets the banner glued to the first name, as a real image would
// print it.
type listingSandbox struct {
	sandbox.Sandbox
	stdout    string
	truncated bool
	readErr   map[string]error
}

func (s *listingSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	const banner = "welcome to the image "
	res := sandbox.ExecResult{Stdout: s.stdout, StdoutTruncated: s.truncated}
	if _, f, ok := sandboxtest.Unwrap(req.Command); ok {
		res = sandboxtest.Framed(f, res)
	}
	res.Stdout = banner + res.Stdout
	return res, nil
}

func (s *listingSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	if err := s.readErr[p]; err != nil {
		return nil, err
	}
	dir := path.Base(path.Dir(p))
	return []byte("---\ndescription: From " + dir + ".\n---\n"), nil
}
