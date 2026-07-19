package k8s

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	gopath "path"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	run := func(t *testing.T, stdin []byte, declared int, path string) int {
		t.Helper()
		cmd := exec.Command("/bin/bash", "-c", writeScript, "map-write", path, gopath.Dir(path), strconv.Itoa(declared))
		cmd.Stdin = bytes.NewReader(stdin)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run writeScript: %v", err)
			}
			return ee.ExitCode()
		}
		return 0
	}
	dir := t.TempDir()

	// The bytes arrived intact — including ones no shell round-trip would
	// survive — and the parent directory is created on the way.
	t.Run("FullDelivery", func(t *testing.T) {
		payload := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		path := dir + "/deep/nested/blob.bin"
		if code := run(t, payload, len(payload), path); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("file = %v, %v; want %v", got, err, payload)
		}
	})

	// The #103 signature: the stdin stream delivered nothing, the redirection
	// truncated the file anyway, and `cat` exited 0. Without the length check
	// this is indistinguishable from a successful write.
	t.Run("NothingDelivered", func(t *testing.T) {
		if code := run(t, nil, 4, dir+"/lost"); code != writeShort {
			t.Errorf("exit %d, want %d (short write)", code, writeShort)
		}
	})

	// A stream that lost only its tail must not read as success either.
	t.Run("PartialDelivery", func(t *testing.T) {
		if code := run(t, []byte("kept"), 100, dir+"/partial"); code != writeShort {
			t.Errorf("exit %d, want %d (short write)", code, writeShort)
		}
	})

	// Writing no bytes is a legitimate write of an empty file, not a loss.
	t.Run("EmptyWriteIsNotShort", func(t *testing.T) {
		path := dir + "/empty"
		if code := run(t, nil, 0, path); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stat empty file: %v", err)
		}
	})

	// A write that cannot land at all keeps its own failure code — the length
	// check must not swallow it and report a short write instead.
	t.Run("UnwritablePathIsNotShort", func(t *testing.T) {
		if code := run(t, []byte("x"), 1, dir); code == 0 || code == writeShort {
			t.Errorf("exit %d writing onto a directory, want a non-zero code other than %d", code, writeShort)
		}
	})

	// The count comes from the stream, not from re-reading the target, so a
	// destination that keeps nothing is still a delivered write. Re-stating the
	// path here would report 0 bytes and fail a write that in fact succeeded —
	// and the docker backend accepts this, so the two would diverge.
	t.Run("DestinationThatKeepsNothing", func(t *testing.T) {
		if code := run(t, []byte("swallowed"), 9, "/dev/null"); code != 0 {
			t.Errorf("exit %d writing to /dev/null, want 0", code)
		}
	})

	// `tee` needs only write permission; re-reading the target would need read
	// permission the sandbox user may not have on a file it can legitimately
	// write.
	t.Run("WriteOnlyFile", func(t *testing.T) {
		path := dir + "/writeonly"
		if err := os.WriteFile(path, nil, 0o200); err != nil {
			t.Fatalf("stage write-only file: %v", err)
		}
		if os.Geteuid() == 0 {
			t.Skip("root ignores the read bit, so this proves nothing")
		}
		if code := run(t, []byte("kept"), 4, path); code != 0 {
			t.Errorf("exit %d writing a write-only file, want 0", code)
		}
	})
}

