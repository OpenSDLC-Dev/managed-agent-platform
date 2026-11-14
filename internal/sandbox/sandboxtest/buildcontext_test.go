package sandboxtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Removing the root cache exclusion must fail this test; excluding its whole
// directory or matching the same path at every depth must fail it too. Only the
// ignore rules come from the checkout. No local cache contents enter the probe.
func TestDockerContextExcludesOnlyRootHookCache(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repo root")
	}
	ignore, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	contextDir, outputDir := t.TempDir(), t.TempDir()
	const marker = "synthetic public context marker\n"
	const cache = ".impeccable/hook.cache.json"
	kept := []string{"context-marker.txt", ".impeccable/keep.txt", "nested/" + cache}
	files := map[string]string{
		"Dockerfile":    "FROM scratch\nCOPY . /context/\n",
		".dockerignore": string(ignore),
		cache:           marker,
	}
	for _, name := range kept {
		files[name] = marker
	}
	for name, body := range files {
		path := filepath.Join(contextDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// FROM scratch and a local export need no base image, container or tag.
	// A successful COPY, not a missing source or failed build, proves exclusion.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "--no-cache", "--network=none",
		"--progress=plain", "--output", "type=local,dest="+outputDir, contextDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build synthetic Docker context: %v\n%s", err, out)
	}
	t.Logf("synthetic Docker context %s exported to %s\n%s", contextDir, outputDir, out)
	if _, err := os.Lstat(filepath.Join(outputDir, "context", filepath.FromSlash(cache))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("COPY included root hook cache or could not check its absence: %v", err)
	}
	for _, name := range kept {
		got, err := os.ReadFile(filepath.Join(outputDir, "context", filepath.FromSlash(name)))
		if err != nil || string(got) != marker {
			t.Errorf("COPY must retain %s: read error %v, marker matches %v", name, err, string(got) == marker)
		}
	}
}
