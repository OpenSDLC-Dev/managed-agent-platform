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
// nodes — its tag and its import record both — leaves another package's image
// working on both backends.
func TestHookedImagesOfParallelPackagesAreTheirOwn(t *testing.T) {
	kept := hookedtest.Backends(t)
	id := func(image string) string {
		out, err := exec.Command("docker", "--host", docker.DaemonHost(), "image", "inspect", "-f", "{{.Id}}", image).Output()
		if err != nil {
			t.Fatalf("inspect %s: %v", image, err)
		}
		return strings.TrimSpace(string(out))
	}
	var finished string
	t.Run("a package that finished", func(t *testing.T) {
		finished = hookedtest.Backends(t)[0].Image
		if id(finished) == id(kept[0].Image) {
			t.Errorf("two builds made one image, %s", id(finished))
		}
	})
	// What the finished package loaded onto the kind nodes is gone — its tag
	// and the import record `kind load` leaves beside it — and what the
	// running one loaded is all there.
	if refs, _ := hookedtest.KindRefs(t, finished); len(refs) > 0 {
		t.Errorf("a finished package's image left %v on the kind nodes", refs)
	}
	if refs, loaded := hookedtest.KindRefs(t, kept[0].Image); !loaded {
		t.Logf("%s is not a kind cluster: no node records to check", sandboxtest.KubeContext(t))
	} else if len(refs) < 2 {
		t.Errorf("a running package's image is held on the kind nodes as %v, want its tag and its import record", refs)
	}
	for _, b := range kept {
		sb, _ := b.Provision(t, sandbox.Hardening{})
		if res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "echo ok", Timeout: 30 * time.Second}); err != nil ||
			sandboxtest.Unbanner(res.Stdout) != "ok\n" {
			t.Errorf("%s: exec on the image another package's removal left = %+v, %v", b.Name, res, err)
		}
	}
}

// The platform's own scripts open with a preamble that turns errexit back off
// (sandbox.Script), because an image's startup file can turn it on in their
// shell and they are not written for it (#860): they let an `rm` or a `chmod`
// fail on purpose and read a status after it. As a uid the image did not
// choose, under a read-only root — where those failures are real on Docker,
// the daemon landing every temporary root-owned — and on an image whose
// startup sets -e: a batch lands; a single write and a batch member onto a
// directory are refused as directories, whatever the `rm` of the temporary
// said; and a batch refused at its renames is shed and emptied (#316), the
// shed's report reaching the backend. Without the preamble, on Docker, the
// batch dies at its first `chmod`, both refusals read as the `rm`'s exit, and
// the shed dies before it reports, its payloads left.
func TestPlatformScriptsUnderAnErrexitStartupAsNonRoot(t *testing.T) {
	for _, b := range hookedtest.BackendsFor(t, "set -e\n"+sandboxtest.BannerHook) {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			uid := int64(65534)
			sb, _ := b.Provision(t, sandbox.Hardening{RunAsUser: &uid, ReadOnlyRootfs: true})
			sh := func(command string) string {
				t.Helper()
				res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: command, Timeout: 30 * time.Second})
				if err != nil || res.ExitCode != 0 {
					t.Fatalf("%s = %+v, %v", command, res, err)
				}
				return strings.TrimSpace(sandboxtest.Unbanner(res.Stdout))
			}

			batch := []sandbox.FileWrite{
				{Path: "/tmp/ee/skills/pack/SKILL.md", Data: []byte("# skill")},
				{Path: "/tmp/ee/memory/todo.md", Data: []byte("rw"), Mode: 0o666},
			}
			if err := sb.WriteFiles(ctx, batch); err != nil {
				t.Errorf("a batch as uid %d: %v", uid, err)
			} else {
				for _, f := range batch {
					if got := sh("cat " + f.Path); got != string(f.Data) {
						t.Errorf("cat %s = %q, want %q", f.Path, got, f.Data)
					}
				}
			}

			// Directories in the sticky /tmp: a temporary the daemon landed
			// root-owned beside one is not the sandbox user's to remove there.
			sh("mkdir /tmp/ee-dir-single /tmp/ee-dir-batch")
			if err := sb.WriteFile(ctx, "/tmp/ee-dir-single", []byte("x")); !errors.Is(err, sandbox.ErrIsDirectory) {
				t.Errorf("a write onto a directory = %v, want ErrIsDirectory", err)
			}
			if err := sb.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/tmp/ee-dir-batch", Data: []byte("x")}}); !errors.Is(err, sandbox.ErrIsDirectory) ||
				!strings.Contains(err.Error(), "/tmp/ee-dir-batch") {
				t.Errorf("a batch onto a directory = %v, want ErrIsDirectory naming /tmp/ee-dir-batch", err)
			}

			// A batch into the workdir, which on Docker is root-owned here: its
			// renames are refused, and the shed's report has the daemon empty
			// what the sandbox user could not remove. Where the sandbox user can
			// write the workdir (Kubernetes' emptyDir) the batch lands, and
			// nothing is left.
			big := bytes.Repeat([]byte("P"), 4096)
			err := sb.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/workspace/ee-a.txt", Data: big}, {Path: "/workspace/ee-b.txt", Data: big}})
			// No line at all is a workdir the sandbox user cannot write with
			// no temporary left in it.
			lines := strings.FieldsFunc(sh("[ -w /workspace ] && echo writable; find /workspace -maxdepth 1 -name '"+sandbox.TempPrefix+"*' -printf '%s %f\\n'"), func(r rune) bool { return r == '\n' })
			if len(lines) > 0 && lines[0] == "writable" {
				if err != nil || len(lines) != 1 {
					t.Errorf("a batch into a workdir the sandbox user can write: err = %v, left %q", err, lines[1:])
				}
				return
			}
			if err == nil {
				t.Error("a batch renamed into a workdir the sandbox user cannot write reported success")
			}
			for _, line := range lines {
				if !strings.HasPrefix(line, "0 ") {
					t.Errorf("temporary %q in the workdir still holds a payload, want it emptied", line)
				}
			}
		})
	}
}
