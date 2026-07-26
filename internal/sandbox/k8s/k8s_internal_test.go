package k8s

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	gopath "path"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// These unit tests cover the branches a real cluster cannot easily stage —
// adoption, foreign-pod rejection, validation, a pod that fails before it is
// ready, and the not-found reclassification — with a fake clientset. The exec,
// deadline, file, and networking paths are covered by the contract test
// (k8s_test.go) against a live cluster, which the fake clientset cannot drive.

func fakeProvider(objs ...runtime.Object) *Provider {
	return &Provider{
		client:        &client{cs: fake.NewClientset(objs...), namespace: "default"},
		netSetupImage: "busybox",
	}
}

func readyPod(sid domain.ID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName(sid), Namespace: "default",
			Labels: map[string]string{sessionLabel: string(sid)},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, Ready: true}},
		},
	}
}

func TestPodNameSanitizesSessionID(t *testing.T) {
	if got := podName(domain.ID("sesn_ABC123")); got != "map-sesn-abc123" {
		t.Errorf("podName = %q, want map-sesn-abc123 (one '_' → '-', lowercased)", got)
	}
}

func TestOurs(t *testing.T) {
	sid := domain.ID("sesn_x")
	if err := ours(readyPod(sid), sid); err != nil {
		t.Errorf("ours(matching label) = %v, want nil", err)
	}
	foreign := readyPod(sid)
	foreign.Labels[sessionLabel] = "sesn_someone_else"
	if err := ours(foreign, sid); err == nil {
		t.Error("ours(mismatched label) = nil, want an error")
	}
}

func TestProvisionValidates(t *testing.T) {
	p := fakeProvider()
	if _, err := p.Provision(context.Background(), sandbox.Spec{Image: "img"}); err == nil {
		t.Error("provision without a session id: want an error")
	}
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: domain.NewID("sesn")}); err == nil {
		t.Error("provision without an image: want an error")
	}
}

func TestProvisionAdoptsReadyPod(t *testing.T) {
	sid := domain.ID("sesn_adopt")
	p := fakeProvider(readyPod(sid))
	sb, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"})
	if err != nil {
		t.Fatalf("provision (adopt): %v", err)
	}
	if sb.ID() != podName(sid) {
		t.Errorf("adopted sandbox id = %q, want %q", sb.ID(), podName(sid))
	}
}

func TestProvisionRejectsForeignPod(t *testing.T) {
	sid := domain.ID("sesn_foreign")
	foreign := readyPod(sid) // right name, wrong owner
	foreign.Labels[sessionLabel] = "not-this-session"
	p := fakeProvider(foreign)
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"}); err == nil {
		t.Error("provision adopting a foreign pod: want an error")
	}
}

func TestProvisionWaitsForReadinessAndFailsClosed(t *testing.T) {
	sid := domain.ID("sesn_failed")
	failed := readyPod(sid)
	failed.Status.Phase = corev1.PodFailed
	failed.Status.ContainerStatuses = nil
	p := fakeProvider(failed)
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"}); err == nil {
		t.Error("provision of a pod that failed before ready: want an error")
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	sid := domain.ID("sesn_destroy")
	p := fakeProvider(readyPod(sid))
	sb, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := sb.Destroy(context.Background()); err != nil {
		t.Errorf("first destroy: %v", err)
	}
	if err := sb.Destroy(context.Background()); err != nil {
		t.Errorf("second destroy (pod already gone): %v, want nil", err)
	}
}

func TestExecErrReclassifiesVanishedPod(t *testing.T) {
	sid := domain.ID("sesn_execerr")
	ctx := context.Background()

	// No pod exists: a generic exec error becomes ErrNotFound once the existence
	// check confirms the pod is gone (remotecommand's upgrade error hides this).
	gone := fakeProvider().attach(podName(sid), "/workspace")
	if gone.execErr(ctx, nil) != nil {
		t.Error("execErr(nil) = non-nil, want nil")
	}
	if err := gone.execErr(ctx, errors.New("unable to upgrade connection")); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("execErr(absent pod) = %v, want ErrNotFound", err)
	}
	structured := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, podName(sid))
	if err := gone.execErr(ctx, structured); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("execErr(structured NotFound) = %v, want ErrNotFound", err)
	}

	// The pod is present: a transient error is surfaced unchanged, not masked as
	// a vanished sandbox.
	live := fakeProvider(readyPod(sid)).attach(podName(sid), "/workspace")
	transient := errors.New("transient stream reset")
	if err := live.execErr(ctx, transient); err != transient {
		t.Errorf("execErr(present pod, transient) = %v, want the original error", err)
	}
}

func TestCappedBuffer(t *testing.T) {
	var c cappedBuffer
	c.limit = 4
	_, _ = c.Write([]byte("ab"))   // within
	_, _ = c.Write([]byte("cdef")) // straddles the cap: keeps "cd"
	if c.String() != "abcd" || !c.truncated {
		t.Errorf("after straddle: buf=%q truncated=%v, want abcd/true", c.String(), c.truncated)
	}
	_, _ = c.Write([]byte("more")) // already full
	if c.String() != "abcd" {
		t.Errorf("wrote past the cap: %q", c.String())
	}
	var empty cappedBuffer
	empty.limit = 2
	if n, _ := empty.Write(nil); n != 0 || empty.truncated {
		t.Error("empty write should be a no-op")
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	if _, err := New(Config{Kubeconfig: "/definitely/not/a/kubeconfig", Context: "nonexistent"}); err == nil {
		t.Error("New with an unusable kubeconfig+context: want an error")
	}
}

func TestProvisionSurfacesCreateError(t *testing.T) {
	sid := domain.ID("sesn_cerr")
	cs := fake.NewClientset() // no pod, so the first Get 404s and Provision reaches Create
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver rejected the create")
	})
	p := &Provider{client: &client{cs: cs, namespace: "default"}, netSetupImage: "busybox"}
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"}); err == nil {
		t.Error("provision with a failing create: want an error")
	}
}

func TestProvisionReclaimsUnreadyPodItCreated(t *testing.T) {
	sid := domain.ID("sesn_reclaim")
	cs := fake.NewClientset() // no pod yet: the existence Get 404s and Provision creates
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		// The pod comes up Failed (so waitReady fails closed at once instead of
		// polling to the readiness timeout) and carries a UID (so reclaimUnready's
		// UID-guarded delete has an identity to match).
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodFailed
		pod.UID = "uid-reclaim-test"
		return false, nil, nil // fall through to the tracker, which stores the mutated pod
	})
	p := &Provider{client: &client{cs: cs, namespace: "default"}, netSetupImage: "busybox"}
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"}); err == nil {
		t.Fatal("provision of a pod that never became ready: want an error")
	}
	// The pod it created must be gone, so a retry of this session starts clean
	// rather than re-adopting a wedged pod and failing the same way.
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Provision left its unready pod behind: get err = %v, want NotFound", err)
	}
}

