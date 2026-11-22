package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/tools/harnessdoctor/gitworktree"
)

// The hook runs in the invoking worktree, even when a parent process exports
// environment for its own checkout or Git index.
func gitEnv() []string {
	return gitworktree.Environment()
}

func worktree(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cannot resolve Git worktree for %s: %w", cwd, err)
	}
	root := filepath.Clean(strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r"))
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	// Preserve a Windows short name or a link to the repository itself if
	// the caller has an ancestor naming the same directory Git identified.
	ancestor, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	for {
		info, err := os.Stat(ancestor)
		if err == nil && os.SameFile(rootInfo, info) {
			return ancestor, nil
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	return root, nil
}

// Walk each component so a directory link never redirects a source read or an
// adapter write. Missing components are allowed for generated files.
func safePath(root, name string) (string, error) {
	if !fs.ValidPath(name) || name == "." || !filepath.IsLocal(filepath.FromSlash(name)) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("invalid repository path %q", name)
	}
	path := root
	for _, part := range strings.Split(name, "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s: linked or special harness path", path)
		}
	}
	return path, nil
}

func regularFile(root, name string) ([]byte, error) {
	path, err := safePath(root, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Prefer the index so a maintainer's staged settings survive a working-file
// deletion. After a staged deletion the current worktree's HEAD is the source.
func recordedSettings(root, name string) ([]byte, error) {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = gitEnv()
		return cmd.Output()
	}
	listed, err := git("ls-files", "--", name)
	if err != nil {
		return nil, err
	}
	if len(listed) != 0 {
		return git("show", ":"+name)
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil // A newly initialized fixture or repository has no HEAD.
		}
		return nil, err
	}
	listed, err = git("ls-tree", "-z", "--name-only", "HEAD", "--", name)
	if err != nil || len(listed) == 0 {
		return nil, err
	}
	return git("show", "HEAD:"+name)
}

func walkFiles(root, dir string) ([]string, error) {
	start, err := safePath(root, dir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(start); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s must be a directory", dir)
	}
	var files []string
	err = filepath.WalkDir(start, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%s: linked or special harness file", p)
		}
		name, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(name))
		return nil
	})
	return files, err
}

var link = regexp.MustCompile(`\]\((?:<([^>]+)>|([^\s)]+))\)`)

func checkLinks(root, doc, body string) error {
	for _, m := range link.FindAllStringSubmatch(body, -1) {
		loc := m[1] + m[2]
		loc, _, _ = strings.Cut(loc, "#")
		if loc == "" || strings.Contains(loc, ":") || strings.HasPrefix(loc, "/") {
			continue
		}
		rel := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(doc)), filepath.FromSlash(loc)))
		if !filepath.IsLocal(rel) {
			continue
		} // Sibling reference checkouts are optional.
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			return fmt.Errorf("%s references %s: %w", doc, loc, err)
		}
	}
	return nil
}

