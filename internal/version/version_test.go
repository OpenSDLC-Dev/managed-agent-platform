package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLinkedVersion(t *testing.T) {
	for stamp, want := range map[string]string{
		"1.2.3":               "v1.2.3",
		"v1.2.3":              "v1.2.3",
		"v1.2.3-dev-abcdef01": "v1.2.3-dev-abcdef01",
		"1.2.3-dev-abcdef01":  "unknown",
		"dev":                 "unknown",
		"1.02.3":              "unknown",
		"v1.2.3-rc":           "unknown",
		"v1.2.3-dev-ABCDE123": "unknown",
		"v1.2.3-dev-abcd":     "unknown",
		"unknown":             "unknown",
		"":                    "unknown",
	} {
		if got := linkedVersion(stamp); got != want {
			t.Errorf("linked %q: got %q, want %q", stamp, got, want)
		}
	}
}

func TestCheckoutBuildVersion(t *testing.T) {
	// Keep this synthetic module's release tags out of the caller's Go cache.
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	write := func(name string, contents []byte, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, mode); err != nil {
			t.Fatal(err)
		}
	}
	const script = "tools/build-version.sh"
	for _, name := range []string{"Makefile", "go.mod", "go.sum", "internal/version/version.go", script} {
		data, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == script {
			mode = 0o755
		}
		write(name, data, mode)
	}
	write("cmd/worker/main.go", []byte(`package main

import (
	"fmt"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/version"
)

func main() {
	fmt.Println(version.Version)
}
`), 0o644)
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	check := func(want string) {
		t.Helper()
		if got := run(filepath.Join(dir, script)); got != want {
			t.Errorf("checkout resolver: got %q, want %q", got, want)
		}
	}
	check("unknown") // unpacked source archive
	run("git", "init", "-q", "-b", "main")
	run("git", "config", "user.name", "Fixture")
	run("git", "config", "user.email", "fixture@example.invalid")
	run("git", "config", "core.abbrev", "16")
	run("git", "add", ".")
	run("git", "commit", "-qm", "root")
	check("unknown") // Git revision but no published baseline
	run("git", "tag", "v0.9.0")
	run("git", "tag", "-a", "v0.10.0", "-m", "release")
	want := func(base string) string { return base + "-dev-" + run("git", "rev-parse", "HEAD")[:8] }
	run("git", "config", "column.ui", "always")
	check(want("v0.10.0")) // a development build on the release commit
	run("git", "config", "--unset", "column.ui")
	run("git", "switch", "-qc", "unrelated")
	run("git", "commit", "-qm", "unrelated release", "--allow-empty")
	run("git", "tag", "v2.0.0")
	run("git", "switch", "-q", "main")
	run("git", "commit", "-qm", "development", "--allow-empty")
	for _, tag := range []string{"v0.11.0-rc.1", "v9.09.0", "v0.80.0+preview", "v2", "tools/v3.0.0"} {
		run("git", "tag", tag)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	checkBinaries := func(base string) {
		t.Helper()
		check(want(base))
		run("go", "build", "-o", bare, "./cmd/worker")
		if got := run(bare); got != "unknown" {
			t.Errorf("unstamped binary reports %q with baseline %s", got, base)
		}
		// Exercise the real build recipe. This tiny worker has no ripgrep assets.
		run("make", "-s", "-o", "ripgrep", "build-worker")
		if got := run(filepath.Join(dir, "worker")); got != want(base) {
			t.Errorf("build-worker produced %q, want %q", got, want(base))
		}
	}
	run("git", "commit", "-qm", "after metadata tag", "--allow-empty")
	checkBinaries("v0.10.0")
	run("git", "tag", "v2.1.0")
	run("git", "commit", "-qm", "after new major", "--allow-empty")
	run("git", "tag", "v0.7.0")
	checkBinaries("v2.1.0")
}