// writeScript is the one place a short exec stdin stream can be caught, so its
// exit-code contract is pinned here rather than left to the live cluster:
// declaring a length the stdin bytes do not match reproduces the signature
// deterministically, on any machine, in milliseconds, without needing a cluster
// to lose the bytes for real.
//
// This runs the script through the host's /bin/bash rather than the sandbox
// image. It pins what the script does with its arguments; that the image carries
// a shell able to run it is the live contract test's job.
func TestWriteScriptVerifiesDeliveredLength(t *testing.T) {
	// The temporary file the script writes through is named by Go, exactly as
	// WriteFileStream names it, and handed back so a test can assert it is gone:
	// every failure path in the script has to take its own residue with it.
	//
	// The prologue runs ahead of the script in the same shell. It is how the one
	// row that needs the image's umask stages it: a umask is a property of the
	// process the exec starts, not something an argument or the environment can
	// carry, so it has to be set by a shell the script then inherits from.
	runScript := func(t *testing.T, prologue string, stdin []byte, declared int, path string, env ...string) (int, string) {
		t.Helper()
		tmp := gopath.Join(gopath.Dir(path), sandbox.TempName())
		cmd := exec.Command("/bin/bash", "-c", prologue+writeScript, "map-write", path,
			gopath.Dir(path), strconv.Itoa(declared), tmp)
		cmd.Env = env // nil inherits, which is what every row but the shimmed one wants
		cmd.Stdin = bytes.NewReader(stdin)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run writeScript: %v", err)
			}
			return ee.ExitCode(), tmp
		}
		return 0, tmp
	}
	run := func(t *testing.T, stdin []byte, declared int, path string, env ...string) (int, string) {
		t.Helper()
		return runScript(t, "", stdin, declared, path, env...)
	}
	// gone asserts the script left no temporary file behind, whatever it decided.
	gone := func(t *testing.T, tmp string) {
		t.Helper()
		if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
			t.Errorf("temporary file %s survived (%v), want it removed", tmp, err)
		}
	}
	dir := t.TempDir()

	// The bytes arrived intact — including ones no shell round-trip would
	// survive — and the parent directory is created on the way.
	t.Run("FullDelivery", func(t *testing.T) {
		payload := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		path := dir + "/deep/nested/blob.bin"
		code, tmp := run(t, payload, len(payload), path)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("file = %v, %v; want %v", got, err, payload)
		}
		// The rename consumed it; nothing hidden is left beside the file.
		gone(t, tmp)
	})

	// The #103 signature: the stdin stream delivered nothing, the redirection
	// truncated the file anyway, and `cat` exited 0. Without the length check
	// this is indistinguishable from a successful write.
	t.Run("NothingDelivered", func(t *testing.T) {
		code, tmp := run(t, nil, 4, dir+"/lost")
		if code != writeShort {
			t.Errorf("exit %d, want %d (short write)", code, writeShort)
		}
		gone(t, tmp)
	})

	// A stream that lost only its tail must not read as success either — and what
	// did arrive must not be at the target. This is the atomicity guarantee at its
	// narrowest: a write that failed leaves the file it was replacing untouched,
	// where writing straight to the target truncated it before the loss was even
	// visible (#71).
	t.Run("PartialDelivery", func(t *testing.T) {
		path := dir + "/partial"
		if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
			t.Fatalf("seed the target: %v", err)
		}
		code, tmp := run(t, []byte("kept"), 100, path)
		if code != writeShort {
			t.Errorf("exit %d, want %d (short write)", code, writeShort)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "original" {
			t.Errorf("target = %q, %v; want %q untouched", got, err, "original")
		}
		gone(t, tmp)
	})

	// Writing no bytes is a legitimate write of an empty file, not a loss.
	t.Run("EmptyWriteIsNotShort", func(t *testing.T) {
		path := dir + "/empty"
		code, tmp := run(t, nil, 0, path)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stat empty file: %v", err)
		}
		gone(t, tmp)
	})

	// A target that is a directory is refused by name, because the rename would
	// otherwise move the file *into* it, and the docker daemon's extraction would
	// delete it outright. The directory and its contents survive (#71).
	t.Run("TargetIsADirectory", func(t *testing.T) {
		held := dir + "/adir/inside"
		if err := os.MkdirAll(gopath.Dir(held), 0o755); err != nil {
			t.Fatalf("stage a directory: %v", err)
		}
		if err := os.WriteFile(held, []byte("kept"), 0o644); err != nil {
			t.Fatalf("stage a file inside it: %v", err)
		}
		code, tmp := run(t, []byte("x"), 1, gopath.Dir(held))
		if code != sandbox.ExitPathIsDirectory {
			t.Errorf("exit %d writing onto a directory, want %d", code, sandbox.ExitPathIsDirectory)
		}
		if got, err := os.ReadFile(held); err != nil || string(got) != "kept" {
			t.Errorf("file inside the directory = %q, %v; want it untouched", got, err)
		}
		gone(t, tmp)
	})

	// The target is asked about twice, and this is the second answer's row. The race
	// it defends against cannot be interleaved from a test — something in the
	// sandbox would have to make the target a directory in the instant between the
	// first check and the move — so what a racing `mv` *does* is staged instead:
	// this one finds the destination a directory and puts the file inside it,
	// exiting 0, exactly as the real one would. The write must not report that as a
	// success, and must not leave the file it never asked to put there.
	t.Run("TargetBecameADirectoryDuringTheMove", func(t *testing.T) {
		realMV, err := exec.LookPath("mv")
		if err != nil {
			t.Fatalf("find the real mv: %v", err)
		}
		bin := t.TempDir()
		shim := "#!/bin/sh\nmkdir -p \"$3\" && exec " + realMV + " \"$2\" \"$3\"/\n"
		if err := os.WriteFile(bin+"/mv", []byte(shim), 0o755); err != nil {
			t.Fatalf("stage the racing mv: %v", err)
		}
		path := dir + "/raced"
		code, tmp := run(t, []byte("x"), 1, path, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if code != sandbox.ExitPathIsDirectory {
			t.Errorf("exit %d, want %d — the move landed inside a directory", code, sandbox.ExitPathIsDirectory)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatalf("read the directory the move landed in: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("the directory holds %d entries, want the file the move put there removed", len(entries))
		}
		gone(t, tmp)
	})

	// The rename replaces the name, so the mode the target had goes with it unless
	// the script puts it back on the temporary file first — `write` a script,
	// `chmod +x` it, `edit` it, and it no longer runs (#204). This is the shell
	// half of that; holding both backends to the same answer is the contract
	// suite's job.
	t.Run("ModeOfTheTargetSurvives", func(t *testing.T) {
		path := dir + "/run.sh"
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatalf("stage an executable target: %v", err)
		}
		// Chmod'd rather than left to the create mode, which the host's umask
		// would take the bits out of on a machine that runs with a tight one.
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatalf("make the target executable: %v", err)
		}
		code, tmp := run(t, []byte("new"), 3, path, gnuStatEnv(t)...)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the rewritten target: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("mode = %o after the rewrite, want 755", got)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
			t.Errorf("target = %q, %v; want the bytes just written", got, err)
		}
		gone(t, tmp)
	})

	// The other half of that answer, and the one the image gets a vote in: a file
	// the write *creates* has no mode to carry over, so it lands whatever created
	// it: under the exec process's umask, which is the image's, 0644 on the usual
	// 022 and 0600 on a hardened 077, where the docker backend's tar header says
	// 0644 whatever the image thinks (#212). The script sets the umask itself so the
	// two backends answer this the same way, and the answer is the sandbox's rather
	// than the image's. (The umask is no longer the only thing holding it: the script
	// chmods the file it creates to 0644 as well, because a default POSIX ACL decides
	// those bits over a umask — #213, the row after next.) Staged at 077 as the
	// hardened case; every umask whose write bits differ from 022's moves, in both
	// directions — a group-oriented 007 landed 0660 and now lands 0644, dropping
	// group-write and adding other-read. (The execute bits are inert: a create asks
	// for 0666.)
	t.Run("AFreshFileIgnoresTheImagesUmask", func(t *testing.T) {
		fresh := dir + "/hardened/created.txt"
		code, tmp := runScript(t, "umask 077\n", []byte("new"), 3, fresh, gnuStatEnv(t)...)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		info, err := os.Stat(fresh)
		if err != nil {
			t.Fatalf("stat the created file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("a file the write created has mode %o under a 077 umask, want 644", got)
		}
		// The directories on the way there are the other side of that answer, and
		// they keep the image's umask: the `umask` sits after the `mkdir -p`, so a
		// hardened image still gets its 0700 parents — which is what docker's own
		// in-container `mkdir -p` gives too. Asserted because the file mode alone
		// would not notice the line being tidied up to the top of the script, and
		// then a hardened image would silently lose that. Measured: with the umask
		// set first, this directory is 0755.
		if info, err = os.Stat(gopath.Dir(fresh)); err != nil {
			t.Fatalf("stat the directory the write created: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("the directory the write created has mode %o under a 077 umask, want 700", got)
		}
		gone(t, tmp)

		// And what the script fixes is a floor for fresh files only: a target that
		// *does* have bits worth carrying still gets them back. What this half
		// catches is the script's own `chmod 0644` (#213) drifting *below*
		// __map_preserve_mode, where it would overwrite the mode the preservation
		// had just put back and land 644 here; ModeOfTheTargetSurvives and the live
		// contract row catch that too, so this is the fast local signal rather than
		// the only one. (It does not pin the umask's own position: moved below the
		// preservation, the umask would leave this rewrite at 600 all the same, and
		// the fresh-file assertion above is what fails.)
		if err := os.Chmod(fresh, 0o600); err != nil {
			t.Fatalf("give the target a mode worth carrying: %v", err)
		}
		code, tmp = runScript(t, "umask 077\n", []byte("two"), 3, fresh, gnuStatEnv(t)...)
		if code != 0 {
			t.Fatalf("exit %d rewriting it, want 0", code)
		}
		if info, err = os.Stat(fresh); err != nil {
			t.Fatalf("stat the rewritten target: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("after the rewrite the mode is %o, want the target's own 600", got)
		}
		gone(t, tmp)
	})

	// A umask is not the only thing that decides a created file's bits, so the
	// script sets them outright rather than only lowering what a create asks for
	// (#213). This row stages the *condition* — the temporary file already holding
	// a mode the platform did not choose when the bytes reach it — and it stages it
	// with a lever rather than the real cause, because the real cause is a default
	// POSIX ACL and a macOS dev host has none to stage. Nothing in the real path
	// pre-creates that file; its name is random per write. What it pins is the
	// property the ACL case rests on: the mode is *set*, not inherited from
	// whatever the file happened to be created as. The row below runs the real
	// cause wherever the host can.
	t.Run("AFreshFilesModeIsSetNotInherited", func(t *testing.T) {
		// $4 is the temporary file the script is about to write through, so the
		// prologue leaves it already there and already 0600 — which is what a
		// default ACL leaves behind for the script to find, by another route.
		const staged = `: > "$4"
chmod 600 "$4"
`
		fresh := dir + "/set-not-inherited.txt"
		record := dir + "/tee-saw-mode"
		code, tmp := runScript(t, staged, []byte("new"), 3, fresh, teeRecordingEnv(t, record)...)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		info, err := os.Stat(fresh)
		if err != nil {
			t.Fatalf("stat the created file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("a file the write created has mode %o, want 644 — set, not inherited", got)
		}
		// And it was set *before* the bytes reached it: the planted `tee` recorded
		// what the file it was about to fill already held. The landed mode alone
		// answers the same whether the chmod runs before the stream or after it, so
		// without this the `: >` and the chmod's position could both drift with
		// every mode assertion still green — and a large write would hold its bytes
		// at whatever the ACL chose for the length of the transfer.
		saw, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the planted tee recorded nothing: %v", err)
		}
		if got := strings.TrimSpace(string(saw)); got != "644" {
			t.Errorf("tee found the temporary file at mode %q, want 644 before the bytes stream", got)
		}
		gone(t, tmp)
	})

	// The real cause, wherever the host can stage it: a parent directory carrying a
	// **default POSIX ACL** supplies a created file's bits and the kernel ignores
	// the umask, so before #213 a write into one landed what the ACL said here
	// while the docker backend's tar header still said 0644. Skipped rather than
	// faked where there is no `setfacl` — macOS has no POSIX ACLs at all — and run
	// for real on Linux, including CI's ubuntu image, which ships `acl`.
	t.Run("ADefaultACLDoesNotDecideAFreshFilesMode", func(t *testing.T) {
		if _, err := exec.LookPath("setfacl"); err != nil {
			t.Skip("no setfacl on this host, so a default POSIX ACL cannot be staged")
		}
		aclDir := dir + "/defaultacl"
		if err := os.Mkdir(aclDir, 0o755); err != nil {
			t.Fatalf("stage the directory: %v", err)
		}
		// Past the skip, every failure is fatal rather than another way to skip. A
		// `setfacl` on the PATH means the acl package is installed, which on Linux
		// all but means the filesystem carries them; a staging step that then fails
		// is a broken environment or a broken row, and the repo's standing on that
		// is the one `make test` takes for a missing Docker daemon — a hard failure,
		// not a skip. Silently skipping here would lose the only real-ACL coverage
		// in CI without anything saying so.
		if out, err := exec.Command("setfacl", "-d", "-m", "u::rw-,g::rw-,o::rw-", aclDir).CombinedOutput(); err != nil {
			t.Fatalf("stage a default POSIX ACL on %s: %v: %s", aclDir, err, out)
		}
		// Prove the staging bites before trusting what the script lands: under the
		// same 022 umask the script sets, a file created here by anything else comes
		// out 0666 rather than 0644. Without this the row would pass unchanged on a
		// filesystem that accepted the setfacl and then ignored it, and prove
		// nothing at all. Fatal for the same reason the staging above is.
		control := aclDir + "/control"
		if err := exec.Command("/bin/bash", "-c", `umask 022; : > "$1"`, "map-acl-control", control).Run(); err != nil {
			t.Fatalf("stage the control file: %v", err)
		}
		info, err := os.Stat(control)
		if err != nil {
			t.Fatalf("stat the control file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o666 {
			t.Fatalf("the default ACL did not take: a file created under it is %o, want 666", got)
		}

		fresh := aclDir + "/created.txt"
		code, tmp := runScript(t, "", []byte("new"), 3, fresh, gnuStatEnv(t)...)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if info, err = os.Stat(fresh); err != nil {
			t.Fatalf("stat the created file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("a file created under a default POSIX ACL has mode %o, want 644", got)
		}
		gone(t, tmp)
	})

	// `stat` comes off the agent's own PATH, so it chooses what mode the write
	// applies — which is why the value has to be octal digits before it reaches
	// `chmod`. Without that check the planted value need not be a mode at all:
	// a symbolic one (`a+rwx` here) or an option (`--reference=` some setuid
	// binary) is accepted by `chmod` just as happily, and the write lands bits no
	// file involved ever had. Planted here because the guard is otherwise
	// invisible to the suite — removing it leaves every other row green (#204).
	t.Run("ANonOctalModeIsRefused", func(t *testing.T) {
		path := dir + "/planted.sh"
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatalf("stage the target: %v", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatalf("give the target a mode worth carrying: %v", err)
		}
		bin := t.TempDir()
		shim := "#!/bin/sh\necho a+rwx\n"
		if err := os.WriteFile(bin+"/stat", []byte(shim), 0o755); err != nil {
			t.Fatalf("plant the stat: %v", err)
		}
		code, tmp := run(t, []byte("new"), 3, path,
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if code != 0 {
			t.Fatalf("exit %d, want 0 — a planted stat costs the mode, never the write", code)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the written target: %v", err)
		}
		if got := info.Mode().Perm(); got&0o777 == 0o777 {
			t.Errorf("mode = %o, want the planted a+rwx refused rather than applied", got)
		}
		gone(t, tmp)
	})

	// A path blocked by a non-directory is the caller's to fix, and `mkdir -p` is
	// where that shows up: the shared shell names it so the model gets a tool error
	// rather than the executor getting a sandbox fault (#71).
	t.Run("PathBlockedByAFile", func(t *testing.T) {
		if err := os.WriteFile(dir+"/plain", []byte("i am a file"), 0o644); err != nil {
			t.Fatalf("stage a regular file: %v", err)
		}
		for _, path := range []string{dir + "/plain/child", dir + "/plain/deeper/child"} {
			if code, _ := run(t, []byte("x"), 1, path); code != sandbox.ExitPathNotDirectory {
				t.Errorf("exit %d for %s, want %d", code, path, sandbox.ExitPathNotDirectory)
			}
		}
	})

	// A write that cannot land for a reason of the sandbox's own keeps its own
	// failure code — the length check must not swallow it and report a short write,
	// and the path checks must not claim it as theirs.
	t.Run("UnwritableDirectoryIsNotShort", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the write bit, so this proves nothing")
		}
		locked := dir + "/locked"
		if err := os.Mkdir(locked, 0o500); err != nil {
			t.Fatalf("stage a read-only directory: %v", err)
		}
		code, _ := run(t, []byte("x"), 1, locked+"/denied")
		if code == 0 || code == writeShort ||
			code == sandbox.ExitPathNotDirectory || code == sandbox.ExitPathIsDirectory {
			t.Errorf("exit %d writing into a read-only directory, want a plain failure", code)
		}
	})

	// `tee` needs only write permission on the directory: the bytes go to a fresh
	// temporary file and are renamed over the target, so a target the sandbox user
	// cannot read is still replaceable — and re-reading it to count what landed
	// would need a read permission it may not have.
	t.Run("WriteOnlyFile", func(t *testing.T) {
		path := dir + "/writeonly"
		if err := os.WriteFile(path, nil, 0o200); err != nil {
			t.Fatalf("stage write-only file: %v", err)
		}
		if os.Geteuid() == 0 {
			t.Skip("root ignores the read bit, so this proves nothing")
		}
		code, tmp := run(t, []byte("kept"), 4, path)
		if code != 0 {
			t.Errorf("exit %d writing a write-only file, want 0", code)
		}
		gone(t, tmp)
	})
}

// gnuStatEnv supplies a `stat -c` shim when — and only when — the host's own stat
// rejects `-c`, which BSD stat does. readScript reaches its size gate before
// anything under test here, so on a macOS dev host the script would die there and
// the tests below would cover nothing; on Linux and in CI the real binary the
// image contract names still runs. It returns the environment to hand the script,
// nil meaning "inherit".
func gnuStatEnv(t *testing.T) []string {
	t.Helper()
	if exec.Command("stat", "-c", "%s", os.DevNull).Run() == nil {
		return nil
	}
	bin := t.TempDir()
	// The two formats the scripts ask for: `%s` is readScript's size gate, `%a`
	// the shared preserve-mode shell's. BSD `%Lp` is the permission bits alone —
	// it drops the setuid/setgid/sticky bits GNU `%a` would show, which no row
	// here stages.
	shim := "#!/bin/sh\ncase \"$2\" in\n  %s) exec /usr/bin/stat -f %z \"$3\" ;;\n" +
		"  %a) exec /usr/bin/stat -f %Lp \"$3\" ;;\nesac\nexit 1\n"
	if err := os.WriteFile(bin+"/stat", []byte(shim), 0o755); err != nil {
		t.Fatalf("stage stat shim: %v", err)
	}
	return append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// teeRecordingEnv plants a `tee` on the PATH the write script will use, which
// records the mode of the file it is about to fill before handing off to the real
// one. What a write *lands* answers the same whether the script's `chmod 0644`
// runs before the stream or after it, so the reason the file is created empty and
// chmod'd first — that it never holds the bytes at a mode the platform did not
// choose — is otherwise invisible to the suite, exactly as __map_preserve_mode's
// octal check is without a planted `stat` (#213).
//
// It layers on gnuStatEnv's environment rather than replacing it, and its own
// directory carries only `tee`, so the `stat` the shim calls is still whichever
// one speaks `-c` on this host.
func teeRecordingEnv(t *testing.T, record string) []string {
	t.Helper()
	base := gnuStatEnv(t)
	if base == nil {
		base = os.Environ()
	}
	bin := t.TempDir()
	shim := "#!/bin/sh\nstat -c %a \"$1\" > " + record + " 2>/dev/null\nexec /usr/bin/tee \"$@\"\n"
	if err := os.WriteFile(bin+"/tee", []byte(shim), 0o755); err != nil {
		t.Fatalf("stage the tee shim: %v", err)
	}
	env := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(kv, "PATH=") {
			kv = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
		env = append(env, kv)
	}
	return env
}

// readScript's marker is what makes a short exec stdout stream visible, so its
// contract is pinned here rather than left to the live cluster: no cluster can be
// told to lose bytes, but everything else — that the marker goes out on success
// and only on success, and that the classification exits still fire ahead of it —
// is observable from the host's shell, on any machine, in milliseconds.
//
// This runs the script through the host's /bin/bash rather than the sandbox
// image, as the write-side test does. It pins what the script does with its
// arguments; that the image carries a userland able to run it is the live
// contract test's job.
func TestReadScriptMarksWhatItSent(t *testing.T) {
	const marker = "0123456789abcdef"
	env := gnuStatEnv(t)
	dir := t.TempDir()
	run := func(t *testing.T, path string, cap int) (int, []byte) {
		t.Helper()
		var out bytes.Buffer
		cmd := exec.Command("/bin/bash", "-c", readScript, "map-read", path, strconv.Itoa(cap), marker)
		cmd.Env, cmd.Stdout = env, &out
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run readScript: %v", err)
			}
			return ee.ExitCode(), out.Bytes()
		}
		return 0, out.Bytes()
	}
	stage := func(t *testing.T, name string, b []byte) string {
		t.Helper()
		p := dir + "/" + name
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		return p
	}

	// The bytes come back intact — including ones no shell round-trip would
	// survive — with the marker behind them.
	t.Run("FullDelivery", func(t *testing.T) {
		payload := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		code, out := run(t, stage(t, "blob.bin", payload), sandbox.MaxFileBytes)
		if want := append(append([]byte{}, payload...), marker...); code != 0 || !bytes.Equal(out, want) {
			t.Errorf("exit %d, stdout %v; want 0 and %v", code, out, want)
		}
	})

	// A payload spanning many stream buffers, which a handful of bytes does not
	// reach.
	t.Run("LargePayload", func(t *testing.T) {
		payload := make([]byte, 1<<20)
		for i := range payload {
			payload[i] = byte(i)
		}
		code, out := run(t, stage(t, "large.bin", payload), sandbox.MaxFileBytes)
		if want := append(append([]byte{}, payload...), marker...); code != 0 || !bytes.Equal(out, want) {
			t.Errorf("exit %d, %d bytes; want 0 and %d matching", code, len(out), len(want))
		}
	})

	// Reading no bytes is a legitimate read of an empty file, so it is marked like
	// any other — that is what keeps it distinguishable from a stream that
	// delivered nothing at all.
	t.Run("EmptyFileIsMarked", func(t *testing.T) {
		code, out := run(t, stage(t, "empty", nil), sandbox.MaxFileBytes)
		if code != 0 || string(out) != marker {
			t.Errorf("exit %d, stdout %q; want 0 and the marker alone", code, out)
		}
	})

	// A read that cannot happen keeps its own failure code and emits no marker, so
	// an unreadable file cannot arrive as a successful read of fewer bytes.
	t.Run("UnreadableFileIsNotMarked", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the read bit, so this proves nothing")
		}
		p := stage(t, "noperm", []byte("secret"))
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		if code, out := run(t, p, sandbox.MaxFileBytes); code == 0 || len(out) != 0 {
			t.Errorf("exit %d, %d bytes; want a non-zero exit and no output", code, len(out))
		}
	})

	// The classification gates still run ahead of the cat, so none of them can
	// arrive as a marked read of zero bytes.
	t.Run("ClassifiesBeforeCatting", func(t *testing.T) {
		gate := stage(t, "gate.bin", []byte("seven!!"))
		if err := os.Symlink(gate, dir+"/link"); err != nil {
			t.Fatalf("stage symlink: %v", err)
		}
		if err := os.Mkdir(dir+"/sub", 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := exec.Command("mkfifo", dir+"/fifo").Run(); err != nil {
			t.Fatalf("stage fifo: %v", err)
		}
		// A directory and a regular file whose name is that directory's plus a
		// newline. Asking the shared shell about the *file*'s child must not be
		// answered for the *directory* — which is what happens the moment the path
		// travels through a command substitution on its way there.
		if err := os.Mkdir(dir+"/nl", 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(dir+"/nl\n", nil, 0o600); err != nil {
			t.Fatalf("stage its newline-suffixed sibling: %v", err)
		}
		for _, c := range []struct {
			name string
			path string
			cap  int
			want int
		}{
			{"Missing", dir + "/nope", sandbox.MaxFileBytes, readNotExist},
			// Missing for a reason the model can act on: a file is in the way. It
			// must not read as a plain absence, which would invite a mkdir that
			// cannot work either (#71).
			{"BlockedByAFile", gate + "/child", sandbox.MaxFileBytes, sandbox.ExitPathNotDirectory},
			{"DeeperBlockedByAFile", gate + "/x/y", sandbox.MaxFileBytes, sandbox.ExitPathNotDirectory},
			{"BlockedByAFileNamedWithANewline", dir + "/nl\n/child", sandbox.MaxFileBytes, sandbox.ExitPathNotDirectory},
			{"Directory", dir + "/sub", sandbox.MaxFileBytes, readIsDir},
			{"Symlink", dir + "/link", sandbox.MaxFileBytes, readNotRegular},
			{"Fifo", dir + "/fifo", sandbox.MaxFileBytes, readNotRegular},
			{"OverTheCap", gate, 2, readTooLarge},
		} {
			t.Run(c.name, func(t *testing.T) {
				if code, out := run(t, c.path, c.cap); code != c.want || len(out) != 0 {
					t.Errorf("exit %d, %d bytes; want %d and no output", code, len(out), c.want)
				}
			})
		}
	})
}

// readStdout is where a short read is caught, and no cluster can stage one — a
// stream cannot be told to lose bytes. Its branches are pinned against streams
// fed byte-for-byte into the buffer ReadFile actually uses, so the cap arithmetic
// is exercised rather than asserted: the marker rides in the same buffer as the
// content, which puts the largest legal file and the first oversize one one
// marker-length apart.
func TestReadStdoutRequiresTheMarker(t *testing.T) {
	const marker = "0123456789abcdef"
	// The buffer ReadFile hands the exec, filled the way the stream fills it.
	recv := func(chunks ...[]byte) *cappedBuffer {
		out := &cappedBuffer{limit: sandbox.MaxFileBytes + len(marker)}
		for _, c := range chunks {
			for len(c) > 0 {
				n := min(len(c), 32768)
				_, _ = out.Write(c[:n])
				c = c[n:]
			}
		}
		return out
	}
	mark := []byte(marker)
	body := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }

	// Bytes then marker is a complete read, and only the file's bytes come back.
	t.Run("WholeFile", func(t *testing.T) {
		want := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		got, err := readStdout("/w/f", marker, recv(want, mark))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("readStdout = %v, %v; want %v", got, err, want)
		}
	})

	// An empty file is a file, which is why the marker goes out unconditionally on
	// success: an empty read is not evidence of a lost stream.
	t.Run("EmptyFileIsNotShort", func(t *testing.T) {
		got, err := readStdout("/w/f", marker, recv(mark))
		if err != nil || len(got) != 0 {
			t.Errorf("readStdout = %q, %v; want an empty read", got, err)
		}
	})

	// Only the tail is stripped, so a file whose own bytes contain the marker
	// still round-trips whole.
	t.Run("ContentContainingTheMarker", func(t *testing.T) {
		want := append(append([]byte{}, mark...), body(10)...)
		got, err := readStdout("/w/f", marker, recv(want, mark))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("readStdout = %d bytes, %v; want %d", len(got), err, len(want))
		}
	})

	// The #105 signature: the exec exited 0 and stdout stopped early. Each of
	// these is, without the marker, indistinguishable from a shorter file.
	t.Run("NothingArrived", func(t *testing.T) {
		if got, err := readStdout("/w/f", marker, recv()); err == nil {
			t.Errorf("an empty stream read back as %d bytes", len(got))
		}
	})
	t.Run("TailLost", func(t *testing.T) {
		got, err := readStdout("/w/f", marker, recv(body(100)))
		if err == nil {
			t.Fatalf("a stream that lost its tail read back as %d bytes", len(got))
		}
		if errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Errorf("err = %v, want a short read rather than a size fault", err)
		}
	})
	t.Run("MarkerCutInHalf", func(t *testing.T) {
		if _, err := readStdout("/w/f", marker, recv(body(100), mark[:len(mark)/2])); err == nil {
			t.Error("a half-delivered marker read back as a whole file")
		}
	})

	// A file at exactly the cap is the largest legal read, and the buffer carries
	// the marker on top of the cap so it still fits. Sizing the buffer as if the
	// marker came out of the file's budget fails here — the case that would make
	// the guard worse than the hazard.
	t.Run("AtTheCapIsNotTooLarge", func(t *testing.T) {
		got, err := readStdout("/w/f", marker, recv(body(sandbox.MaxFileBytes), mark))
		if err != nil || len(got) != sandbox.MaxFileBytes {
			t.Errorf("readStdout = %d bytes, %v; want %d and no error", len(got), err, sandbox.MaxFileBytes)
		}
	})

	// One byte past it is a size fault, not a short read: the file grew after
	// readScript's gate, and the cap dropped the marker along with the excess, so
	// only the order of the two checks decides which answer the caller gets.
	t.Run("PastTheCapIsTooLarge", func(t *testing.T) {
		if _, err := readStdout("/w/f", marker, recv(body(sandbox.MaxFileBytes+1), mark)); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Errorf("err = %v, want ErrFileTooLarge", err)
		}
	})

	// The returned slice must not lend its spare capacity back over the marker.
	t.Run("ReturnedSliceIsClipped", func(t *testing.T) {
		got, err := readStdout("/w/f", marker, recv(body(4), mark))
		if err != nil {
			t.Fatalf("readStdout: %v", err)
		}
		if cap(got) != len(got) {
			t.Errorf("cap %d, len %d: appending would write over the marker", cap(got), len(got))
		}
	})
}

