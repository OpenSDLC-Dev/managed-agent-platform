package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
)

// The pinned inputs describe the ripgrep internal/ripgrep embeds: a version
// bump that leaves them behind would ship one release's binaries under
// another's licenses.
func TestSourcesPinThePinnedRelease(t *testing.T) {
	raw, err := os.ReadFile("sources.json")
	if err != nil {
		t.Fatal(err)
	}
	var src sources
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatal(err)
	}
	if src.Ripgrep != ripgrep.Pinned.Version {
		t.Fatalf("sources.json is for ripgrep %s, and internal/ripgrep pins %s", src.Ripgrep, ripgrep.Pinned.Version)
	}
	tag := "/BurntSushi/ripgrep/" + src.Ripgrep + "/"
	var lock, license bool
	for _, l := range src.Lockfiles {
		lock = lock || strings.Contains(l.URL, tag+"Cargo.lock")
	}
	for _, f := range src.Files {
		license = license || strings.Contains(f.URL, tag+"LICENSE-MIT")
	}
	if !lock || !license {
		t.Errorf("sources.json does not take ripgrep's Cargo.lock and LICENSE-MIT from the %s tag", src.Ripgrep)
	}
}

// The committed file is the one the committed sources generate: it names the
// sources' digest, which any edit to them changes, so an edit that was not
// followed by `make third-party-licenses` fails here. (Regenerating needs the
// network; this does not.)
func TestTheGeneratedFileIsCurrent(t *testing.T) {
	raw, err := os.ReadFile("sources.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile("../../THIRD_PARTY_LICENSES")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"embed ripgrep " + ripgrep.Pinned.Version + " ", "sources.json, sha256 " + digest(raw) + ".",
		"ripgrep " + ripgrep.Pinned.Version + " (LICENSE-MIT)", "Copyright (c) 2015 Andrew Gallant"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("THIRD_PARTY_LICENSES does not carry %q; run `make third-party-licenses`", want)
		}
	}
}

// crateArchive builds a .crate holding the given files under its root.
func crateArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: root + "/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func lockEntry(name, version, sum string) string {
	return fmt.Sprintf("[[package]]\nname = %q\nversion = %q\nsource = %q\nchecksum = %q\n\n", name, version, registry, sum)
}

// world is a fake internet: the bytes at each URL.
type world map[string][]byte

func (w world) fetch(_ context.Context, url string) ([]byte, error) {
	b, ok := w[url]
	if !ok {
		return nil, fmt.Errorf("GET %s: 404", url)
	}
	return b, nil
}

// fixture is two lock files sharing a crate, one crate vendoring a
// library's license at depth, one with no license file of its own, and two
// upstream files with the same text.
func fixture(t *testing.T) (world, sources) {
	t.Helper()
	was := crateURL
	crateURL = func(name, version string) string { return "crate:" + name + "-" + version }
	t.Cleanup(func() { crateURL = was })

	alpha := crateArchive(t, "alpha-1.0.0", map[string]string{
		"Cargo.toml":     "[package]\nname = \"alpha\"\nlicense = \"MIT OR Apache-2.0\"\n",
		"LICENSE-MIT":    "MIT text, (c) Alpha\n",
		"LICENSE-APACHE": "Apache text\r\n",
		"src/lib.rs":     "// not a license\n",
	})
	beta := crateArchive(t, "beta-0.2.0+x.1", map[string]string{
		"Cargo.toml":        "[package]\nlicense = \"MIT/Apache-2.0\"\n",
		"LICENSE-APACHE":    "Apache text\n",
		"vendored/COPYING":  "vendored library's terms\n",
		"vendored/main.c":   "int main;\n",
		"tests/data/readme": "x\n",
	})
	gamma := crateArchive(t, "gamma-3.0.0", map[string]string{"Cargo.toml": "[package]\nlicense = \"Zlib\"\n"})
	lock1 := "version = 4\n\n" + lockEntry("alpha", "1.0.0", digest(alpha)) + lockEntry("beta", "0.2.0+x.1", digest(beta)) +
		"[[package]]\nname = \"workspace-member\"\nversion = \"0.1.0\"\n\n"
	lock2 := lockEntry("alpha", "1.0.0", digest(alpha)) + lockEntry("gamma", "3.0.0", digest(gamma))
	w := world{
		"lock:1":                lockAsBytes(lock1),
		"lock:2":                lockAsBytes(lock2),
		"crate:alpha-1.0.0":     alpha,
		"crate:beta-0.2.0+x.1":  beta,
		"crate:gamma-3.0.0":     gamma,
		"file:one":              []byte("Project terms\n"),
		"file:two":              []byte("Project terms\n"),
		"file:gamma-repo-terms": []byte("Zlib text, (c) Gamma\n"),
	}
	src := sources{
		Ripgrep: "9.9.9",
		Lockfiles: []source{
			{Name: "first", Note: "the first lock", URL: "lock:1", SHA256: digest(w["lock:1"])},
			{Name: "second", Note: "the second lock", URL: "lock:2", SHA256: digest(w["lock:2"])},
		},
		Files: []source{
			{Component: "Project 1", Note: "its terms", URL: "file:one", SHA256: digest(w["file:one"])},
			{Component: "Project 2", URL: "file:two", SHA256: digest(w["file:two"])},
		},
		CrateFiles: map[string][]source{"gamma 3.0.0": {{URL: "file:gamma-repo-terms", SHA256: digest(w["file:gamma-repo-terms"])}}},
	}
	return w, src
}