func checkGuide(root string) error {
	const name = "AGENTS.md"
	body, err := cleanText(root, name)
	if err != nil {
		return err
	}
	if lines := strings.Count(strings.TrimSuffix(body, "\n"), "\n") + 1; lines > 200 {
		return fmt.Errorf("%s has %d physical lines; maximum 200", name, lines)
	}
	if !strings.Contains(body, "harnessdoctor check") || !strings.Contains(body, "sync") {
		return errors.New("AGENTS.md must explain harnessdoctor check and sync")
	}
	for _, file := range []string{"CLAUDE.md", "GEMINI.md", "AGENTS.override.md", ".claude/CLAUDE.md", ".claude/AGENTS.md", ".agents/AGENTS.md", ".agents/GEMINI.md", ".agent/AGENTS.md", ".agent/GEMINI.md", "_agents/AGENTS.md", "_agents/GEMINI.md", "_agent/AGENTS.md", "_agent/GEMINI.md"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); err == nil {
			return fmt.Errorf("competing project instructions in %s; maintain AGENTS.md", file)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return checkLinks(root, name, body)
}

type treeMode struct{ executable map[string]bool }

func readModes(root string) (treeMode, error) {
	t := treeMode{}
	if runtime.GOOS != "windows" {
		return t, nil
	}
	t.executable = map[string]bool{}
	cmd := exec.Command("git", "-C", root, "ls-files", "--stage", "-z", "--", "harness/hooks", "harness/skills", ".claude/skills", ".agents/skills")
	cmd.Env = gitEnv()
	b, err := cmd.Output()
	if err != nil {
		return t, fmt.Errorf("read executable bits from Git index: %w", err)
	}
	for _, row := range strings.Split(string(b), "\x00") {
		head, path, ok := strings.Cut(row, "\t")
		if ok {
			t.executable[path] = strings.HasPrefix(head, "100755 ")
		}
	}
	return t, nil
}

func (t treeMode) executableAt(root, name string) (bool, error) {
	if t.executable != nil {
		return t.executable[name], nil
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0100 != 0, nil
}

type change struct {
	name   string
	data   []byte
	before []byte
	mode   fs.FileMode
}

func managedTrees(host string) []string {
	trees := []string{}
	if host == "" || host == "claude" {
		trees = append(trees, ".claude/skills", ".claude/rules", ".claude/agents")
	}
	if host != "claude" {
		trees = append(trees, ".agents/skills")
	}
	if host == "" || host == "antigravity" {
		trees = append(trees, ".agents/rules", ".agents/agents")
	}
	if host == "" || host == "codex" {
		trees = append(trees, ".codex/agents")
	}
	return trees
}

func compare(root, host string, m manifest, files map[string]output) (changes []change, conflicts []string, windowsModeFixes []string) {
	modes, err := readModes(root)
	if err != nil {
		return nil, []string{err.Error()}, nil
	}
	scripts := []string{"harness/hooks/verify-commit-gate.sh", "harness/hooks/verify-stamp.sh"}
	if (host == "" || host == "claude") && slices.ContainsFunc(m.Agents, func(a agent) bool { return a.ClaudeBashGuard }) {
		scripts = append(scripts, "harness/hooks/issue-triage-bash-guard.sh", "harness/hooks/issue-triage-read.sh")
	}
	for _, script := range scripts {
		executable, err := modes.executableAt(root, script)
		if err != nil {
			conflicts = append(conflicts, err.Error())
		} else if !executable {
			fix := "chmod +x " + script
			if runtime.GOOS == "windows" {
				fix = "git update-index --chmod=+x -- " + script
			}
			conflicts = append(conflicts, script+": missing executable permission; run "+fix)
		}
	}
	for _, dir := range managedTrees(host) {
		actual, err := walkFiles(root, dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			conflicts = append(conflicts, err.Error())
			continue
		}
		for _, name := range actual {
			if _, ok := files[name]; !ok {
				conflicts = append(conflicts, name+": unregistered adapter; register the source or move the file explicitly")
			}
		}
	}
	// Merge every registry against its actual worktree bytes, even if it has
	// to recover the merge base for a missing settings file from the Git index.
	preimages := map[string][]byte{}
	for _, client := range hosts {
		if host != "" && host != client {
			continue
		}
		source := "harness/hooks/" + client + ".json"
		src, err := regularFile(root, source)
		if err != nil {
			conflicts = append(conflicts, err.Error())
			continue
		}
		path := registryPaths[client]
		old, err := regularFile(root, path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			conflicts = append(conflicts, err.Error())
			continue
		}
		preimages[path] = old
		// Deleting the whole Claude settings file also removes its independent
		// permission and plugin settings. Restore those from this worktree's
		// recorded settings when the shared file is tracked.
		if old == nil && client == "claude" {
			old, err = recordedSettings(root, path)
			if err != nil {
				conflicts = append(conflicts, fmt.Sprintf("%s: cannot recover staged project settings: %v", path, err))
				continue
			}
		}
		merged, err := mergeRegistry(client, src, old)
		if err != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s (%s): %v", path, source, err))
			continue
		}
		files[path] = output{data: merged}
	}
	for name, want := range files {
		path, err := safePath(root, name)
		if err != nil {
			conflicts = append(conflicts, err.Error())
			continue
		}
		old, err := os.ReadFile(path)
		missing := errors.Is(err, fs.ErrNotExist)
		if err != nil && !missing {
			conflicts = append(conflicts, err.Error())
			continue
		}
		if before, registry := preimages[name]; registry && !samePreimage(before, old) {
			conflicts = append(conflicts, name+" changed during sync; rerun check")
			continue
		}
		mode := fs.FileMode(0644)
		if !missing {
			info, err := os.Stat(path)
			if err != nil {
				conflicts = append(conflicts, err.Error())
				continue
			}
			mode = info.Mode().Perm()
		}
		different := missing || !bytes.Equal(old, want.data)
		// Git text conversion on checkout applies to Markdown, JSON and TOML
		// projections. Skill resources, including opaque files, are byte copies.
		if !missing && want.source == "" {
			different = !bytes.Equal(bytes.ReplaceAll(old, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(want.data, []byte("\r\n"), []byte("\n")))
		}
		if want.source != "" {
			required, err := modes.executableAt(root, want.source)
			if err != nil {
				conflicts = append(conflicts, err.Error())
				continue
			}
			have := false
			if !missing || modes.executable != nil {
				have, err = modes.executableAt(root, name)
				if err != nil {
					conflicts = append(conflicts, err.Error())
					continue
				}
			}
			if required {
				mode |= (mode&0444)>>2 | 0100
			} else {
				mode &^= 0111
			}
			if required != have {
				if modes.executable != nil {
					flag := "-x"
					if required {
						flag = "+x"
					}
					windowsModeFixes = append(windowsModeFixes, fmt.Sprintf("%s: stage the executable bit with git add --chmod=%s -- %q", name, flag, name))
				} else {
					different = true
				}
			}
		}
		if different {
			changes = append(changes, change{name, want.data, old, mode})
		}
	}
	slices.Sort(conflicts)
	slices.Sort(windowsModeFixes)
	slices.SortFunc(changes, func(a, b change) int { return strings.Compare(a.name, b.name) })
	return changes, conflicts, windowsModeFixes
}

func samePreimage(before, current []byte) bool {
	return (before == nil) == (current == nil) && bytes.Equal(before, current)
}

func (c change) apply(root string) error {
	path, err := safePath(root, c.name)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !samePreimage(c.before, current) {
		return fmt.Errorf("%s changed during sync; rerun check", c.name)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Open with the final mode so the process umask applies to new files.
	// A replacement keeps the permissions of the existing private file.
	f, err := os.OpenFile(filepath.Join(dir, ".harnessdoctor-"+rand.Text()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, c.mode)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(c.data)
	if err == nil && c.before != nil {
		err = f.Chmod(c.mode)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