// classifyTimeout is where #95 and #110 were lost: the watchdog's SIGKILL landed
// (exit 137) and the call still reported TimedOut false, because the only
// evidence that the kill was the deadline's came from a probe that had raced an
// apiserver round trip and lost. Pinning the decision here costs no clock and no
// cluster, which is the point — on a live cluster the losing case is exactly the
// one that cannot be staged on demand.
func TestClassifyTimeout(t *testing.T) {
	const other = 7
	cases := []struct {
		name          string
		undeadlined   bool
		code          int
		watchdogFired bool
		v             verdict
		want          bool
	}{
		// A command given no deadline has no watchdog to mark it, so a mark it is
		// found wearing is one it planted — and an untimed command must not be
		// able to call itself timed out by planting one and exiting 137.
		{name: "NoDeadlineIgnoresAPlantedMark", undeadlined: true, code: sigkillExit, watchdogFired: true, want: false},
		// The regression. The watchdog says it fired and the exit code agrees a
		// SIGKILL landed; no probe needs to have caught the command alive.
		{name: "WatchdogFiredAndProbeMissedIt", code: sigkillExit, watchdogFired: true, want: true},
		{name: "WatchdogFiredAndProbeSawIt", code: sigkillExit, watchdogFired: true, v: verdict{aliveAtDeadline: true}, want: true},

		// A SIGKILL the watchdog did not deliver is still the deadline's if the
		// command was alive to receive it — the tenant can kill the watchdog, and
		// the node can do the killing, so the probe keeps earning its place.
		{name: "ProbeAloneStillCounts", code: sigkillExit, v: verdict{aliveAtDeadline: true}, want: true},

		// Overrunning is a timeout on its own authority: a command still running
		// past the deadline and the slop can report no exit code worth believing.
		{name: "OverranWithoutASigkill", code: other, v: verdict{overran: true}, want: true},
		{name: "OverranWithNoEvidenceAtAll", v: verdict{overran: true}, want: true},

		// The self-inflicted kill the contract suite pins: exit 137, but the
		// watchdog never fired and the command was already gone when Exec looked.
		{name: "SelfInflictedKillIsNotATimeout", code: sigkillExit},

		// A mark without a SIGKILL is not a timeout. This is the window between
		// the watchdog's last `kill -0` and its `kill -9`, where the command exits
		// on its own terms and the mark is already written — and it is also what
		// keeps a forged mark from manufacturing a timeout out of a clean exit.
		{name: "MarkWithoutASigkill", code: other, watchdogFired: true},
		{name: "MarkWithACleanExit", watchdogFired: true},

		// An honest command that finished inside its deadline.
		{name: "CleanExit"},
		{name: "CleanExitSeenAliveJustBefore", v: verdict{aliveAtDeadline: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTimeout(!c.undeadlined, c.code, c.watchdogFired, c.v); got != c.want {
				t.Errorf("classifyTimeout(%v, %d, %v, %+v) = %v, want %v",
					!c.undeadlined, c.code, c.watchdogFired, c.v, got, c.want)
			}
		})
	}
}