func lockAsBytes(s string) []byte { return []byte(s) }

func TestGenerateWritesEachTextOnceNamingWhoShipsIt(t *testing.T) {
	w, src := fixture(t)
	out, err := generate(context.Background(), w.fetch, src, "feed")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"embed ripgrep 9.9.9 as upstream builds it",
		"sources.json, sha256 feed.",
		"- Project 1: file:one\n  its terms\n",
		"- first: lock:1\n  the first lock\n",
		"- alpha 1.0.0 — MIT OR Apache-2.0 — in first; second\n",
		"- beta 0.2.0+x.1 — MIT/Apache-2.0 — in first\n",
		"- gamma 3.0.0 — Zlib — in second\n",
		// Identical texts, CRLF or not, are one, headed by all who ship it.
		"1. Project 1 (file:one), Project 2 (file:two)\n",
		"alpha 1.0.0 (LICENSE-APACHE), beta 0.2.0+x.1 (LICENSE-APACHE)\n" + strings.Repeat("=", 78) + "\n\nApache text\n",
		"beta 0.2.0+x.1 (vendored/COPYING)\n" + strings.Repeat("=", 78) + "\n\nvendored library's terms\n",
		"gamma 3.0.0 (file:gamma-repo-terms)\n" + strings.Repeat("=", 78) + "\n\nZlib text, (c) Gamma\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"workspace-member", "not a license", "int main", "\r"} {
		if strings.Contains(text, not) {
			t.Errorf("output carries %q", not)
		}
	}
	again, err := generate(context.Background(), w.fetch, src, "feed")
	if err != nil || !bytes.Equal(out, again) {
		t.Errorf("a second run differs (%v)", err)
	}
}

// Every input is checked against its pin, and an input this tool cannot
// stand behind stops the run rather than leaving a gap in the file.
func TestGenerateRefusesWhatItCannotCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(world, *sources)
		want string
	}{
		"a lock file that is not its pin":              {func(w world, _ *sources) { w["lock:1"] = append(w["lock:1"], '\n') }, "not the pinned"},
		"a crate that is not its lock file's checksum": {func(w world, _ *sources) { w["crate:beta-0.2.0+x.1"] = []byte("x") }, "not the pinned"},
		"a file that is not its pin":                   {func(w world, _ *sources) { w["file:two"] = []byte("other") }, "not the pinned"},
		"a crate with no license text at all": {func(_ world, s *sources) { s.CrateFiles = nil },
			"gamma 3.0.0 ships no license file and crate_files supplies none"},
		"crate files for a crate no lock names": {func(_ world, s *sources) { s.CrateFiles["delta 1.0.0"] = nil }, "crate_files names delta 1.0.0"},
		"a crate from git": {func(w world, s *sources) {
			w["lock:2"] = []byte("[[package]]\nname = \"g\"\nversion = \"1.0.0\"\nsource = \"git+https://example.invalid/g\"\n")
			s.Lockfiles[1].SHA256 = digest(w["lock:2"])
		}, "comes from git+https://example.invalid/g"},
		"a registry crate with no checksum": {func(w world, s *sources) {
			w["lock:2"] = []byte("[[package]]\nname = \"g\"\nversion = \"1.0.0\"\nsource = \"" + registry + "\"\n")
			s.Lockfiles[1].SHA256 = digest(w["lock:2"])
		}, "g 1.0.0 has no sha256 checksum"},
		"a download that fails": {func(w world, _ *sources) { delete(w, "file:one") }, "GET file:one: 404"},
	} {
		w, src := fixture(t)
		tc.edit(w, &src)
		if _, err := generate(context.Background(), w.fetch, src, "feed"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestWrap(t *testing.T) {
	long := "- " + strings.Repeat("word ", 30)
	for _, line := range strings.Split(wrap(long, "  "), "\n") {
		if len(line) > 78 {
			t.Errorf("line of %d columns: %q", len(line), line)
		}
	}
	url := "- x: https://example.invalid/" + strings.Repeat("a", 90)
	if got := wrap(url, "  "); got != "- x:\n  https://example.invalid/"+strings.Repeat("a", 90) {
		t.Errorf("wrap(url) = %q, want the URL on a line of its own, whole", got)
	}
}