// gnuStatEnv supplies a `stat -c %s` shim when — and only when — the host's own
// stat rejects `-c`, which BSD stat does. readScript reaches its size gate before
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
	// readScript's one and only stat invocation is `stat -c %s "$f"`.
	shim := "#!/bin/sh\nexec /usr/bin/stat -f %z \"$3\"\n"
	if err := os.WriteFile(bin+"/stat", []byte(shim), 0o755); err != nil {
		t.Fatalf("stage stat shim: %v", err)
	}
	return append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		for _, c := range []struct {
			name string
			path string
			cap  int
			want int
		}{
			{"Missing", dir + "/nope", sandbox.MaxFileBytes, readNotExist},
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
		code          int
		watchdogFired bool
		v             verdict
		want          bool
	}{
		// The regression. The watchdog says it fired and the exit code agrees a
		// SIGKILL landed; no probe needs to have caught the command alive.
		{"WatchdogFiredAndProbeMissedIt", sigkillExit, true, verdict{}, true},
		{"WatchdogFiredAndProbeSawIt", sigkillExit, true, verdict{aliveAtDeadline: true}, true},

		// A SIGKILL the watchdog did not deliver is still the deadline's if the
		// command was alive to receive it — the tenant can kill the watchdog, and
		// the node can do the killing, so the probe keeps earning its place.
		{"ProbeAloneStillCounts", sigkillExit, false, verdict{aliveAtDeadline: true}, true},

		// Overrunning is a timeout on its own authority: a command still running
		// past the deadline and the slop can report no exit code worth believing.
		{"OverranWithoutASigkill", other, false, verdict{overran: true}, true},
		{"OverranWithNoEvidenceAtAll", 0, false, verdict{overran: true}, true},

		// The self-inflicted kill the contract suite pins: exit 137, but the
		// watchdog never fired and the command was already gone when Exec looked.
		{"SelfInflictedKillIsNotATimeout", sigkillExit, false, verdict{}, false},

		// A mark without a SIGKILL is not a timeout. This is the window between
		// the watchdog's last `kill -0` and its `kill -9`, where the command exits
		// on its own terms and the mark is already written — and it is also what
		// keeps a forged mark from manufacturing a timeout out of a clean exit.
		{"MarkWithoutASigkill", other, true, verdict{}, false},
		{"MarkWithACleanExit", 0, true, verdict{}, false},

		// An honest command that finished inside its deadline.
		{"CleanExit", 0, false, verdict{}, false},
		{"CleanExitSeenAliveJustBefore", 0, false, verdict{aliveAtDeadline: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTimeout(c.code, c.watchdogFired, c.v); got != c.want {
				t.Errorf("classifyTimeout(%d, %v, %+v) = %v, want %v",
					c.code, c.watchdogFired, c.v, got, c.want)
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
		{name: "KilledByTheWatchdog", out: "137 K\n", code: sigkillExit, killed: true},
		{name: "FinishedOnItsOwn", out: "0 \n", code: 0},
		{name: "NonZeroExit", out: "7 \n", code: 7},
		// The $PPID sabotage: no wrapper left to record anything. It reads as the
		// kill's code, and there is no mark to believe with the wrapper gone.
		{name: "NothingRecorded", out: "", code: sigkillExit},
		{name: "WhitespaceOnly", out: "\n", code: sigkillExit},
		// Only the mark's presence is trusted, never its content — the wrapper
		// writes the letter itself and never echoes the file's bytes.
		{name: "UnknownSecondField", out: "137 X\n", code: sigkillExit},
		{name: "GarbageCode", out: "not-a-code\n", fails: true},
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
		t.Skipf("host has neither setsid nor perl to stand in for it: %v", err)
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
	run := func(t *testing.T, name, command string, seconds int) string {
		t.Helper()
		state := dir + "/" + name
		cmd := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", command, strconv.Itoa(seconds), state)
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			t.Fatalf("run execWrapper: %v", err)
		}
		recorded, err := os.ReadFile(state + ".exit")
		if err != nil {
			t.Fatalf("read recorded exit: %v", err)
		}
		return string(recorded)
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
		if !classifyTimeout(code, killed, verdict{}) {
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
		if classifyTimeout(code, killed, verdict{}) {
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
		if classifyTimeout(code, killed, verdict{}) {
			t.Error("a self-inflicted SIGKILL classified as a timeout")
		}
	})

	// The mark must never be able to hold the kill back. A tenant that
	// pre-creates the mark's path as a directory makes the write fail; the kill
	// still has to land, and the command still has to die on its deadline —
	// classification falls back to the probes, which is where it stood before.
	t.Run("AnUnwritableMarkStillKills", func(t *testing.T) {
		state := dir + "/blocked"
		if err := os.Mkdir(state+".killed", 0o755); err != nil {
			t.Fatalf("stage the blocked mark: %v", err)
		}
		cmd := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", "sleep 30", "1", state)
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			t.Fatalf("run execWrapper: %v", err)
		}
		recorded, err := os.ReadFile(state + ".exit")
		if err != nil {
			t.Fatalf("read recorded exit: %v", err)
		}
		code, killed, err := parseExit(string(recorded))
		if err != nil || code != sigkillExit {
			t.Fatalf("parseExit = %d, %v, %v; want %d — the kill did not land", code, killed, err, sigkillExit)
		}
		if killed {
			t.Error("a mark that could not be written was read as written")
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
	stage := func(t *testing.T, name string, files map[string]string) string {
		t.Helper()
		state := dir + "/" + name
		for suffix, content := range files {
			if err := os.WriteFile(state+suffix, []byte(content), 0o644); err != nil {
				t.Fatalf("stage %s: %v", suffix, err)
			}
		}
		return state
	}

	t.Run("KilledByTheWatchdog", func(t *testing.T) {
		state := stage(t, "killed", map[string]string{".pid": "42\n", ".exit": "137 K\n", ".killed": ""})
		if code, killed, err := parseExit(run(t, state)); err != nil || code != sigkillExit || !killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
	})

	t.Run("FinishedOnItsOwn", func(t *testing.T) {
		state := stage(t, "clean", map[string]string{".pid": "42\n", ".exit": "0 \n"})
		if code, killed, err := parseExit(run(t, state)); err != nil || code != 0 || killed {
			t.Errorf("parseExit = %d, %v, %v; want 0, false, nil", code, killed, err)
		}
	})

	// The wrapper never recorded anything, and the cleanup still has to run.
	t.Run("NothingRecorded", func(t *testing.T) {
		state := stage(t, "sabotaged", map[string]string{".pid": "42\n"})
		if out := run(t, state); out != "" {
			t.Errorf("exitScript printed %q for a command that recorded nothing", out)
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