// parseExit is the other half of the same decision, and the half that has to
// stay compatible: the wrapper's mark rides on the exit line, so "the wrapper
// recorded nothing" must still be the one and only empty case.
func TestParseExitReadsTheWatchdogsMark(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		code   int
		killed bool
		fails  bool
	}{
		{name: "KilledByTheWatchdog", out: "K 137\n", code: sigkillExit, killed: true},
		{name: "FinishedOnItsOwn", out: " 0\n", code: 0},
		{name: "NonZeroExit", out: " 7\n", code: 7},
		// The $PPID sabotage: the wrapper never recorded a code. It reads as the
		// kill's — and the watchdog's mark, left independently of the wrapper,
		// still says the deadline was what caused it.
		{name: "NothingRecorded", out: " \n", code: sigkillExit},
		{name: "Empty", out: "", code: sigkillExit},
		{name: "SabotagedWrapperButMarked", out: "K \n", code: sigkillExit, killed: true},
		// The mark leads, so a stream that loses its tail loses the code and keeps
		// the timeout, never the other way round.
		{name: "GarbageCode", out: "K not-a-code\n", fails: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, killed, err := parseExit(c.out)
			if c.fails {
				if err == nil {
					t.Fatalf("parseExit(%q) = %d, %v, nil; want an error", c.out, code, killed)
				}
				return
			}
			if err != nil || code != c.code || killed != c.killed {
				t.Errorf("parseExit(%q) = %d, %v, %v; want %d, %v, nil",
					c.out, code, killed, err, c.code, c.killed)
			}
		})
	}
}

