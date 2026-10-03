package hookedtest_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
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
			if err != nil || res.ExitCode != 7 || res.TimedOut || hookedtest.Unbanner(res.Stdout) != "hi\n" ||
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
		})
	}
}
