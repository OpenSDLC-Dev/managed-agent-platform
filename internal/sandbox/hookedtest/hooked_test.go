package hookedtest_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// Every primitive a backend implements with a script of its own answers
// through an image whose BASH_ENV file prints around every exec, leaves the
// directory and traps EXIT (#860): on Kubernetes the exec's exit record and
// liveness probe, a read's bytes, a refused write's reason and the export
// probe; on Docker the writability probe's reason and mkdir's — the refusals
// under a read-only root, so they are real ones. The model's own command still
// runs in the image's environment, banner and all.
func TestBackendScriptsAnswerThroughAnImageHook(t *testing.T) {
	for _, b := range hookedtest.Backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			sb, sid := b.Provision(t, sandbox.Hardening{})

			res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "echo hi; exit 7", Timeout: 30 * time.Second})
			if err != nil || res.ExitCode != 7 || res.TimedOut || sandboxtest.Unbanner(res.Stdout) != "hi\n" ||
				!strings.Contains(res.Stdout, "welcome to the image") {
				t.Errorf("exec = %+v, %v; want exit 7, hi, and the image's banner around it", res, err)
			}
			if res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 30", Timeout: 2 * time.Second}); err != nil || !res.TimedOut {
				t.Errorf("a command past its deadline = %+v, %v; want a timeout", res, err)
			}

			payload := []byte{0x00, 'b', 'i', 'n', 0xff, '\n', 'n', 'o', ' ', 'n', 'l'}
			if err := sb.WriteFile(ctx, "/workspace/hooked/blob.bin", payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := sb.WriteFile(ctx, "/workspace/hooked/empty", nil); err != nil {
				t.Fatalf("write an empty file: %v", err)
			}
			if got, err := sb.ReadFile(ctx, "/workspace/hooked/blob.bin"); err != nil || !bytes.Equal(got, payload) {
				t.Errorf("read = %q, %v; want %q", got, err, payload)
			}
			if got, err := sb.ReadFile(ctx, "/workspace/hooked/empty"); err != nil || len(got) != 0 {
				t.Errorf("read an empty file = %q, %v", got, err)
			}
			if rc, n, err := sb.ReadFileStream(ctx, "/workspace/hooked/blob.bin", 1<<20); err != nil {
				t.Errorf("read stream: %v", err)
			} else {
				got, _ := io.ReadAll(rc)
				_ = rc.Close()
				if n != int64(len(payload)) || !bytes.Equal(got, payload) {
					t.Errorf("read stream = %q (%d), want %q", got, n, payload)
				}
			}
			if _, err := sb.ReadFile(ctx, "/workspace/hooked/none"); !errors.Is(err, sandbox.ErrFileNotExist) {
				t.Errorf("read a missing file: %v, want ErrFileNotExist", err)
			}
			if _, err := sb.ReadFile(ctx, "/workspace/hooked"); !errors.Is(err, sandbox.ErrIsDirectory) {
				t.Errorf("read a directory: %v, want ErrIsDirectory", err)
			}

			batch := []sandbox.FileWrite{{Path: "/workspace/hooked/batch/a.md", Data: []byte("alpha")}, {Path: "/workspace/hooked/batch/b/c.md", Data: []byte("gamma")}}
			if err := sb.WriteFiles(ctx, batch); err != nil {
				t.Fatalf("write a batch: %v", err)
			}
			for _, w := range batch {
				if got, err := sb.ReadFile(ctx, w.Path); err != nil || !bytes.Equal(got, w.Data) {
					t.Errorf("read %s = %q, %v; want %q", w.Path, got, err, w.Data)
				}
			}
			// A batch refused at one member names that member, the image's
			// banner on the same stream notwithstanding.
			refused := []sandbox.FileWrite{{Path: "/workspace/hooked/batch/d.md", Data: []byte("delta")}, {Path: "/workspace/hooked/batch/b", Data: []byte("a directory")}}
			if err := sb.WriteFiles(ctx, refused); !errors.Is(err, sandbox.ErrIsDirectory) || !strings.Contains(err.Error(), "/workspace/hooked/batch/b") {
				t.Errorf("a batch onto a directory = %v; want ErrIsDirectory naming /workspace/hooked/batch/b", err)
			}

			rc, err := b.Provider.Export(ctx, sid, "/workspace")
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			var names []string
			tr := tar.NewReader(rc)
			for {
				h, err := tr.Next()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						t.Errorf("export's tar: %v", err)
					}
					break
				}
				names = append(names, h.Name)
			}
			_ = rc.Close()
			if !strings.Contains(strings.Join(names, "\n"), "workspace/hooked/blob.bin") {
				t.Errorf("export holds %v; want the workspace", names)
			}
			if _, err := b.Provider.Export(ctx, sid, "/no/such/root"); !errors.Is(err, sandbox.ErrFileNotExist) {
				t.Errorf("export a missing root: %v, want ErrFileNotExist", err)
			}

			// A refused write says why, in the sandbox's own words and in
			// nothing else: the create refused in an existing directory, and
			// the directory refused.
			ro, _ := b.Provision(t, sandbox.Hardening{ReadOnlyRootfs: true})
			for _, p := range []string{"/usr/hooked.txt", "/usr/hooked/deeper.txt"} {
				var pnw *sandbox.PathNotWritableError
				if err := ro.WriteFile(ctx, p, []byte("x")); !errors.As(err, &pnw) || pnw.Reason != "Read-only file system" {
					t.Errorf("write %s = %v; want ErrNotWritable for the read-only file system, and no banner", p, err)
				}
			}
			// A batch lands under the read-only root where its mounts are
			// writable, and one refused outside them says why the same way.
			if err := ro.WriteFiles(ctx, batch); err != nil {
				t.Errorf("a batch under a read-only root: %v", err)
			} else if got, err := ro.ReadFile(ctx, batch[1].Path); err != nil || !bytes.Equal(got, batch[1].Data) {
				t.Errorf("read %s = %q, %v", batch[1].Path, got, err)
			}
			var pnw *sandbox.PathNotWritableError
			if err := ro.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/usr/hooked/batch.md", Data: []byte("x")}}); !errors.As(err, &pnw) ||
				pnw.Reason != "Read-only file system" {
				t.Errorf("a batch onto the read-only root = %v; want ErrNotWritable for the read-only file system, and no banner", err)
			}
		})
	}
}

