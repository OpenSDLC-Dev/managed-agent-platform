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
