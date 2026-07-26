package sandbox_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The two shared bulk shells are what both backends' batches actually run, so
// their contract is pinned here against a real /bin/bash rather than left to the
// two live contract suites — the same reasoning the path-fault shell's own test
// gives: a host runs them in milliseconds and stages the awkward cases (a member
// the delivery lost, a target that turned into a directory) that a container makes
// expensive to reach. That the sandbox image carries a userland able to run them
// is the live contract suites' job, and they run these same strings.

// What is deliberately NOT here: the mode-preservation step (#204). It reaches for
// `stat -c %a`, which is GNU/BusyBox and not BSD, so on a macOS host it degrades to
// the no-op it is documented to degrade to and an assertion would pass on CI and
// fail on a developer's machine. The live contract suites pin it on both backends,
// in images that carry the right `stat` — BulkWriteKeepsTheTargetsMode.

// bulkShell runs one of the shared bulk shells over a manifest and directory list
// staged on the host, and returns its exit code and stderr. The paths travel as
// arguments, the form the k8s backend uses and the one that leaves the shell
// nothing to expand.
func bulkShell(t *testing.T, shell, fn, manifest, dirList string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/bash", "-c",
		shell+"\n"+fn+" \"$1\" \"$2\"", "map-bulk-test", manifest, dirList)
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run %s: %v", fn, err)
	}
	return ee.ExitCode(), stderr.String()
}

// stageBatch writes a manifest and directory list naming each member, and creates
// the temporary files listed unless the member is in absent.
func stageBatch(t *testing.T, dir string, members map[string]string, absent map[string]bool) (string, string) {
	t.Helper()
	var manifest, dirs bytes.Buffer
	seen := map[string]bool{}
	// Sorted, so a member's manifest index is the same on every run and a test can
	// name the one it expects the shell to blame.
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		tmp := dir + "/" + sandbox.TempPrefix + "batch-" + strconv.Itoa(i)
		target := dir + "/" + name
		if !absent[name] {
			if err := os.WriteFile(tmp, []byte(members[name]), 0o644); err != nil {
				t.Fatalf("stage %s: %v", tmp, err)
			}
		}
		manifest.WriteString(tmp + "\x00" + target + "\x00")
		if !seen[dir] {
			seen[dir] = true
			dirs.WriteString(dir + "\x00")
		}
	}
	mPath, dPath := dir+"/"+sandbox.TempPrefix+"manifest", dir+"/"+sandbox.TempPrefix+"dirs"
	if err := os.WriteFile(mPath, manifest.Bytes(), 0o600); err != nil {
		t.Fatalf("stage the manifest: %v", err)
	}
	if err := os.WriteFile(dPath, dirs.Bytes(), 0o600); err != nil {
		t.Fatalf("stage the directory list: %v", err)
	}
	return mPath, dPath
}

// The happy path: every member is renamed into place, and the manifest and
// directory list go with it — a successful batch leaves the sandbox holding the
// files it was asked for and nothing else.
func TestBulkRenameShell(t *testing.T) {
	dir := t.TempDir()
	members := map[string]string{"a.txt": "AAA", "b.txt": "BBB"}
	manifest, dirList := stageBatch(t, dir, members, nil)

	code, stderr := bulkShell(t, sandbox.BulkRenameShell, "__map_bulk_rename", manifest, dirList)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr)
	}
	for name, want := range members {
		got, err := os.ReadFile(dir + "/" + name)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	assertNoTemps(t, dir)
}

// A temporary file the manifest names but that is not there is the delivery
// having lost it — the k8s backend's short-stream guard restated for a batch, and
// the branch no containerized row can reach, because neither backend can be made
// to deliver a manifest without its members. It is the one failure that lands
// NOTHING: the members are checked in a pass of their own before any of them is
// moved, so a batch that arrived short is refused whole rather than leaving a tree
// with a hole in it. (Every other failure is D2's ordinary non-transactional one —
// what landed before it stays.)
func TestBulkRenameShellRefusesAnUndeliveredMember(t *testing.T) {
	dir := t.TempDir()
	members := map[string]string{"a.txt": "AAA", "b.txt": "BBB", "c.txt": "CCC"}
	manifest, dirList := stageBatch(t, dir, members, map[string]bool{"b.txt": true})

	code, stderr := bulkShell(t, sandbox.BulkRenameShell, "__map_bulk_rename", manifest, dirList)
	if code != sandbox.ExitBulkIncomplete {
		t.Errorf("exit %d, want ExitBulkIncomplete (%d); stderr: %s", code, sandbox.ExitBulkIncomplete, stderr)
	}
	if !strings.Contains(stderr, "map-bulk-fail 1") {
		t.Errorf("stderr = %q, want it to name member 1 — the one the delivery lost", stderr)
	}
	// Nothing landed — not the member before the gap, nor the one after it.
	for _, name := range []string{"a.txt", "c.txt"} {
		if _, err := os.Stat(dir + "/" + name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v, want a short delivery to land nothing at all", name, err)
		}
	}
	assertNoTemps(t, dir)
}