// Every call to Backends builds a hooked image of its own. The same Dockerfile
// would build one image ID for every package that asks at once, and removing
// one package's image from a kind node — crictl removes an image by its ID,
// every tag with it — would take the others' out from under their running
// pods. A package that finished, its image removed from the daemon and the
// nodes, leaves another package's image working on both backends.
func TestHookedImagesOfParallelPackagesAreTheirOwn(t *testing.T) {
	kept := hookedtest.Backends(t)
	id := func(image string) string {
		out, err := exec.Command("docker", "--host", docker.DaemonHost(), "image", "inspect", "-f", "{{.Id}}", image).Output()
		if err != nil {
			t.Fatalf("inspect %s: %v", image, err)
		}
		return strings.TrimSpace(string(out))
	}
	t.Run("a package that finished", func(t *testing.T) {
		if other := hookedtest.Backends(t)[0].Image; id(other) == id(kept[0].Image) {
			t.Errorf("two builds made one image, %s", id(other))
		}
	})
	for _, b := range kept {
		sb, _ := b.Provision(t, sandbox.Hardening{})
		if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "echo ok", Timeout: 30 * time.Second}); err != nil ||
			sandboxtest.Unbanner(res.Stdout) != "ok\n" {
			t.Errorf("%s: exec on the image another package's removal left = %+v, %v", b.Name, res, err)
		}
	}
}
