package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestGoBuildVersion(t *testing.T) {
	if got := goBuildVersion(nil); got != "unknown" {
		t.Fatalf("no build info: got %q", got)
	}
	const rev = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tc := range []struct{ name, version, vcs, revision, want string }{
		{"development", "v0.6.1-0.20261121040104-abcdef012345", "git", rev, "v0.6.0-dev-abcdef01"},
		{"dirty checkout", "v0.6.11-0.20261121040104-abcdef012345+dirty", "git", rev, "v0.6.10-dev-abcdef01"},
		{"local build on a tag", "v1.2.3", "git", rev, "v1.2.3-dev-abcdef01"},
		{"no base tag", "v0.0.0-20261121040104-abcdef012345", "git", rev, "unknown"},
		{"only prerelease recorded", "v0.7.0-rc.1.0.20261121040104-abcdef012345", "git", rev, "unknown"},
		{"gitless module archive", "v0.6.0", "", "", "unknown"},
		{"no module version", "(devel)", "git", rev, "unknown"},
		{"different VCS", "v0.6.0", "hg", rev, "unknown"},
		{"no revision", "v0.6.0", "git", "", "unknown"},
		{"invalid revision", "v0.6.0", "git", "NOHASH00", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{
				Main:     debug.Module{Version: tc.version},
				Settings: []debug.BuildSetting{{Key: "vcs", Value: tc.vcs}, {Key: "vcs.revision", Value: tc.revision}},
			}
			if got := goBuildVersion(info); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinkedVersion(t *testing.T) {
	for stamp, want := range map[string]string{
		"1.2.3":               "v1.2.3",
		"v1.2.3":              "v1.2.3",
		"v1.2.3-dev-abcdef01": "v1.2.3-dev-abcdef01",
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
	dir := t.TempDir()
	script, err := os.ReadFile("../../tools/build-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	toolsDir := filepath.Join(dir, "tools")
	if err := os.Mkdir(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(toolsDir, "build-version.sh")
	if err := os.WriteFile(scriptPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	check := func(want string) {
		t.Helper()
		if got := run(scriptPath); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	check("unknown") // unpacked source archive
	run("git", "init", "-q", "-b", "main")
	run("git", "config", "user.name", "Fixture")
	run("git", "config", "user.email", "fixture@example.invalid")
	run("git", "config", "core.abbrev", "16")
	run("git", "commit", "-qm", "root", "--allow-empty")
	check("unknown") // Git revision but no published baseline
	run("git", "tag", "v0.9.0")
	run("git", "tag", "-a", "v0.10.0", "-m", "release")
	want := func() string { return "v0.10.0-dev-" + run("git", "rev-parse", "HEAD")[:8] }
	check(want()) // a development build on the release commit
	run("git", "switch", "-qc", "unrelated")
	run("git", "commit", "-qm", "unrelated release", "--allow-empty")
	run("git", "tag", "v2.0.0")
	run("git", "switch", "-q", "main")
	run("git", "commit", "-qm", "development", "--allow-empty")
	for _, tag := range []string{"v0.11.0-rc.1", "v9.09.0", "v99.0.0+demo", "v2", "tools/v3.0.0"} {
		run("git", "tag", tag)
	}
	check(want())
}