// setsidEnv supplies a `setsid` shim when — and only when — the host has none,
// which macOS does not. execWrapper backgrounds the command through it, and the
// watchdog's group kill only reaches the command's children because of it, so a
// shim that merely `exec`s would prove nothing about the kill: this one creates
// the session for real. On Linux and in CI the util-linux binary the image
// contract names still runs. It returns the environment to hand the script, nil
// meaning "inherit".
func setsidEnv(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err == nil {
		return nil
	}
	perl, err := exec.LookPath("perl")
	if err != nil {
		// Not a skip: skipping would delete the whole #95 regression suite on a
		// host that happens to lack both, and take the tamper cases with it.
		t.Fatalf("host has neither setsid nor perl to stand in for it: %v", err)
	}
	bin := t.TempDir()
	shim := "#!/bin/sh\nexec " + perl +
		" -e 'use POSIX qw(setsid); setsid() != -1 or die \"setsid: $!\"; exec @ARGV or die \"exec: $!\"' \"$@\"\n"
	if err := os.WriteFile(bin+"/setsid", []byte(shim), 0o755); err != nil {
		t.Fatalf("stage setsid shim: %v", err)
	}
	return append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The watchdog is the only thing in this backend that knows whether a SIGKILL
// was the deadline's, so what it records is the fix for #95/#110 and is pinned
// here rather than left to the live cluster: no cluster can be told to answer a
// liveness probe late, but the mark itself is observable from the host's shell.
//
// This runs the script through the host's /bin/bash rather than the sandbox
// image, as the write- and read-side tests do. It pins what the script does with
// its arguments; that the image carries a userland able to run it is the live
// contract test's job.
func TestExecWrapperMarksTheWatchdogsKill(t *testing.T) {
	env := setsidEnv(t)
	dir := t.TempDir()
	// Both scripts run, the way the provider runs them: the wrapper records the
	// exec's state and exitScript is the one that reads the mark back out.
	run := func(t *testing.T, name, command string, seconds int, extraEnv ...string) string {
		t.Helper()
		state := dir + "/" + name
		wrapper := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", command, strconv.Itoa(seconds), state)
		base := env
		if base == nil {
			base = os.Environ()
		}
		wrapper.Env = append(append([]string{}, base...), extraEnv...)
		if err := wrapper.Run(); err != nil {
			t.Fatalf("run execWrapper: %v", err)
		}
		out, err := exec.Command("/bin/bash", "-c", exitScript, "map-exit", state).Output()
		if err != nil {
			t.Fatalf("run exitScript: %v", err)
		}
		return string(out)
	}

	// The #95 signature, from the other side: the command is killed on its
	// deadline, and the exit line says who did it. Read back through the same
	// parse and classification the provider uses, a punctual kill is a timeout
	// with no probe involved at all.
	t.Run("KilledOnItsDeadline", func(t *testing.T) {
		code, killed, err := parseExit(run(t, "killed", "sleep 30", 1))
		if err != nil || code != sigkillExit || !killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
		if !classifyTimeout(true, code, killed, verdict{}) {
			t.Error("a command the watchdog killed on its deadline did not classify as a timeout")
		}
	})

	// An honest command that finishes early leaves no mark, and its own exit code
	// stands. Its watchdog is still asleep when it exits, so this also pins that
	// the wrapper never waits for the watchdog to notice.
	t.Run("FinishedInsideItsDeadline", func(t *testing.T) {
		code, killed, err := parseExit(run(t, "clean", "exit 7", 5))
		if err != nil || code != 7 || killed {
			t.Fatalf("parseExit = %d, %v, %v; want 7, false, nil", code, killed, err)
		}
		if classifyTimeout(true, code, killed, verdict{}) {
			t.Error("a command that finished inside its deadline classified as a timeout")
		}
	})

	// A command that SIGKILLs itself exits 137 without the watchdog firing, so
	// the mark is what keeps 137 from meaning "timeout" on its own.
	t.Run("SelfInflictedKillLeavesNoMark", func(t *testing.T) {
		code, killed, err := parseExit(run(t, "selfkill", "kill -9 $$", 30))
		if err != nil || code != sigkillExit || killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, false, nil", code, killed, err, sigkillExit)
		}
		if classifyTimeout(true, code, killed, verdict{}) {
			t.Error("a self-inflicted SIGKILL classified as a timeout")
		}
	})

	// The mark must never be able to hold the kill back — the property that makes
	// it safe to write one at all. The path is the wrapper's own argv, so a
	// command can read it out of /proc and plant whatever it likes there before
	// the watchdog fires. Each of these makes the mark fail; each must still
	// leave the command dead on its deadline, with the classification falling
	// back to the probes, which is where it stood before the mark existed.
	//
	// The FIFO is the one that matters. `: > "$3.killed"` — the obvious way to
	// write a mark — blocks forever opening a FIFO for writing, so the watchdog
	// would never reach `kill -9` and the runaway would outlive its deadline
	// entirely. `mkdir` cannot block on any of these.
	//
	// What each plant does to the *label* differs, and is asserted rather than
	// waved at: a planted directory is indistinguishable from the watchdog's own
	// mark, so it forges a timeout — the tenant mislabelling its own tool call,
	// the one direction this trade is allowed to fail in. A file or a FIFO is not
	// a directory, so those suppress the mark instead.
	blocked := []struct {
		name       string
		plant      func(t *testing.T, path string) error
		wantMarked bool
	}{
		{"Fifo", func(t *testing.T, path string) error { return syscall.Mkfifo(path, 0o644) }, false},
		{"SymlinkToFifo", func(t *testing.T, path string) error {
			if err := syscall.Mkfifo(path+".target", 0o644); err != nil {
				return err
			}
			return os.Symlink(path+".target", path)
		}, false},
		{"RegularFile", func(t *testing.T, path string) error {
			return os.WriteFile(path, []byte("not mine"), 0o644)
		}, false},
		{"Directory", func(t *testing.T, path string) error { return os.Mkdir(path, 0o755) }, true},
	}
	for _, b := range blocked {
		t.Run("MarkBlockedBy"+b.name+"StillKills", func(t *testing.T) {
			state := "blocked" + b.name
			if err := b.plant(t, dir+"/"+state+".killed"); err != nil {
				t.Fatalf("plant %s at the mark: %v", b.name, err)
			}
			code, killed, err := parseExit(run(t, state, "sleep 30", 1))
			if err != nil || code != sigkillExit {
				t.Fatalf("parseExit = %d, %v, %v; want %d — the kill did not land",
					code, killed, err, sigkillExit)
			}
			if killed != b.wantMarked {
				t.Errorf("watchdogFired = %v, want %v", killed, b.wantMarked)
			}
		})
	}

	// bash aborts on a redirection failure in a POSIX special builtin when it is
	// in POSIX mode, which the deployment's own image can turn on through the
	// environment. That is why the mark is not written with a redirect at all:
	// the kill has to land whatever mode the shell is in.
	t.Run("PosixModeStillKills", func(t *testing.T) {
		state := "posix"
		if err := syscall.Mkfifo(dir+"/"+state+".killed", 0o644); err != nil {
			t.Fatalf("plant a fifo at the mark: %v", err)
		}
		code, killed, err := parseExit(run(t, state, "sleep 30", 1, "POSIXLY_CORRECT=1"))
		if err != nil || code != sigkillExit {
			t.Fatalf("parseExit = %d, %v, %v; want %d — the kill did not land in POSIX mode",
				code, killed, err, sigkillExit)
		}
		if killed {
			t.Error("a mark the watchdog could not make was read as made")
		}
	})

	// The $PPID sabotage: the command kills the wrapper before it can record an
	// exit code. The watchdog is a separate process and still marks its kill, and
	// because the mark is read by exitScript rather than folded in by the wrapper,
	// the timeout survives the sabotage.
	t.Run("MarkSurvivesASabotagedWrapper", func(t *testing.T) {
		state := dir + "/sabotaged"
		if err := os.Mkdir(state+".killed", 0o755); err != nil {
			t.Fatalf("stage the watchdog's mark: %v", err)
		}
		out, err := exec.Command("/bin/bash", "-c", exitScript, "map-exit", state).Output()
		if err != nil {
			t.Fatalf("run exitScript: %v", err)
		}
		code, killed, err := parseExit(string(out))
		if err != nil || code != sigkillExit || !killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
		if !classifyTimeout(true, code, killed, verdict{}) {
			t.Error("a watchdog kill whose wrapper was sabotaged did not classify as a timeout")
		}
	})
}

