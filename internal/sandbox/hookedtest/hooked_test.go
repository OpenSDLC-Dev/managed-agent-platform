package hookedtest_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
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
	if missing, loaded := hookedtest.KindMissing(t, kept[0].Image); !loaded {
		t.Logf("%s is not a kind cluster: no node records to check", sandboxtest.KubeContext(t))
	} else if len(missing) > 0 {
		t.Errorf("a running package's image is missing %v on the kind nodes, want its tag and its import record held", missing)
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

// An image that sets SHELLOPTS — to anything, even nothing — has bash export
// it, and keep the exported value in step with the shell's options (#866). The
// Docker exec wrapper turns job control on (`set -m`) for its watchdog, so the
// command's own shell, exec'd from the wrapper, started with job control too,
// and such a shell forks each of its jobs into a process group of its own: the
// watchdog's group kill took the command's shell and missed its child, and the
// deadline was reported on work that ran on. An image that names monitor in
// SHELLOPTS itself turns job control on in both backends' wrappers, and on
// Kubernetes the runaway then read as a command that exited 0, its deadline
// neither enforced nor reported. On both images and both backends a runaway
// past its deadline is a timeout and its child dies with it; and the image's
// other options still reach the command, only job control taken out of them.
func TestADeadlineKillsTheCommandsChildrenUnderAnImagesShellopts(t *testing.T) {
	for _, image := range []struct{ name, shellopts, flag string }{
		{"empty", "", ""},
		{"monitor and notify", "monitor:notify", "b"},
	} {
		for _, b := range hookedtest.BackendsFor(t, "", "SHELLOPTS="+image.shellopts) {
			t.Run(image.name+"/"+b.Name, func(t *testing.T) {
				ctx := context.Background()
				sb, _ := b.Provision(t, sandbox.Hardening{})
				res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `echo "$-"`, Timeout: 30 * time.Second})
				if err != nil || res.ExitCode != 0 {
					t.Fatalf(`echo "$-" = %+v, %v`, res, err)
				}
				flags := strings.TrimSpace(res.Stdout)
				if strings.Contains(flags, "m") {
					t.Errorf("the command's shell runs with job control on: flags %q", flags)
				}
				if !strings.Contains(flags, image.flag) {
					t.Errorf("the command's shell runs with flags %q, without the image's %q", flags, image.flag)
				}

				res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 986601; :", Timeout: time.Second})
				if err != nil {
					t.Fatalf("exec: %v", err)
				}
				if res.ExitCode != 137 || !res.TimedOut {
					t.Errorf("a runaway past its deadline = %+v; want a timeout, and the kill's 137", res)
				}
				// The kill lands at the deadline; the count waits out the
				// reaping rather than racing it.
				var n int
				for range 50 {
					if n = sandboxtest.CountProcesses(t, sb, "sleep 986601"); n == 0 {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				t.Errorf("%d child(ren) of the command outlived its deadline: its shell was killed, its work was not", n)
			})
		}
	}
}

// The preamble opens scripts a POSIX sh runs too — the Kubernetes export
// probe's `sh -c` — so its bash-only lines are skipped there: BusyBox's sh,
// an Alpine image's /bin/sh, exits on `set +p`. A script the preamble opens
// runs to its end under it, with nothing on stderr.
func TestScriptPreambleUnderBusyBoxSh(t *testing.T) {
	dp, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	image := dockertest.ImageFrom(t, "busyboxsh", "FROM bash:3.2\nRUN ln -s /usr/local/bin/bash /bin/bash\n", "--host", docker.DaemonHost())
	sb, _ := hookedtest.Backend{Name: "docker", Provider: dp, Image: image}.Provision(t, sandbox.Hardening{})
	ctx := context.Background()
	exec := func(command string) sandbox.ExecResult {
		t.Helper()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: command, Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return res
	}
	if res := exec("readlink -f /bin/sh"); strings.TrimSpace(res.Stdout) != "/bin/busybox" {
		t.Fatalf("/bin/sh = %+v; want BusyBox's", res)
	}
	script := "'" + strings.ReplaceAll(sandbox.Script("echo P"), "'", `'\''`) + "'"
	if res := exec("sh -c " + script); res.Stdout != "P\n" || res.Stderr != "" || res.ExitCode != 0 {
		t.Errorf("sh -c of a script the preamble opens = %+v; want P, nothing on stderr, exit 0", res)
	}
}

// exitHook is an image's `ENV BASH_ENV` file that ends one shell before the
// platform's script in it begins: the first whose script carries the frame
// label /tmp/map-startup-exit names, with the status written beside the label,
// removing the file as it does. The test arms it with a command of its own,
// then calls the primitive whose script carries the label.
const exitHook = `if [ -r /tmp/map-startup-exit ]; then read -r __map_l __map_s < /tmp/map-startup-exit; ` +
	`case "$BASH_EXECUTION_STRING $*" in *"map-$__map_l-begin-"*) rm -f /tmp/map-startup-exit; exit "$__map_s";; esac; fi` + "\n"

// A platform script whose shell the image's startup ended before the script
// began takes no answer from the status the startup left, whatever it is
// (#942). Where the status was the primitive's answer, each status the script
// reads, 0 among them, comes back as the startup's exit
// (sandbox.StartupExitError), and the primitive did not do what that status
// would have said it did: on Docker the single write's mkdir and rename and the
// bulk write's prepare and rename; on Kubernetes the read, the write, the bulk
// write and its prepare, and the exec's exit record. Where the script only
// classifies a refusal the backend already has, it answers nothing, and the
// refusal stays unclassified: on Docker the path, replaceability and
// writability probes and the bulk refusal probe; on Kubernetes the bulk
// refusal probe. Each row checks that the startup ended the script it names,
// so a row whose script never ran fails.
func TestPlatformScriptsTakeNoStatusAStartupChose(t *testing.T) {
	for _, b := range hookedtest.BackendsFor(t, sandboxtest.BannerHook+exitHook) {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			sb, _ := b.Provision(t, sandbox.Hardening{})
			uid := int64(65534)
			ro, _ := b.Provision(t, sandbox.Hardening{RunAsUser: &uid, ReadOnlyRootfs: true})
			sh := func(s sandbox.Sandbox, command string) (sandbox.ExecResult, error) {
				return s.Exec(ctx, sandbox.ExecRequest{Command: command, Timeout: 30 * time.Second})
			}
			armCommand := func(label string, status int) string {
				return fmt.Sprintf("printf '%%s %%d\\n' %s %d > /tmp/map-startup-exit", label, status)
			}
			arm := func(s sandbox.Sandbox, label string, status int) {
				t.Helper()
				if res, err := sh(s, armCommand(label, status)); err != nil || res.ExitCode != 0 {
					t.Fatalf("arm %s %d = %+v, %v", label, status, res, err)
				}
			}
			fired := func(s sandbox.Sandbox, label string, status int) {
				t.Helper()
				if res, err := sh(s, "if [ -e /tmp/map-startup-exit ]; then rm -f /tmp/map-startup-exit; exit 3; fi"); err != nil || res.ExitCode != 0 {
					t.Errorf("%s ended with %d: the startup never ended the script it names (%+v, %v)", label, status, res, err)
				}
			}
			ended := func(s sandbox.Sandbox, label string, status int, err error) {
				t.Helper()
				var startup *sandbox.StartupExitError
				if !errors.As(err, &startup) || startup.Status != status {
					t.Errorf("%s ended with %d: %v; want the startup's exit with that status", label, status, err)
				}
				fired(s, label, status)
			}
			unclassified := func(s sandbox.Sandbox, label string, status int, err, wrong error) {
				t.Helper()
				if err == nil || errors.Is(err, wrong) {
					t.Errorf("%s ended with %d: %v; want the backend's own refusal, not %v", label, status, err, wrong)
				}
				fired(s, label, status)
			}
			absent := func(path string) {
				t.Helper()
				if _, err := sb.ReadFile(ctx, path); !errors.Is(err, sandbox.ErrFileNotExist) {
					t.Errorf("read %s = %v; want it not written", path, err)
				}
			}
			batch := func(dir string) []sandbox.FileWrite {
				return []sandbox.FileWrite{{Path: dir + "/a.md", Data: []byte("alpha")}, {Path: dir + "/b/c.md", Data: []byte("gamma")}}
			}
			x := []byte("x")

			switch b.Name {
			case "docker":
				for _, status := range []int{0, 1, sandbox.ExitPathNotDirectory} {
					dir := fmt.Sprintf("/workspace/mkdir%d", status)
					arm(sb, "mkdir", status)
					ended(sb, "mkdir", status, sb.WriteFile(ctx, dir+"/f", x))
					absent(dir + "/f")
				}
				for _, status := range []int{0, 1, sandbox.ExitPathIsDirectory, sandbox.ExitPathNotReplaceable} {
					path := fmt.Sprintf("/workspace/rename%d", status)
					arm(sb, "rename", status)
					ended(sb, "rename", status, sb.WriteFile(ctx, path, x))
					absent(path)
				}
				for _, label := range []string{"bulkprepare", "bulkrename"} {
					for _, status := range []int{0, 1} {
						dir := fmt.Sprintf("/workspace/%s%d", label, status)
						arm(sb, label, status)
						ended(sb, label, status, sb.WriteFiles(ctx, batch(dir)))
						absent(dir + "/a.md")
					}
				}
				// A read-only root refuses every archive PUT, and a read through a
				// file is a daemon error, so each probe below classifies a
				// refusal the daemon already made.
				arm(ro, "pathfault", sandbox.ExitPathNotDirectory)
				_, err := ro.ReadFile(ctx, "/etc/passwd/x")
				unclassified(ro, "pathfault", sandbox.ExitPathNotDirectory, err, sandbox.ErrNotDirectory)
				for _, status := range []int{sandbox.ExitPathIsDirectory, sandbox.ExitPathNotReplaceable} {
					arm(ro, "replaceable", status)
					err := ro.WriteFile(ctx, "/etc/map-replaceable", x)
					unclassified(ro, "replaceable", status, err, sandbox.ErrIsDirectory)
				}
				arm(ro, "writable", sandbox.ExitPathNotWritable)
				unclassified(ro, "writable", sandbox.ExitPathNotWritable, ro.WriteFile(ctx, "/etc/map-writable", x), sandbox.ErrNotWritable)
				arm(ro, "bulkrefused", sandbox.ExitPathIsDirectory)
				unclassified(ro, "bulkrefused", sandbox.ExitPathIsDirectory,
					ro.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/etc/map-bulk.md", Data: x}}), sandbox.ErrIsDirectory)
			case "k8s":
				if err := sb.WriteFile(ctx, "/workspace/present", []byte("here")); err != nil {
					t.Fatalf("write: %v", err)
				}
				for _, status := range []int{0, 10, 11, 12, 13, sandbox.ExitPathNotDirectory} {
					arm(sb, "read", status)
					got, err := sb.ReadFile(ctx, "/workspace/present")
					ended(sb, "read", status, err)
					if got != nil {
						t.Errorf("read ended with %d answered %q", status, got)
					}
				}
				for _, status := range []int{0, 14, sandbox.ExitPathNotDirectory, sandbox.ExitPathIsDirectory, sandbox.ExitPathNotReplaceable, sandbox.ExitPathNotWritable} {
					path := fmt.Sprintf("/workspace/write%d", status)
					arm(sb, "write", status)
					ended(sb, "write", status, sb.WriteFile(ctx, path, x))
					absent(path)
				}
				for _, status := range []int{0, sandbox.ExitPathIsDirectory, sandbox.ExitBulkExtract, sandbox.ExitPathNotReplaceable, sandbox.ExitPathNotWritable} {
					dir := fmt.Sprintf("/workspace/bulk%d", status)
					arm(sb, "bulkwrite", status)
					ended(sb, "bulkwrite", status, sb.WriteFiles(ctx, batch(dir)))
					absent(dir + "/a.md")
				}
				// A member a file blocks fails the extraction, which runs the
				// prepare pass.
				if err := sb.WriteFile(ctx, "/workspace/blocker", x); err != nil {
					t.Fatalf("write: %v", err)
				}
				for _, status := range []int{0, sandbox.ExitPathNotDirectory} {
					arm(sb, "bulkprepare", status)
					ended(sb, "bulkprepare", status, sb.WriteFiles(ctx, batch("/workspace/blocker/sub")))
				}
				// A directory the sandbox user cannot create in fails the
				// extraction after the prepare pass, which runs the refusal probe.
				for _, status := range []int{sandbox.ExitPathNotWritable, sandbox.ExitPathIsDirectory} {
					wrong := sandbox.ErrNotWritable
					if status == sandbox.ExitPathIsDirectory {
						wrong = sandbox.ErrIsDirectory
					}
					arm(ro, "bulkrefused", status)
					unclassified(ro, "bulkrefused", status,
						ro.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/etc/map-bulk.md", Data: x}}), wrong)
				}
				// The exec's own exit record: the command that arms it has run.
				for _, status := range []int{0, 7} {
					_, err := sh(sb, armCommand("exit", status))
					var startup *sandbox.StartupExitError
					if !errors.As(err, &startup) || !startup.Ran {
						t.Errorf("exit record ended with %d: %v; want the startup's exit of a command that ran", status, err)
					}
					ended(sb, "exit", status, err)
				}
			}
		})
	}
}

// A startup that prints past the output cap on stdout in every shell pushes
// each platform script's frame out of stdout (#860), but not out of stderr,
// where the frame's lines arrive whole: a write into a new directory, a write
// over a file, a batch and a refused write still answer through it, on both
// backends (#942).
func TestPlatformWritesAnswerThroughAStdoutFlood(t *testing.T) {
	for _, b := range hookedtest.BackendsFor(t, "yes | head -c 1200000\n") {
		t.Run(b.Name, func(t *testing.T) {
			ctx := context.Background()
			sb, _ := b.Provision(t, sandbox.Hardening{})
			want := map[string]string{"/workspace/flood/new/f": "two", "/workspace/flood/batch/a.md": "alpha", "/workspace/flood/batch/b/c.md": "gamma"}
			for _, data := range []string{"one", "two"} {
				if err := sb.WriteFile(ctx, "/workspace/flood/new/f", []byte(data)); err != nil {
					t.Fatalf("write %q under a flood: %v", data, err)
				}
			}
			batch := []sandbox.FileWrite{{Path: "/workspace/flood/batch/a.md", Data: []byte("alpha")}, {Path: "/workspace/flood/batch/b/c.md", Data: []byte("gamma")}}
			if err := sb.WriteFiles(ctx, batch); err != nil {
				t.Fatalf("a batch under a flood: %v", err)
			}
			if err := sb.WriteFile(ctx, "/workspace/flood/new", []byte("x")); !errors.Is(err, sandbox.ErrIsDirectory) {
				t.Errorf("a write onto a directory under a flood = %v; want ErrIsDirectory", err)
			}
			for path, data := range want {
				if got, err := sb.ReadFile(ctx, path); err != nil || string(got) != data {
					t.Errorf("read %s = %q, %v; want %q", path, got, err, data)
				}
			}
		})
	}
}