// A manifest that is not there, or that is empty, is not an empty batch — it is a
// delivery that lost everything, or something in the sandbox having deleted the
// manifest in the window between the delivery and this pass (the docker backend
// runs them as two round trips, so that window is real). Either way the run must
// fail: reporting success for a batch that wrote nothing, and leaving its members
// behind as residue, is the one outcome the caller cannot detect.
func TestBulkRenameShellRefusesAManifestThatIsNotThere(t *testing.T) {
	dir := t.TempDir()
	manifest, dirList := stageBatch(t, dir, map[string]string{"a.txt": "AAA"}, nil)
	if err := os.Remove(manifest); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	if code, _ := bulkShell(t, sandbox.BulkRenameShell, "__map_bulk_rename", manifest, dirList); code != sandbox.ExitBulkIncomplete {
		t.Errorf("a missing manifest exited %d, want ExitBulkIncomplete (%d)", code, sandbox.ExitBulkIncomplete)
	}
	if err := os.WriteFile(manifest, nil, 0o600); err != nil {
		t.Fatalf("stage an empty manifest: %v", err)
	}
	if code, _ := bulkShell(t, sandbox.BulkRenameShell, "__map_bulk_rename", manifest, dirList); code != sandbox.ExitBulkIncomplete {
		t.Errorf("an empty manifest exited %d, want ExitBulkIncomplete (%d)", code, sandbox.ExitBulkIncomplete)
	}
	// The members are NOT shed here, and cannot be: a manifest that is gone takes
	// with it the only list of what the batch put in the sandbox. Failing is the
	// part that matters — the residue is bounded by a sandbox that is thrown away.
	if _, err := os.Stat(manifest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the manifest survived: %v, want it removed", err)
	}
	if _, err := os.Stat(dirList); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory list survived: %v, want it removed", err)
	}
}

// A target that is a directory is refused, the directory is left whole, and the
// temporary file is shed — the same answer a single write gives, per member.
func TestBulkRenameShellRefusesADirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/b.txt", 0o755); err != nil {
		t.Fatalf("stage a directory in the way: %v", err)
	}
	if err := os.WriteFile(dir+"/b.txt/inside", []byte("kept"), 0o644); err != nil {
		t.Fatalf("stage a file inside it: %v", err)
	}
	manifest, dirList := stageBatch(t, dir,
		map[string]string{"a.txt": "AAA", "b.txt": "clobber", "c.txt": "CCC"}, nil)

	code, stderr := bulkShell(t, sandbox.BulkRenameShell, "__map_bulk_rename", manifest, dirList)
	if code != sandbox.ExitPathIsDirectory {
		t.Errorf("exit %d, want ExitPathIsDirectory (%d); stderr: %s", code, sandbox.ExitPathIsDirectory, stderr)
	}
	if !strings.Contains(stderr, "map-bulk-fail 1") {
		t.Errorf("stderr = %q, want it to name member 1", stderr)
	}
	if got, err := os.ReadFile(dir + "/b.txt/inside"); err != nil || string(got) != "kept" {
		t.Errorf("the directory's contents = %q, %v; want it whole", got, err)
	}
	assertNoTemps(t, dir)
}

// The recovery pass sheds what a refused delivery landed, makes the directories
// the batch needs, and takes its own bookkeeping with it — so a batch that ends
// there leaves the sandbox holding nothing of itself.
func TestBulkRecoverShell(t *testing.T) {
	dir := t.TempDir()
	manifest, dirList := stageBatch(t, dir, map[string]string{"a.txt": "AAA"}, nil)
	// A directory the delivery would have needed and that does not exist yet.
	if err := os.WriteFile(dirList, []byte(dir+"\x00"+dir+"/deep/nested\x00"), 0o600); err != nil {
		t.Fatalf("stage the directory list: %v", err)
	}

	code, stderr := bulkShell(t, sandbox.BulkRecoverShell, "__map_bulk_recover", manifest, dirList)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr)
	}
	if fi, err := os.Stat(dir + "/deep/nested"); err != nil || !fi.IsDir() {
		t.Errorf("the recovery did not make the directory the batch needs: %v", err)
	}
	assertNoTemps(t, dir)
	// A manifest that is not there is a delivery that landed nothing: there is
	// nothing to recover and the caller's own error stands.
	if code, _ := bulkShell(t, sandbox.BulkRecoverShell, "__map_bulk_recover",
		dir+"/no-such-manifest", dirList); code != 0 {
		t.Errorf("recovery with no manifest exited %d, want 0", code)
	}
}

// A directory the batch needs that is blocked by a non-directory is the path's
// fault, not the sandbox's, and the recovery names it as such — the sentinel a
// tool hands the model rather than one the executor retries on.
func TestBulkRecoverShellClassifiesABlockedPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/blocker", []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("stage a regular file: %v", err)
	}
	manifest, dirList := stageBatch(t, dir, map[string]string{"a.txt": "AAA"}, nil)
	if err := os.WriteFile(dirList, []byte(dir+"/blocker/deeper\x00"), 0o600); err != nil {
		t.Fatalf("stage the directory list: %v", err)
	}

	code, _ := bulkShell(t, sandbox.BulkRecoverShell, "__map_bulk_recover", manifest, dirList)
	if code != sandbox.ExitPathNotDirectory {
		t.Errorf("exit %d, want ExitPathNotDirectory (%d)", code, sandbox.ExitPathNotDirectory)
	}
	if got, err := os.ReadFile(dir + "/blocker"); err != nil || string(got) != "i am a file" {
		t.Errorf("the file in the way = %q, %v; want it untouched", got, err)
	}
	// It sheds the batch's temporary files before it classifies, so the terminal
	// path leaves nothing behind either.
	assertNoTemps(t, dir)
}

// assertNoTemps fails unless dir holds no file named with the write path's
// temporary prefix — the members' own, and the two bookkeeping files.
func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), sandbox.TempPrefix) {
			t.Errorf("%s left behind in %s", e.Name(), dir)
		}
	}
}