// exitScript carries the mark home and takes the exec's state with it, so the
// two halves are pinned together: a mark the provider cannot read is a lost
// timeout, and one it does not remove is a file per timed-out command left in
// the pod for the session's life.
func TestExitScriptReportsAndClearsTheWatchdogsMark(t *testing.T) {
	dir := t.TempDir()
	run := func(t *testing.T, state string) string {
		t.Helper()
		cmd := exec.Command("/bin/bash", "-c", exitScript, "map-exit", state)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run exitScript: %v", err)
		}
		for _, suffix := range []string{".pid", ".exit", ".killed"} {
			if _, err := os.Stat(state + suffix); !os.IsNotExist(err) {
				t.Errorf("%s survived the read: stat err = %v", suffix, err)
			}
		}
		return string(out)
	}
	stage := func(t *testing.T, name, exitLine string, marked bool) string {
		t.Helper()
		state := dir + "/" + name
		if err := os.WriteFile(state+".pid", []byte("42\n"), 0o644); err != nil {
			t.Fatalf("stage .pid: %v", err)
		}
		if exitLine != "" {
			if err := os.WriteFile(state+".exit", []byte(exitLine), 0o644); err != nil {
				t.Fatalf("stage .exit: %v", err)
			}
		}
		if marked {
			if err := os.Mkdir(state+".killed", 0o755); err != nil {
				t.Fatalf("stage the watchdog's mark: %v", err)
			}
		}
		return state
	}

	t.Run("KilledByTheWatchdog", func(t *testing.T) {
		state := stage(t, "killed", "137\n", true)
		if code, killed, err := parseExit(run(t, state)); err != nil || code != sigkillExit || !killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
	})

	t.Run("FinishedOnItsOwn", func(t *testing.T) {
		state := stage(t, "clean", "0\n", false)
		if code, killed, err := parseExit(run(t, state)); err != nil || code != 0 || killed {
			t.Errorf("parseExit = %d, %v, %v; want 0, false, nil", code, killed, err)
		}
	})

	// The mark is a directory, and a tenant may have made the path something else
	// entirely. Either way the cleanup has to take it: `rm -f` alone would leave
	// one entry per timed-out command in the pod for the session's life.
	t.Run("MarkOfAnyShapeIsCleared", func(t *testing.T) {
		state := dir + "/planted"
		if err := os.WriteFile(state+".pid", []byte("42\n"), 0o644); err != nil {
			t.Fatalf("stage .pid: %v", err)
		}
		if err := syscall.Mkfifo(state+".killed", 0o644); err != nil {
			t.Fatalf("stage a fifo at the mark: %v", err)
		}
		if code, killed, err := parseExit(run(t, state)); err != nil || code != sigkillExit || killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, false, nil — a fifo is not the watchdog's mark",
				code, killed, err, sigkillExit)
		}
	})

	// The wrapper never recorded anything, and the cleanup still has to run.
	t.Run("NothingRecorded", func(t *testing.T) {
		state := stage(t, "sabotaged", "", false)
		if code, killed, err := parseExit(run(t, state)); err != nil || code != sigkillExit || killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, false, nil", code, killed, err, sigkillExit)
		}
	})

	// The same sabotage, but the watchdog did fire before the wrapper died. The
	// mark is the only witness left, and it is read here rather than by the
	// wrapper precisely so this case is not lost.
	t.Run("NothingRecordedButMarked", func(t *testing.T) {
		state := stage(t, "sabotaged-marked", "", true)
		if code, killed, err := parseExit(run(t, state)); err != nil || code != sigkillExit || !killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
	})
}

func TestDestroySurfacesError(t *testing.T) {
	sid := domain.ID("sesn_derr")
	cs := fake.NewClientset(readyPod(sid))
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver rejected the delete")
	})
	p := &Provider{client: &client{cs: cs, namespace: "default"}, netSetupImage: "busybox"}
	sb, err := p.Provision(context.Background(), sandbox.Spec{SessionID: sid, Image: "img"})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := sb.Destroy(context.Background()); err == nil {
		t.Error("destroy with a failing delete (not a NotFound): want an error")
	}
}

// mintRecorder records the mint-on-create seam's calls for the gated tests.
type mintRecorder struct {
	generated  int
	persisted  []string // tokens handed to Persist
	persistErr error
}

func (m *mintRecorder) Generate() string { m.generated++; return "gtk_unit_test_token" }

func (m *mintRecorder) Persist(_ context.Context, _ domain.ID, token string) error {
	if m.persistErr != nil {
		return m.persistErr
	}
	m.persisted = append(m.persisted, token)
	return nil
}

func gateSpecFixture(m *mintRecorder) *sandbox.GateSpec {
	return &sandbox.GateSpec{
		Image:           "map-gate:test",
		ControlplaneURL: "http://cp.test:8080",
		TokenMinter:     m,
		OTelEndpoint:    "otel.test:4317",
		OTelInsecure:    true,
	}
}

// markPodsReadyOnCreate makes every created pod immediately report both the
// sandbox container and the gate sidecar ready, so Provision's waitReady
// returns at once against the fake clientset.
func markPodsReadyOnCreate(p *Provider) {
	cs := p.client.cs.(*fake.Clientset)
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = types.UID("uid-" + pod.Name) // the fake clientset assigns none
		pod.Status = corev1.PodStatus{
			Phase:                 corev1.PodRunning,
			ContainerStatuses:     []corev1.ContainerStatus{{Name: containerName, Ready: true}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: gateContainerName, Ready: true}},
		}
		return false, nil, nil // fall through to the tracker with the status set
	})
}

// TestPodSpecGatedShape pins the gated pod: the gate native sidecar (restart
// Always, NET_ADMIN, exec healthcheck probes, the cmd/gate env contract), no
// route-flush init container, and the hardened sandbox with proxy env.
func TestPodSpecGatedShape(t *testing.T) {
	p := fakeProvider()
	spec := sandbox.Spec{
		SessionID:  domain.ID("sesn_gated"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"api.example.com"}},
		Env:        map[string]string{"API_KEY": "vltph_x"},
		Gate:       gateSpecFixture(&mintRecorder{}),
	}
	pod := p.podSpec("map-sesn-gated", "/workdir", spec, "gtk_unit_test_token")

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("gated pod has %d init containers, want exactly the gate sidecar", len(pod.Spec.InitContainers))
	}
	g := pod.Spec.InitContainers[0]
	if g.Name != gateContainerName || g.Image != "map-gate:test" {
		t.Errorf("gate sidecar = %s/%s, want %s/map-gate:test", g.Name, g.Image, gateContainerName)
	}
	if g.RestartPolicy == nil || *g.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("gate sidecar is not a native sidecar (restartPolicy Always)")
	}
	if g.SecurityContext == nil || g.SecurityContext.Capabilities == nil ||
		len(g.SecurityContext.Capabilities.Add) != 1 || g.SecurityContext.Capabilities.Add[0] != "NET_ADMIN" {
		t.Errorf("gate sidecar capabilities = %+v, want exactly NET_ADMIN added", g.SecurityContext)
	}
	for _, probe := range []*corev1.Probe{g.StartupProbe, g.ReadinessProbe} {
		if probe == nil || probe.Exec == nil || strings.Join(probe.Exec.Command, " ") != "/gate -healthcheck" {
			t.Errorf("gate probe = %+v, want exec /gate -healthcheck", probe)
		}
	}
	wantEnv := map[string]string{
		"CONTROLPLANE_URL":            "http://cp.test:8080",
		"GATE_TOKEN":                  "gtk_unit_test_token",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "otel.test:4317",
		"OTEL_EXPORTER_OTLP_INSECURE": "true",
	}
	gotEnv := map[string]string{}
	for _, e := range g.Env {
		gotEnv[e.Name] = e.Value
	}
	for k, v := range wantEnv {
		if gotEnv[k] != v {
			t.Errorf("gate env %s = %q, want %q", k, gotEnv[k], v)
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == "netsetup" {
			t.Error("gated pod still carries the route-flush init container (it would strand the gate)")
		}
	}

	sb := pod.Spec.Containers[0]
	if sb.SecurityContext == nil || sb.SecurityContext.AllowPrivilegeEscalation == nil || *sb.SecurityContext.AllowPrivilegeEscalation {
		t.Error("gated sandbox allows privilege escalation")
	}
	drops := map[corev1.Capability]bool{}
	if sb.SecurityContext != nil && sb.SecurityContext.Capabilities != nil {
		for _, c := range sb.SecurityContext.Capabilities.Drop {
			drops[c] = true
		}
	}
	for _, c := range []corev1.Capability{"NET_RAW", "SETUID", "SETGID"} {
		if !drops[c] {
			t.Errorf("gated sandbox does not drop %s", c)
		}
	}
	sbEnv := map[string]string{}
	for _, e := range sb.Env {
		sbEnv[e.Name] = e.Value
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if sbEnv[k] != "http://127.0.0.1:15080" {
			t.Errorf("sandbox %s = %q, want the gate loopback proxy", k, sbEnv[k])
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		if v, ok := sbEnv[k]; !ok || v != "" {
			t.Errorf("sandbox %s = %q,%v, want forced empty", k, v, ok)
		}
	}
	if sbEnv["API_KEY"] != "vltph_x" {
		t.Errorf("sandbox vault placeholder lost: API_KEY = %q", sbEnv["API_KEY"])
	}
}

// TestPodSpecUngatedUnchanged pins that a gate-less limited pod keeps the
// pre-gate shape: route-flush init container, no proxy env, no securityContext.
func TestPodSpecUngatedUnchanged(t *testing.T) {
	p := fakeProvider()
	spec := sandbox.Spec{
		SessionID:  domain.ID("sesn_plain"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetLimited},
	}
	pod := p.podSpec("map-sesn-plain", "/workdir", spec, "")
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "netsetup" {
		t.Fatalf("ungated limited pod init containers = %+v, want the netsetup flush", pod.Spec.InitContainers)
	}
	sb := pod.Spec.Containers[0]
	if sb.SecurityContext != nil {
		t.Errorf("ungated sandbox grew a securityContext: %+v", sb.SecurityContext)
	}
	for _, e := range sb.Env {
		if strings.Contains(strings.ToUpper(e.Name), "PROXY") {
			t.Errorf("ungated sandbox has proxy env %s", e.Name)
		}
	}
}

// TestProvisionGatedMintsOnlyOnCreate: the create path generates and persists
// exactly one token (the same one), and adopting an existing gated pod never
// touches the seam.
func TestProvisionGatedMintsOnlyOnCreate(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_mint")
	p := fakeProvider()
	markPodsReadyOnCreate(p)
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}
	if _, err := p.Provision(context.Background(), spec); err != nil {
		t.Fatalf("provision (create): %v", err)
	}
	if m.generated != 1 || len(m.persisted) != 1 || m.persisted[0] != "gtk_unit_test_token" {
		t.Fatalf("create path minted %d/persisted %v, want exactly one matching token", m.generated, m.persisted)
	}
	if _, err := p.Provision(context.Background(), spec); err != nil {
		t.Fatalf("provision (adopt): %v", err)
	}
	if m.generated != 1 || len(m.persisted) != 1 {
		t.Errorf("adoption touched the mint seam: generated=%d persisted=%v", m.generated, m.persisted)
	}
}

// TestProvisionGateShapeMismatchRebuilds: a pre-gate pod on a now-gated
// session is replaced by a gated pod (and the reverse), minting only for the
// gated create.
func TestProvisionGateShapeMismatchRebuilds(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_reshape")
	p := fakeProvider(readyPod(sid)) // pre-gate shape, ready
	markPodsReadyOnCreate(p)
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}
	if _, err := p.Provision(context.Background(), spec); err != nil {
		t.Fatalf("provision (reshape to gated): %v", err)
	}
	pod, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rebuilt pod: %v", err)
	}
	if !hasGateSidecar(pod) {
		t.Error("rebuilt pod has no gate sidecar")
	}
	if m.generated != 1 || len(m.persisted) != 1 {
		t.Errorf("reshape minted %d/persisted %d, want exactly one", m.generated, len(m.persisted))
	}

	// The reverse: the session no longer wants a gate — the gated pod is
	// replaced by a plain one, with no further minting.
	specUngated := sandbox.Spec{SessionID: sid, Image: "img"}
	if _, err := p.Provision(context.Background(), specUngated); err != nil {
		t.Fatalf("provision (reshape to ungated): %v", err)
	}
	pod, err = p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get re-rebuilt pod: %v", err)
	}
	if hasGateSidecar(pod) {
		t.Error("ungated reshape kept the gate sidecar")
	}
	if m.generated != 1 {
		t.Errorf("ungated reshape minted (generated=%d)", m.generated)
	}
}

// TestProvisionGatedPersistFailureCleansUp: a pod whose token was never
// recorded can only ever 401 — Provision must fail and remove it.
func TestProvisionGatedPersistFailureCleansUp(t *testing.T) {
	m := &mintRecorder{persistErr: errors.New("db down")}
	sid := domain.ID("sesn_persistfail")
	p := fakeProvider()
	markPodsReadyOnCreate(p)
	spec := sandbox.Spec{SessionID: sid, Image: "img", Gate: gateSpecFixture(m)}
	if _, err := p.Provision(context.Background(), spec); err == nil {
		t.Fatal("provision succeeded despite a token-persist failure")
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("pod with an unpersisted token was left behind (get err = %v)", err)
	}
}

// TestProvisionGatedAdoptUnreadyReclaims: adopting a gated pod whose gate never
// becomes ready must reclaim it, not just error. This is the crash-window
// recovery: an executor that died between the pod create and the token Persist
// leaves a gate that can only ever 401 and crash-loop, so the pod never turns
// ready — without the reclaim every retry would re-adopt the same wedged pod
// forever (the Docker twin recovers by rebuilding a stopped gate; a native
// sidecar never presents as stopped).
func TestProvisionGatedAdoptUnreadyReclaims(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_wedged")
	p := fakeProvider()
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}

	// The wedged pod: correct gated shape and session labels (built by podSpec
	// itself), sandbox ready but the gate sidecar never Ready — the signature of
	// an unpersisted token. The fake clientset assigns no UID, so set one for
	// the reclaim's UID-guarded delete to match.
	wedged := p.podSpec(podName(sid), sandbox.DefaultWorkdir, spec, "gtk_never_persisted")
	wedged.UID = "uid-wedged"
	wedged.Status = corev1.PodStatus{
		Phase:                 corev1.PodRunning,
		ContainerStatuses:     []corev1.ContainerStatus{{Name: containerName, Ready: true}},
		InitContainerStatuses: []corev1.ContainerStatus{{Name: gateContainerName, Ready: false}},
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), wedged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// A short deadline stands in for waitReady's 2-minute timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := p.Provision(ctx, spec); err == nil {
		t.Fatal("adopting a never-ready gated pod: want an error")
	}
	if m.generated != 0 || len(m.persisted) != 0 {
		t.Errorf("adoption touched the mint seam: generated=%d persisted=%v", m.generated, m.persisted)
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("wedged gated pod was not reclaimed (get err = %v); the session can never recover", err)
	}
}

// TestProvisionGatedCreateRaceAdoptsWinner: a create that loses the 409 race
// against a same-shape pod adopts the winner, and the loser's generated token
// is discarded unpersisted — persisting it would revoke the winner's.
func TestProvisionGatedCreateRaceAdoptsWinner(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_race")
	p := fakeProvider()
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}

	// The winner's pod, gated and ready, is in the tracker — but the loser's
	// initial existence Get must 404 (it raced ahead of the winner's create),
	// and its own create must answer 409. The get-reactor 404s exactly once.
	winner := p.podSpec(podName(sid), sandbox.DefaultWorkdir, spec, "gtk_winners_token")
	winner.UID = "uid-race-winner"
	winner.Status = corev1.PodStatus{
		Phase:                 corev1.PodRunning,
		ContainerStatuses:     []corev1.ContainerStatus{{Name: containerName, Ready: true}},
		InitContainerStatuses: []corev1.ContainerStatus{{Name: gateContainerName, Ready: true}},
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), winner, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := p.client.cs.(*fake.Clientset)
	first := true
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), podName(sid))
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), podName(sid))
	})

	if _, err := p.Provision(context.Background(), spec); err != nil {
		t.Fatalf("losing the create race against a same-shape pod should adopt it: %v", err)
	}
	if m.generated != 1 {
		t.Errorf("race loser generated %d tokens, want the 1 minted before its create", m.generated)
	}
	if len(m.persisted) != 0 {
		t.Errorf("race loser persisted %v — that would revoke the winner's live token", m.persisted)
	}
}

// TestProvisionGatedCreateRaceMismatchFailsClosed: losing the create race to a
// pod of the wrong gate shape is a hard error — serving the mismatched pod
// would run a gated session without its gate. The loser must not persist and
// must not delete the winner (the next attempt replaces it).
func TestProvisionGatedCreateRaceMismatchFailsClosed(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_racebad")
	p := fakeProvider()
	gatedSpec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}

	// The winner raced in UNGATED while we are provisioning gated.
	ungated := gatedSpec
	ungated.Gate = nil
	winner := p.podSpec(podName(sid), sandbox.DefaultWorkdir, ungated, "")
	winner.UID = "uid-racebad-winner"
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), winner, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := p.client.cs.(*fake.Clientset)
	first := true
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), podName(sid))
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), podName(sid))
	})

	if _, err := p.Provision(context.Background(), gatedSpec); err == nil {
		t.Fatal("losing the race to a wrong-shape pod: want a fail-closed error")
	}
	if len(m.persisted) != 0 {
		t.Errorf("mismatch race loser persisted %v", m.persisted)
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); err != nil {
		t.Errorf("mismatch race loser deleted the winner's pod: %v", err)
	}
}

// TestPodReadyRequiresGateSidecar pins the gated readiness rule directly: a
// ready sandbox container alone is not enough when the pod is gated.
func TestPodReadyRequiresGateSidecar(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, Ready: true}},
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: gateContainerName, Ready: false},
		},
	}}
	if podReady(pod, true) {
		t.Error("gated pod with an unready gate sidecar reported ready")
	}
	if !podReady(pod, false) {
		t.Error("ungated readiness must ignore init container statuses")
	}
	pod.Status.InitContainerStatuses[0].Ready = true
	if !podReady(pod, true) {
		t.Error("gated pod with both ready reported not ready")
	}
}

// The bulk write's two script exit codes, pinned against a real shell the way the
// single write's are: the numbers the scripts spell as literals must be the
// constants the Go side classifies on, or a drift renames a recoverable extraction
// failure into an unclassified one and the retry that answers it never runs.
//
// The archive is delivered on stdin, so a failed extraction is the one failure this
// backend can have that the docker backend cannot — its daemon extracts on the host
// and answers over HTTP instead. The split between the two codes is `tar`'s to
// make, and it is not the obvious one: bytes that are not an archive fail the
// extraction (18), but an empty stream, or one truncated to a block of zeros, is a
// perfectly valid *empty* archive to GNU tar, which exits 0. What catches that is
// the rename script refusing a manifest that never arrived (17) — the same guard
// that stops a batch whose manifest was deleted underneath it from reading as a
// success that wrote nothing.
func TestBulkScriptsClassifyAnArchiveThatDidNotArrive(t *testing.T) {
	dir := t.TempDir()
	run := func(stdin []byte) int {
		t.Helper()
		cmd := exec.Command("/bin/bash", "-c", bulkWriteScript, "map-bulk-write",
			dir+"/manifest", dir+"/dirs")
		cmd.Stdin = bytes.NewReader(stdin)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run bulkWriteScript: %v", err)
			}
			return ee.ExitCode()
		}
		return 0
	}
	if got := run([]byte("this is not a tar archive at all\n")); got != sandbox.ExitBulkExtract {
		t.Errorf("bytes that are not an archive: exit %d, want ExitBulkExtract (%d)",
			got, sandbox.ExitBulkExtract)
	}
	// A block of zeros is an end-of-archive marker, so every tar accepts it as a
	// valid *empty* archive and exits 0. Nothing was delivered, so the rename
	// script's own guard is what has to catch it.
	if got := run(bytes.Repeat([]byte{0}, 512)); got != sandbox.ExitBulkIncomplete {
		t.Errorf("an empty archive: exit %d, want ExitBulkIncomplete (%d)",
			got, sandbox.ExitBulkIncomplete)
	}
	// A stream carrying nothing at all is the one case the two tars disagree
	// about — GNU refuses it as not an archive (measured on CI), BSD takes it for
	// an empty one — so which of the two codes comes back is the image's to
	// decide, not ours. What must hold on every image is that it is one of them:
	// a delivery that brought no manifest can never read as a batch that wrote
	// what it was given.
	if got := run(nil); got != sandbox.ExitBulkExtract && got != sandbox.ExitBulkIncomplete {
		t.Errorf("an empty stream: exit %d, want ExitBulkExtract (%d) or ExitBulkIncomplete (%d) — never a success",
			got, sandbox.ExitBulkExtract, sandbox.ExitBulkIncomplete)
	}
}
