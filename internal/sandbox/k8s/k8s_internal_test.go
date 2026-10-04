package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	gopath "path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
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

// readyPod is the plain fixture: the ready pod a create from the tests' usual
// spec (image "img", default workdir, unrestricted) would have left behind —
// faithful via readyFromSpec, so adoption's spec check sees a real shape.
func readyPod(sid domain.ID) *corev1.Pod {
	return readyFromSpec(fakeProvider(), sandbox.DefaultWorkdir,
		sandbox.Spec{SessionID: sid, Image: "img"})
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

// readyFromSpec builds exactly the pod podSpec would create for the spec —
// marked running and ready — so what the adoption tests stage in the tracker
// is what a real create would have left in the cluster.
func readyFromSpec(p *Provider, workdir string, spec sandbox.Spec) *corev1.Pod {
	pod := p.podSpec(podName(spec.SessionID), workdir, spec, "")
	pod.Namespace = "default"
	pod.UID = types.UID("uid-" + pod.Name)
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, Ready: true}},
	}
	return pod
}

// A pod that carries this session's label but was created from a different
// spec must not be adopted: its networking shape, image, and workdir are fixed
// at create, so adopting it would silently serve the wrong containment. The
// mismatch fails closed with sandbox.ErrSpecMismatch and deletes nothing
// (#296, the k8s twin of #29).
func TestProvisionRefusesAdoptingAMismatchedPod(t *testing.T) {
	sid := domain.ID("sesn_mismatch")
	created := sandbox.Spec{SessionID: sid, Image: "img:1",
		Networking: domain.Networking{Type: domain.NetUnrestricted}}
	for name, requested := range map[string]sandbox.Spec{
		"networking": {SessionID: sid, Image: "img:1",
			Networking: domain.Networking{Type: domain.NetLimited}},
		"image": {SessionID: sid, Image: "img:2",
			Networking: domain.Networking{Type: domain.NetUnrestricted}},
		"workdir": {SessionID: sid, Image: "img:1",
			Networking: domain.Networking{Type: domain.NetUnrestricted}, Workdir: "/elsewhere"},
	} {
		t.Run(name, func(t *testing.T) {
			p := fakeProvider()
			pod := readyFromSpec(p, sandbox.DefaultWorkdir, created)
			if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			cs := p.client.cs.(*fake.Clientset)
			cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				t.Error("a mismatch has no authority to delete the existing pod")
				return true, nil, errors.New("refused")
			})
			if _, err := p.Provision(context.Background(), requested); !errors.Is(err, sandbox.ErrSpecMismatch) {
				t.Errorf("provision over a mismatched pod: err = %v, want sandbox.ErrSpecMismatch", err)
			}
		})
	}
}

// The check must not refuse what it should adopt: a limited session's own pod
// — netsetup init container and all — is still adopted, with no create and no
// delete.
func TestProvisionAdoptsAMatchingLimitedPod(t *testing.T) {
	sid := domain.ID("sesn_limadopt")
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}}
	p := fakeProvider()
	pod := readyFromSpec(p, sandbox.DefaultWorkdir, spec)
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := p.client.cs.(*fake.Clientset)
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Error("adopting a matching pod must not create")
		return true, nil, errors.New("refused")
	})
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Error("adopting a matching pod must not delete")
		return true, nil, errors.New("refused")
	})
	sb, err := p.Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("provision (adopt matching limited): %v", err)
	}
	if sb.ID() != podName(sid) {
		t.Errorf("adopted sandbox id = %q, want %q", sb.ID(), podName(sid))
	}
}

// The same check guards the gated adopt path: a gated pod whose gate shape
// matches is still refused when its image was fixed at create from a different
// spec — before anything is minted or deleted.
func TestProvisionRefusesAMismatchedGatedPod(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_gatedmis")
	created := sandbox.Spec{SessionID: sid, Image: "img:1",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}
	p := fakeProvider()
	pod := readyFromSpec(p, sandbox.DefaultWorkdir, created)
	// Gate sidecar ready too: the spec check runs only after the readiness
	// wait (so the #198 wedged-pod reclaim stays reachable), and a gated pod
	// is not ready without its gate.
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: gateContainerName, Ready: true}}
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := p.client.cs.(*fake.Clientset)
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Error("a mismatch has no authority to delete the existing pod")
		return true, nil, errors.New("refused")
	})
	requested := created
	requested.Image = "img:2"
	if _, err := p.Provision(context.Background(), requested); !errors.Is(err, sandbox.ErrSpecMismatch) {
		t.Errorf("gated provision over a mismatched pod: err = %v, want sandbox.ErrSpecMismatch", err)
	}
	if m.generated != 0 || len(m.persisted) != 0 {
		t.Errorf("the refusal touched the mint seam: generated=%d persisted=%v", m.generated, m.persisted)
	}
}

// A pod that is both wedged (its gate never turns ready) and spec-mismatched
// must still be reclaimed: the readiness wait and its #198 reclaim run before
// the spec check, deliberately — refusing first would strand a dead pod
// forever, since a mismatch deletes nothing and the platform has no other
// automatic teardown.
func TestProvisionGatedAdoptWedgedMismatchStillReclaims(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_wedgedmis")
	p := fakeProvider()
	created := sandbox.Spec{SessionID: sid, Image: "img:1",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m)}
	wedged := p.podSpec(podName(sid), sandbox.DefaultWorkdir, created, "gtk_never_persisted")
	wedged.UID = "uid-wedgedmis"
	wedged.Status = corev1.PodStatus{
		Phase:                 corev1.PodRunning,
		ContainerStatuses:     []corev1.ContainerStatus{{Name: containerName, Ready: true}},
		InitContainerStatuses: []corev1.ContainerStatus{{Name: gateContainerName, Ready: false}},
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Create(context.Background(), wedged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	requested := created
	requested.Image = "img:2"
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := p.Provision(ctx, requested); err == nil {
		t.Fatal("adopting a never-ready mismatched pod: want an error")
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("wedged mismatched pod was not reclaimed (get err = %v); no retry could ever recover the session", err)
	}
}

// The create-race loser applies the same validation to the winner's pod it
// adopts: a winner created from a different spec is refused, not served — and
// not deleted.
func TestProvisionRefusesAMismatchedRaceWinner(t *testing.T) {
	sid := domain.ID("sesn_racemis")
	winnerSpec := sandbox.Spec{SessionID: sid, Image: "img:1",
		Networking: domain.Networking{Type: domain.NetUnrestricted}}
	requested := sandbox.Spec{SessionID: sid, Image: "img:2",
		Networking: domain.Networking{Type: domain.NetUnrestricted}}
	p := fakeProvider()
	winner := readyFromSpec(p, sandbox.DefaultWorkdir, winnerSpec)
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
	if _, err := p.Provision(context.Background(), requested); !errors.Is(err, sandbox.ErrSpecMismatch) {
		t.Errorf("losing the race to a mismatched winner: err = %v, want sandbox.ErrSpecMismatch", err)
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); err != nil {
		t.Errorf("mismatch race loser deleted the winner's pod: %v", err)
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

// hookedEnv is env (nil inheriting this process's) with BASH_ENV naming a file
// that holds sandboxtest.BannerHook, as the hooked image's sets it.
func hookedEnv(t *testing.T, env []string) []string {
	t.Helper()
	if env == nil {
		env = os.Environ()
	}
	hook := t.TempDir() + "/hook.sh"
	if err := os.WriteFile(hook, []byte(sandboxtest.BannerHook), 0o644); err != nil {
		t.Fatalf("stage the hook: %v", err)
	}
	return append(append([]string{}, env...), "BASH_ENV="+hook)
}

// readScript's frame is what makes a short exec stdout stream visible, and
// what keeps an image's startup output out of the file, so its contract is
// pinned here rather than left to the live cluster: no cluster can be told to
// lose bytes, but everything else — that the file's bytes go out inside the
// frame on success and none go out otherwise, that the classification exits
// still fire, and that a startup file's banner and EXIT trap stay outside —
// is observable from the host's shell, on any machine, in milliseconds.
//
// This runs the exec ReadFile makes (readArgv) through the host's /bin/bash
// rather than the sandbox image, with sandboxtest.BannerHook as its BASH_ENV file, as the
// write-side test does. It pins what the script does with its arguments; that
// the image carries a userland able to run it is the live contract test's job.
func TestReadScriptFramesWhatItSent(t *testing.T) {
	env := hookedEnv(t, gnuStatEnv(t))
	dir := t.TempDir()
	run := func(t *testing.T, path string, cap int) (int, []byte) {
		t.Helper()
		f := sandbox.NewFrame("read")
		var out bytes.Buffer
		argv := readArgv(f, path, int64(cap))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env, cmd.Stdout = env, &out
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run readScript: %v", err)
			}
			code = ee.ExitCode()
		}
		if !bytes.HasPrefix(out.Bytes(), []byte("welcome to the image ")) || !bytes.HasSuffix(out.Bytes(), []byte("exit banner ")) {
			t.Fatalf("stdout %q; want the hook's banner before the frame and its trap's words after it", out.Bytes())
		}
		got, framed, short := f.CutBytes(out.Bytes(), false)
		if !framed || short {
			t.Fatalf("stdout %q is not framed whole", out.Bytes())
		}
		return code, got
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
	// survive — and nothing of the banner around them.
	t.Run("FullDelivery", func(t *testing.T) {
		payload := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		if code, out := run(t, stage(t, "blob.bin", payload), sandbox.MaxFileBytes); code != 0 || !bytes.Equal(out, payload) {
			t.Errorf("exit %d, file %v; want 0 and %v", code, out, payload)
		}
	})

	// A payload spanning many stream buffers, which a handful of bytes does not
	// reach.
	t.Run("LargePayload", func(t *testing.T) {
		payload := make([]byte, 1<<20)
		for i := range payload {
			payload[i] = byte(i)
		}
		if code, out := run(t, stage(t, "large.bin", payload), sandbox.MaxFileBytes); code != 0 || !bytes.Equal(out, payload) {
			t.Errorf("exit %d, %d bytes; want 0 and %d matching", code, len(out), len(payload))
		}
	})

	// Reading no bytes is a legitimate read of an empty file, framed like any
	// other — that is what keeps it distinguishable from a stream that
	// delivered nothing at all.
	t.Run("EmptyFileIsFramed", func(t *testing.T) {
		if code, out := run(t, stage(t, "empty", nil), sandbox.MaxFileBytes); code != 0 || len(out) != 0 {
			t.Errorf("exit %d, file %q; want 0 and nothing", code, out)
		}
	})

	// A read that cannot happen keeps its own failure code and sends no bytes, so
	// an unreadable file cannot arrive as a successful read of fewer bytes.
	t.Run("UnreadableFileSendsNothing", func(t *testing.T) {
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
	// arrive as a read of zero bytes.
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
// fed byte-for-byte into the buffer ReadFile actually uses, so the cap
// arithmetic is exercised rather than asserted: the frame and what an image's
// startup prints around it ride in the same buffer as the content, in the room
// beside the cap (readRoom).
func TestReadStdoutRequiresTheFrame(t *testing.T) {
	f := sandbox.NewFrame("read")
	b, e := f.Lines()
	begin, end := []byte(b), []byte(e)
	// The buffer ReadFile hands the exec, filled the way the stream fills it.
	recv := func(chunks ...[]byte) *cappedBuffer {
		out := &cappedBuffer{limit: sandbox.MaxFileBytes + readRoom}
		for _, c := range chunks {
			for len(c) > 0 {
				n := min(len(c), 32768)
				_, _ = out.Write(c[:n])
				c = c[n:]
			}
		}
		return out
	}
	read := func(out *cappedBuffer) ([]byte, error) { return readStdout("/w/f", f, sandbox.MaxFileBytes, out) }
	body := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }

	// Bytes inside the frame are a complete read, and only the file's bytes come
	// back — not the banner an image's startup printed before the frame, nor
	// what its EXIT trap printed after.
	t.Run("WholeFile", func(t *testing.T) {
		want := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i', 0x00}
		for _, around := range [][2]string{{"", ""}, {"welcome to the image ", "exit banner "}} {
			got, err := read(recv([]byte(around[0]), begin, want, end, []byte(around[1])))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("readStdout = %v, %v; want %v", got, err, want)
			}
		}
	})

	// An empty file is a file, which is why the frame goes out unconditionally
	// on success: an empty read is not evidence of a lost stream.
	t.Run("EmptyFileIsNotShort", func(t *testing.T) {
		got, err := read(recv(begin, end))
		if err != nil || len(got) != 0 {
			t.Errorf("readStdout = %q, %v; want an empty read", got, err)
		}
	})

	// A file whose own bytes hold the end line's constant part still
	// round-trips whole: only this read's nonce ends it.
	t.Run("ContentHoldingAnEndLineOfItsOwn", func(t *testing.T) {
		want := append(append([]byte("\nmap-read-end-0123456789abcdef\n"), body(10)...), '\n')
		got, err := read(recv(begin, want, end))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("readStdout = %d bytes, %v; want %d", len(got), err, len(want))
		}
	})

	// The #105 signature: the exec exited 0 and stdout stopped early. Each of
	// these is, without the end line, indistinguishable from a shorter file.
	t.Run("NothingArrived", func(t *testing.T) {
		if got, err := read(recv()); err == nil {
			t.Errorf("an empty stream read back as %d bytes", len(got))
		}
	})
	t.Run("TailLost", func(t *testing.T) {
		got, err := read(recv(begin, body(100)))
		if err == nil {
			t.Fatalf("a stream that lost its tail read back as %d bytes", len(got))
		}
		if errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Errorf("err = %v, want a short read rather than a size fault", err)
		}
	})
	t.Run("EndLineCutInHalf", func(t *testing.T) {
		if _, err := read(recv(begin, body(100), end[:len(end)/2])); err == nil {
			t.Error("a half-delivered end line read back as a whole file")
		}
	})

	// A file at exactly the cap is the largest legal read, with the banner and
	// the trap's words beside it in the room.
	t.Run("AtTheCapIsNotTooLarge", func(t *testing.T) {
		got, err := read(recv([]byte("welcome to the image "), begin, body(sandbox.MaxFileBytes), end, []byte("exit banner ")))
		if err != nil || len(got) != sandbox.MaxFileBytes {
			t.Errorf("readStdout = %d bytes, %v; want %d and no error", len(got), err, sandbox.MaxFileBytes)
		}
	})

	// One byte past it is a size fault: the file grew after readScript's gate.
	// It is one whether the room took the excess whole, or the cap cut it with
	// the end line.
	t.Run("PastTheCapIsTooLarge", func(t *testing.T) {
		for _, stream := range [][][]byte{
			{begin, body(sandbox.MaxFileBytes + 1), end},
			{begin, body(sandbox.MaxFileBytes + readRoom), end},
		} {
			if _, err := read(recv(stream...)); !errors.Is(err, sandbox.ErrFileTooLarge) {
				t.Errorf("err = %v, want ErrFileTooLarge", err)
			}
		}
	})

	// A startup that prints past the room before the script does leaves no
	// begin line, or cuts a file no larger than the cap: neither is a size
	// fault the file made, nor a file, but the startup's, which every read on
	// that image meets again (sandbox.StartupOutputError).
	t.Run("ABannerPastTheRoomIsNeitherAFileNorTooLarge", func(t *testing.T) {
		for _, stream := range [][][]byte{
			{body(sandbox.MaxFileBytes + readRoom), begin, body(10), end},
			{body(readRoom + 10), begin, body(sandbox.MaxFileBytes), end},
		} {
			var startup *sandbox.StartupOutputError
			if got, err := read(recv(stream...)); !errors.As(err, &startup) || startup.Ran {
				t.Errorf("readStdout = %d bytes, %v; want a StartupOutputError of a read, which runs nothing", len(got), err)
			}
		}
	})

	// A flood an EXIT trap prints after the end line is no part of the file,
	// however far past the room it runs.
	t.Run("ATrapFloodAfterTheEndLine", func(t *testing.T) {
		got, err := read(recv(begin, body(4), end, body(2*readRoom)))
		if err != nil || !bytes.Equal(got, body(4)) {
			t.Errorf("readStdout = %q, %v; want the file alone", got, err)
		}
	})

	// A file that holds this read's own end line, more than the room away from
	// either end of the stream, is read whole: the lines are looked for near
	// the stream's ends (readRoom), where the frame puts them.
	t.Run("ContentHoldingThisReadsEndLine", func(t *testing.T) {
		want := append(append(append([]byte{}, body(readRoom+10)...), end...), body(readRoom+10)...)
		got, err := read(recv([]byte("welcome to the image "), begin, want, end, []byte("exit banner ")))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("readStdout = %d bytes, %v; want the %d-byte file whole", len(got), err, len(want))
		}
	})

	// The returned slice must not lend its spare capacity back over the end line.
	t.Run("ReturnedSliceIsClipped", func(t *testing.T) {
		got, err := read(recv(begin, body(4), end))
		if err != nil {
			t.Fatalf("readStdout: %v", err)
		}
		if cap(got) != len(got) {
			t.Errorf("cap %d, len %d: appending would write over the end line", cap(got), len(got))
		}
	})
}

// BenchmarkReadStdout reads a file at the read cap and one at the harvest's
// 50 MB cap out of their buffers, banner and trap around them: the frame's
// lines are looked for within readRoom of the stream's ends, so the cost does
// not grow with the file (measured on an M-series laptop: 6.3 ms and 69 ms
// when the whole buffer was searched, about 2 ms for both since).
func BenchmarkReadStdout(b *testing.B) {
	for _, size := range []int{sandbox.MaxFileBytes, 50 << 20} {
		f := sandbox.NewFrame("read")
		begin, end := f.Lines()
		out := &cappedBuffer{limit: size + readRoom}
		_, _ = out.Write([]byte("welcome to the image " + begin))
		_, _ = out.Write(bytes.Repeat([]byte("abcdefghij\n"), size/11+1)[:size])
		_, _ = out.Write([]byte(end + "exit banner "))
		b.Run(strconv.Itoa(size>>20)+"MiB", func(b *testing.B) {
			for b.Loop() {
				if _, err := readStdout("/f", f, int64(size), out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// classifier is a pod handle with Exec's default slop and probe lead and
// nothing else: all classifyTimeout reads off its receiver.
var classifier = &pod{overrunSlop: defaultOverrunSlop, probeLead: defaultProbeLead}

// classifyTimeout is where #95, #110, #832 and #838 were lost: a timeout the call
// reported as none, because the only evidence for it came from a probe that had
// raced an apiserver round trip and lost. Pinning the decision here costs no
// clock and no cluster, which is the point — on a live cluster the losing case is
// exactly the one that cannot be staged on demand.
func TestClassifyTimeout(t *testing.T) {
	const (
		other = 7
		sec   = time.Second
		slop  = defaultOverrunSlop
		lead  = defaultProbeLead
		ms    = time.Millisecond
	)
	// launch and watchdog are the wrapper's record of a run, counted from the
	// command's launch or from its watchdog's.
	launch := func(d time.Duration) runRecord { return runRecord{sinceLaunch: d} }
	watchdog := func(d time.Duration) runRecord { return runRecord{sinceWatchdog: d} }
	cases := []struct {
		name          string
		untimed       bool
		timeout       time.Duration // the 1s most rows use when 0
		code          int
		watchdogFired bool
		rec           runRecord
		v             verdict
		want          bool
	}{
		// A command given no deadline has no watchdog to mark it, so a mark it is
		// found wearing is one it planted — and an untimed command must not be
		// able to call itself timed out by planting one and exiting 137, nor by
		// writing a run time as long as it likes.
		{name: "NoDeadlineIgnoresAPlantedMark", untimed: true, code: sigkillExit, watchdogFired: true, want: false},
		{name: "NoDeadlineIgnoresALongRun", untimed: true, code: sigkillExit, rec: launch(time.Hour), want: false},
		{name: "NoDeadlineIgnoresALongRunSinceItsWatchdog", untimed: true, code: sigkillExit, rec: watchdog(time.Hour), want: false},
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

		// #832: the wrapper's record answers the overrun probe's question when the
		// probe answered too late to. Strictly longer than the deadline plus the
		// slop, whatever the exit code, and the probe's own "gone" changes nothing.
		{name: "RecordJustUnderTheOverrun", rec: launch(sec + slop - ms)},
		{name: "RecordAtTheOverrun", rec: launch(sec + slop)},
		{name: "RecordJustOverTheOverrun", rec: launch(sec + slop + ms), want: true},
		{name: "RecordOverranWithItsOwnCode", code: other, rec: launch(2 * sec), want: true},
		// The run since the watchdog's launch is no overrun's witness: it starts
		// after any hold-up of the wrapper past the command's launch, so an
		// overrun could hide in the hold.
		{name: "RecordSinceTheWatchdogIsNotAskedAboutAnOverrun", code: other, rec: watchdog(2 * sec)},

		// #838: the wrapper's record answers the pre-deadline probe's question
		// when the probe answered too late to. A SIGKILL that ended a run
		// strictly longer than the deadline less the probe's lead, counted from
		// the watchdog's launch, is the deadline's, marked or not; one at or
		// under that line is the command's own, as it is to the probe.
		{name: "SigkillJustUnderTheLead", code: sigkillExit, rec: watchdog(sec - lead - ms)},
		{name: "SigkillAtTheLead", code: sigkillExit, rec: watchdog(sec - lead)},
		{name: "SigkillJustPastTheLead", code: sigkillExit, rec: watchdog(sec - lead + ms), want: true},
		{name: "SigkillPastTheDeadline", code: sigkillExit, rec: watchdog(sec + 250*ms), want: true},
		// It answers the kill question only: a run past that line that ended
		// in the command's own code is the command's own exit.
		{name: "RecordSinceTheWatchdogNeedsASigkill", code: other, rec: watchdog(sec + 250*ms)},
		// And it counts from the watchdog's launch, not the command's: a run
		// that reaches the line only from the earlier origin was killed before
		// the watchdog had been running that long.
		{name: "SigkillCountsFromTheWatchdogNotTheLaunch", code: sigkillExit,
			rec: runRecord{sinceLaunch: sec + 10*ms, sinceWatchdog: sec - lead - 10*ms}},

		// The deadline the record is read against is the watchdog's, rounded up to
		// whole seconds, not the caller's fraction: 1.2s is a 2s watchdog.
		{name: "FractionalTimeoutRanPastTheRequestNotTheDeadline", timeout: 1200 * ms, rec: launch(1200*ms + slop + ms)},
		{name: "FractionalTimeoutRanPastTheRoundedDeadline", timeout: 1200 * ms, rec: launch(2*sec + slop + ms), want: true},
		{name: "FractionalTimeoutKilledPastTheRequestNotTheDeadline", timeout: 1200 * ms, code: sigkillExit, rec: watchdog(1200*ms + ms)},
		{name: "FractionalTimeoutKilledPastTheRoundedDeadlinesLead", timeout: 1200 * ms, code: sigkillExit, rec: watchdog(2*sec - lead + ms), want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			timeout := c.timeout
			if timeout == 0 {
				timeout = sec
			}
			if c.untimed {
				timeout = 0
			}
			if got := classifier.classifyTimeout(timeout, c.code, c.watchdogFired, c.rec, c.v); got != c.want {
				t.Errorf("classifyTimeout(%s, %d, %v, %+v, %+v) = %v, want %v",
					timeout, c.code, c.watchdogFired, c.rec, c.v, got, c.want)
			}
		})
	}
}

// parseExit is the other half of the same decision, and the half that has to
// stay compatible: the wrapper's mark rides on the exit line, so "the wrapper
// recorded nothing" must still be the one and only empty case.
func TestParseExitReadsTheWatchdogsMark(t *testing.T) {
	const ms = time.Millisecond
	cases := []struct {
		name   string
		out    string
		code   int
		killed bool
		rec    runRecord
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

		// How long the command ran (#832, #838): the three /proc/uptime readings
		// the wrapper took around it — at its launch, at its watchdog's, at its
		// reap — after the code.
		{name: "HowLongItRan", out: " 0 100.25 100.50 102.75\n", code: 0,
			rec: runRecord{sinceLaunch: 2500 * ms, sinceWatchdog: 2250 * ms}},
		{name: "KilledAndTimed", out: "K 137 5.00 5.01 6.01\n", code: sigkillExit, killed: true,
			rec: runRecord{sinceLaunch: 1010 * ms, sinceWatchdog: time.Second}},
		// Anything short of three readable readings, in order, is no record at
		// all, never a guess. That includes the stream losing its tail, which
		// cuts the reap's reading short or drops it: a lost suffix can only
		// shorten the record or remove it, so it can never lengthen a command
		// into a timeout.
		{name: "OneReading", out: " 0 100.25\n", code: 0},
		{name: "TwoReadings", out: " 0 100.25 100.50\n", code: 0},
		{name: "LastReadingCutShort", out: " 0 100.25 100.50 10\n", code: 0},
		{name: "LastReadingCutToAShorterRun", out: " 0 100.25 100.50 102.7\n", code: 0,
			rec: runRecord{sinceLaunch: 2450 * ms, sinceWatchdog: 2200 * ms}},
		{name: "WatchdogReadingBeforeTheLaunchReading", out: " 0 100.50 100.25 102.75\n", code: 0},
		{name: "UnreadableReading", out: " 0 x 100.50 102.75\n", code: 0},
		{name: "UnreadableWatchdogReading", out: " 0 100.25 x 102.75\n", code: 0},
		{name: "NegativeReading", out: " 0 -5.00 100.50 102.75\n", code: 0},
		{name: "ExtraField", out: " 0 100.25 100.50 102.75 9\n", code: 0},
		// A reading is decimal seconds and nothing else. Each of these would parse
		// as a duration — or as a float — if the record took them at face value.
		{name: "ReadingWithAUnit", out: " 0 1h0 100.50 102.75\n", code: 0},
		{name: "ReadingInMinutes", out: " 0 100.25 100.50 5m\n", code: 0},
		{name: "ReadingWithASign", out: " 0 100.25 +100.50 102.75\n", code: 0},
		{name: "ReadingWithAnExponent", out: " 0 1e2 100.50 102.75\n", code: 0},
		{name: "ReadingWithNoWholePart", out: " 0 .25 100.50 102.75\n", code: 0},
		{name: "ReadingWithAnEmptyFraction", out: " 0 100.25 100. 102.75\n", code: 0},
		{name: "ReadingWithTwoPoints", out: " 0 100.25 100.50 102.7.5\n", code: 0},
		{name: "WholeSecondReadings", out: " 0 100 101 103\n", code: 0,
			rec: runRecord{sinceLaunch: 3 * time.Second, sinceWatchdog: 2 * time.Second}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, killed, rec, err := parseExit(c.out)
			if c.fails {
				if err == nil {
					t.Fatalf("parseExit(%q) = %d, %v, %+v, nil; want an error", c.out, code, killed, rec)
				}
				return
			}
			if err != nil || code != c.code || killed != c.killed || rec != c.rec {
				t.Errorf("parseExit(%q) = %d, %v, %+v, %v; want %d, %v, %+v, nil",
					c.out, code, killed, rec, err, c.code, c.killed, c.rec)
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
	// An image's startup that turns errexit on, as well as printing, runs in
	// the wrapper's own shell and the command's: every row holds under it as
	// under none, a command's own exit code and a watchdog blocked from its
	// mark included (#860). It is the wrapper's BASH_ENV only — exitScript runs
	// framed in a pod, and bare here.
	hook := t.TempDir() + "/hook.sh"
	if err := os.WriteFile(hook, []byte("set -e\n"+sandboxtest.BannerHook), 0o644); err != nil {
		t.Fatalf("stage the hook: %v", err)
	}
	for _, startup := range []struct {
		name string
		env  []string
	}{{"NoStartup", nil}, {"ErrexitStartup", []string{"BASH_ENV=" + hook}}} {
		t.Run(startup.name, func(t *testing.T) { wrapperMarks(t, env, startup.env) })
	}
}

// wrapperMarks is TestExecWrapperMarksTheWatchdogsKill's rows, with startupEnv
// added to the wrapper's environment.
func wrapperMarks(t *testing.T, env, startupEnv []string) {
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
		wrapper.Env = append(append(append([]string{}, base...), startupEnv...), extraEnv...)
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
		code, killed, rec, err := parseExit(run(t, "killed", "sleep 30", 1))
		if err != nil || code != sigkillExit || !killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
		// The watchdog sleeps its whole deadline before it kills, and the record
		// starts before the watchdog is launched, so the run is the deadline —
		// never under it by more than the clock's hundredth of a second, and never
		// past the slop, or a punctual kill would read as an overrun. The run
		// since the watchdog's launch starts later still, so it is never the
		// longer of the two.
		wantRan(t, rec.sinceLaunch, time.Second-uptimeTick, time.Second+defaultOverrunSlop)
		if rec.sinceWatchdog > rec.sinceLaunch {
			t.Errorf("recorded %+v: the run since the watchdog's launch is longer than the run since the command's", rec)
		}
		if !classifier.classifyTimeout(time.Second, code, killed, rec, verdict{}) {
			t.Error("a command the watchdog killed on its deadline did not classify as a timeout")
		}
	})

	// An honest command that finishes early leaves no mark, and its own exit code
	// stands. Its watchdog is still asleep when it exits, so this also pins that
	// the wrapper never waits for the watchdog to notice.
	t.Run("FinishedInsideItsDeadline", func(t *testing.T) {
		code, killed, rec, err := parseExit(run(t, "clean", "exit 7", 5))
		if err != nil || code != 7 || killed {
			t.Fatalf("parseExit = %d, %v, %v; want 7, false, nil", code, killed, err)
		}
		wantRan(t, rec.sinceLaunch, 0, 5*time.Second)
		if classifier.classifyTimeout(5*time.Second, code, killed, rec, verdict{}) {
			t.Error("a command that finished inside its deadline classified as a timeout")
		}
	})

	// A command that SIGKILLs itself exits 137 without the watchdog firing, so
	// the mark is what keeps 137 from meaning "timeout" on its own.
	t.Run("SelfInflictedKillLeavesNoMark", func(t *testing.T) {
		code, killed, rec, err := parseExit(run(t, "selfkill", "kill -9 $$", 30))
		if err != nil || code != sigkillExit || killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, false, nil", code, killed, err, sigkillExit)
		}
		if classifier.classifyTimeout(30*time.Second, code, killed, rec, verdict{}) {
			t.Error("a self-inflicted SIGKILL classified as a timeout")
		}
	})

	// #838: a SIGKILL the watchdog did not deliver — the command disarmed it,
	// and the kill came from elsewhere (here the command itself, on a node the
	// OOM killer) — past the deadline, but inside the slop the overrun rule
	// waits out. It leaves no mark, and verdict{} is a pre-deadline probe that
	// answered too late to see the command alive. The wrapper's record since
	// the watchdog's launch is what still calls it the deadline's — where the
	// host has the clock that record is read from; where it does not, there is
	// no record and no timeout, never one made up. The disarming waits for the
	// watchdog, so the 1.2s runs from after its launch: the record falls under
	// the line only if the wrapper stalls a quarter of a second between the
	// watchdog's launch and its very next step.
	t.Run("SigkillPastTheDeadlineTheWatchdogDidNotDeliver", func(t *testing.T) {
		code, killed, rec, err := parseExit(run(t, "late", disarmWatchdog+"sleep 1.2\nkill -9 $$\n", 1))
		if err != nil || code != sigkillExit || killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, false, nil (3: the command never found its watchdog to disarm)",
				code, killed, err, sigkillExit)
		}
		if got, want := classifier.classifyTimeout(time.Second, code, killed, rec, verdict{}), hostRecordsRuns(); got != want {
			t.Errorf("a SIGKILL 0.2s past a 1s deadline that the watchdog did not deliver (recorded %+v): timed out %v, want %v", rec, got, want)
		}
	})

	// The other side of the same line: a SIGKILL that lands before the
	// deadline less the probe's lead is the command's own, record or none —
	// the line the pre-deadline probe has always drawn, and the shared
	// contract's (ExecSelfInflictedKillIsNotATimeout).
	t.Run("SigkillBeforeTheDeadlinesLeadIsTheCommandsOwn", func(t *testing.T) {
		code, killed, rec, err := parseExit(run(t, "early", "sleep 0.6\nkill -9 $$\n", 1))
		if err != nil || code != sigkillExit || killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, false, nil", code, killed, err, sigkillExit)
		}
		if classifier.classifyTimeout(time.Second, code, killed, rec, verdict{}) {
			t.Errorf("a SIGKILL 0.4s before a 1s deadline (recorded %+v) classified as a timeout", rec)
		}
	})

	// The mark must never be able to hold the kill back — the property that makes
	// it safe to write one at all. The path is the wrapper's own argv, so a
	// command can read it out of /proc and plant whatever it likes there before
	// the watchdog fires. Each of these makes the mark fail; each must still
	// leave the command dead on its deadline, with the classification falling
	// back to the probes and the wrapper's record of the run.
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
			code, killed, _, err := parseExit(run(t, state, "sleep 30", 1))
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
		code, killed, _, err := parseExit(run(t, state, "sleep 30", 1, "POSIXLY_CORRECT=1"))
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
		code, killed, _, err := parseExit(string(out))
		if err != nil || code != sigkillExit || !killed {
			t.Fatalf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
		if !classifier.classifyTimeout(time.Second, code, killed, runRecord{}, verdict{}) {
			t.Error("a watchdog kill whose wrapper was sabotaged did not classify as a timeout")
		}
	})
}

// disarmWatchdog is a command's opening that kills its watchdog — the
// wrapper's other child — once it exists, or exits 3 if it never does. Linux
// lists a process's children in /proc; macOS, which has no /proc, through
// pgrep.
const disarmWatchdog = `w=
for ((i = 0; i < 200; i++)); do
  for p in $(cat /proc/$PPID/task/$PPID/children 2>/dev/null || pgrep -P $PPID); do
    [ "$p" != "$$" ] && w=$p
  done
  [ -n "$w" ] && break
  sleep 0.01
done
[ -n "$w" ] || exit 3
kill -9 "$w" || exit 3
`

// uptimeTick is /proc/uptime's resolution: two readings of it can lose up to
// this much of the time between them.
const uptimeTick = 10 * time.Millisecond

// hostRecordsRuns reports whether this host has the clock the wrapper records
// a run by: a pod always has /proc/uptime, and a macOS host does not.
func hostRecordsRuns() bool {
	_, err := os.Stat("/proc/uptime")
	return err == nil
}

// wantRan checks a run time the wrapper recorded on the host's shell. Its clock
// is /proc/uptime, which a pod always has and a macOS host does not; there the
// check is the other half of the contract instead — no clock, no record, and
// never a reading made up from somewhere else.
func wantRan(t *testing.T, ran, atLeast, under time.Duration) {
	t.Helper()
	if _, err := os.Stat("/proc/uptime"); err != nil {
		if ran != 0 {
			t.Errorf("no /proc/uptime on this host, yet the wrapper recorded a run time of %s", ran)
		}
		return
	}
	if ran < atLeast || ran >= under {
		t.Errorf("recorded run time = %s, want at least %s and under %s", ran, atLeast, under)
	}
}

// What the wrapper records of a run (#832, #838; classifyTimeout argues its
// weight), pinned on the host's shell like the mark. Both bounds matter: one
// too short hides a timeout, one too long reports a timeout that never
// happened.
//
// The command's environment plants every reading name, as a tenant's Spec.Env
// could. A reading that failed must not fall back on them — on a host without
// /proc/uptime that would record 899s — and the wrapper's own readings must not
// reach the command.
func TestExecWrapperRecordsHowLongTheCommandRan(t *testing.T) {
	env := setsidEnv(t)
	if env == nil {
		env = os.Environ()
	}
	state := t.TempDir() + "/state"
	wrapper := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", `echo "$t0 $tw $t1"; sleep 0.3; exit 7`, "30", state)
	wrapper.Env = append(append([]string{}, env...), "t0=100.00", "tw=100.00", "t1=999.00")
	stdout, err := wrapper.Output()
	if err != nil {
		t.Fatalf("run execWrapper: %v", err)
	}
	if got, want := strings.TrimSpace(string(stdout)), "100.00 100.00 999.00"; got != want {
		t.Errorf("the command saw t0 tw t1 = %q, want the %q its environment carried — the wrapper's readings leaked into it", got, want)
	}
	out, err := exec.Command("/bin/bash", "-c", exitScript, "map-exit", state).Output()
	if err != nil {
		t.Fatalf("run exitScript: %v", err)
	}
	code, killed, rec, err := parseExit(string(out))
	if err != nil || code != 7 || killed {
		t.Fatalf("parseExit(%q) = %d, %v, %+v, %v; want 7, false", out, code, killed, rec, err)
	}
	// At least half the 300ms the command slept: each record misses the gap
	// between the command's launch and its first reading, which is microseconds
	// unless the wrapper is descheduled right then — half the sleep would take a
	// host stalled that long. Under a second: what the record adds beyond the
	// sleep is a reap, single-digit milliseconds on an idle host, so 700ms of
	// headroom is load, not slack for a record that counts something it should
	// not.
	wantRan(t, rec.sinceLaunch, 150*time.Millisecond, time.Second)
	wantRan(t, rec.sinceWatchdog, 150*time.Millisecond, time.Second)
}

// Each record's first reading is taken where its question needs it
// (classifyTimeout states what each place bounds).
//
// The overrun's, right after the command's launch: after it, so nothing the
// wrapper sets reaches the command, and before anything else the wrapper does,
// so what it misses of the command's run is the launch gap alone. Behaviourally,
// a wrapper held up after the launch does not shrink it. A FIFO planted at the
// pid file holds the wrapper at its first blocking step — writing the pid — for
// half a second of the command's one-second run; a record that started after
// that write would come to half a second, one that starts at the launch keeps
// the whole run. (The command outlives the hold on purpose: its exit would
// interrupt the blocked write, which macOS's bash 3.2 then abandons rather than
// retries.)
//
// The kill's, right after the watchdog's launch, which that same hold delays:
// so the hold comes out of it, and it counts no time before the watchdog could
// have been counting too. The steps between each launch and its reading are a
// fork and a builtin no host test can stretch, so the script's own order is
// asserted too.
func TestExecWrapperStartsEachRecordAtItsLaunch(t *testing.T) {
	launch := strings.Index(execWrapper, `setsid /bin/bash -c "$1"`)
	reading := strings.Index(execWrapper, "read -r t0 ")
	watchdog := strings.Index(execWrapper, ") >/dev/null 2>&1 3>&- &")
	watchdogReading := strings.Index(execWrapper, "read -r tw ")
	reap := strings.Index(execWrapper, `wait "$cmd"`)
	if launch < 0 || reading < 0 || watchdog < 0 || !(launch < reading && reading < watchdog) {
		t.Errorf("execWrapper takes its first reading at %d; want it after the command's launch (%d) and before the watchdog's (%d)",
			reading, launch, watchdog)
	}
	if watchdogReading < 0 || reap < 0 || !(watchdog < watchdogReading && watchdogReading < reap) {
		t.Errorf("execWrapper takes its watchdog's reading at %d; want it after the watchdog's launch (%d) and before the reap (%d)",
			watchdogReading, watchdog, reap)
	}

	env := setsidEnv(t)
	state := t.TempDir() + "/state"
	if err := syscall.Mkfifo(state+".pid", 0o644); err != nil {
		t.Fatalf("plant a fifo at the pid file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wrapper := exec.CommandContext(ctx, "/bin/bash", "-c", execWrapper, "map-exec", "sleep 1", "30", state)
	if env != nil {
		wrapper.Env = env
	}
	if err := wrapper.Start(); err != nil {
		t.Fatalf("start execWrapper: %v", err)
	}
	const held = 500 * time.Millisecond
	time.Sleep(held)
	opened := make(chan error, 1)
	go func() {
		// Blocks until the wrapper's write opens the other end, which it is
		// already waiting to do.
		f, err := os.Open(state + ".pid")
		if err == nil {
			_, err = io.ReadAll(f)
			_ = f.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("release the wrapper's pid write: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the wrapper never wrote its pid")
	}
	if err := wrapper.Wait(); err != nil {
		t.Fatalf("run execWrapper: %v", err)
	}
	out, err := exec.Command("/bin/bash", "-c", exitScript, "map-exit", state).Output()
	if err != nil {
		t.Fatalf("run exitScript: %v", err)
	}
	code, killed, rec, err := parseExit(string(out))
	if err != nil || code != 0 || killed {
		t.Fatalf("parseExit(%q) = %d, %v, %+v, %v; want 0, false", out, code, killed, rec, err)
	}
	wantRan(t, rec.sinceLaunch, time.Second-held/2, 2*time.Second)
	wantRan(t, rec.sinceWatchdog, 0, time.Second-held/2)
}

// The watchdog must not still be holding the exec's stderr when the command has
// exited: a timed command would then return a poll interval late, every time.
// Nothing in that is Kubernetes-specific — it is a property of the script's file
// descriptors — so it is pinned here, on the host's shell, in milliseconds and
// without a cluster in the measurement.
//
// An os.Pipe stands in for the exec's stderr because that is what it is: one
// descriptor several processes in the pod inherit, which EOFs when the last of
// them closes it — the kubelet copies until exactly that moment. cmd.Stderr
// being an *os.File hands the child that descriptor directly, with no copying
// goroutine in between, so Run returns when the wrapper exits and the parent's
// own copy is then the only one left to close. A read that EOFs immediately
// after means nothing inside outlived the command; one that blocks means a
// straggler — the watchdog, asleep in its `sleep 1` — is still holding it.
//
// The bound is not a load measurement: by the time Run has returned, either the
// pipe is already EOF or a live process is holding it, and 300ms is only the
// margin for this process to be scheduled. The regression it separates from is a
// whole poll interval, ~1s.
func TestExecWrapperReleasesTheStreamWhenTheCommandExits(t *testing.T) {
	env := setsidEnv(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()

	// The command writes to stderr so the pipe is proven to be the stream the
	// wrapper hands the command, rather than a descriptor nothing ever reached —
	// a pipe wired to nothing would EOF promptly for the wrong reason. Its
	// deadline is long enough that the watchdog is still on its first sleep when
	// the command exits, which is the moment under test.
	//
	// It outlives the watchdog's first `kill -0` on purpose. Without that, the
	// command could exit before the watchdog ever looked, the watchdog would find
	// it already gone and leave — and the row would pass whether or not the
	// descriptor was closed, which is a guard that proves nothing rather than a
	// flake. A fifth of a second is far past the microseconds that first poll
	// takes and far short of the poll interval the regression waits out.
	cmd := exec.Command("/bin/bash", "-c", execWrapper, "map-exec", "echo hi >&2; sleep 0.2", "30", t.TempDir()+"/state")
	cmd.Stderr = w
	if env != nil {
		cmd.Env = env
	}
	if err := cmd.Run(); err != nil {
		_ = w.Close()
		t.Fatalf("run execWrapper: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close the parent's end: %v", err)
	}

	if err := r.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Errorf("the exec's stderr did not EOF once the command exited (%v) — the watchdog is holding it open", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hi" {
		t.Errorf("the command's stderr = %q, want %q — it never reached the stream this asserts on", got, "hi")
	}
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
		if code, killed, _, err := parseExit(run(t, state)); err != nil || code != sigkillExit || !killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
	})

	t.Run("FinishedOnItsOwn", func(t *testing.T) {
		state := stage(t, "clean", "0\n", false)
		if code, killed, _, err := parseExit(run(t, state)); err != nil || code != 0 || killed {
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
		if code, killed, _, err := parseExit(run(t, state)); err != nil || code != sigkillExit || killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, false, nil — a fifo is not the watchdog's mark",
				code, killed, err, sigkillExit)
		}
	})

	// The wrapper never recorded anything, and the cleanup still has to run.
	t.Run("NothingRecorded", func(t *testing.T) {
		state := stage(t, "sabotaged", "", false)
		if code, killed, _, err := parseExit(run(t, state)); err != nil || code != sigkillExit || killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, false, nil", code, killed, err, sigkillExit)
		}
	})

	// The same sabotage, but the watchdog did fire before the wrapper died. The
	// mark is the only witness left, and it is read here rather than by the
	// wrapper precisely so this case is not lost.
	t.Run("NothingRecordedButMarked", func(t *testing.T) {
		state := stage(t, "sabotaged-marked", "", true)
		if code, killed, _, err := parseExit(run(t, state)); err != nil || code != sigkillExit || !killed {
			t.Errorf("parseExit = %d, %v, %v; want %d, true, nil", code, killed, err, sigkillExit)
		}
	})
}

// The provider's probes run as framed scripts (sandbox.Frame) and read their
// answers from between the lines, so an image's BASH_ENV file — printing on
// both streams without ending its lines, leaving the directory, and setting an
// EXIT trap that prints after the script — answers none of them (#860). Before
// the frame its banner was read as the exit record's first field, which failed
// every exec on the image, and as the liveness verdict, which read every
// command as dead; and the refused write's reason carried it. Each runs here
// through the host's shell with sandboxtest.BannerHook as its BASH_ENV file, framed as the
// provider frames it; the live hooked test (internal/sandbox/hookedtest) runs
// them in a pod.
func TestProbesReadTheirAnswersThroughAStartupFile(t *testing.T) {
	env := hookedEnv(t, gnuStatEnv(t))
	dir := t.TempDir()
	run := func(t *testing.T, f sandbox.Frame, shell, script string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(shell, append([]string{"-c", f.Wrap(script), "map-test"}, args...)...)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout = &out
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run %s: %v", shell, err)
			}
			code = ee.ExitCode()
		}
		if shell == "/bin/bash" && (!strings.HasPrefix(out.String(), "welcome to the image ") || !strings.HasSuffix(out.String(), "exit banner ")) {
			t.Fatalf("stdout %q; want the hook's banner before the frame and its trap's words after it", out.String())
		}
		return out.String(), code
	}

	t.Run("liveness", func(t *testing.T) {
		gone := exec.Command("true")
		if err := gone.Run(); err != nil {
			t.Fatal(err)
		}
		for pid, want := range map[int]bool{os.Getpid(): true, gone.Process.Pid: false} {
			state := dir + "/alive-" + strconv.Itoa(pid)
			if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			f := sandbox.NewFrame("alive")
			out, code := run(t, f, "/bin/bash", aliveScript, state)
			if alive, err := aliveVerdict(f, out, false); code != 0 || err != nil || alive != want {
				t.Errorf("pid %d: alive = %v, %v (exit %d); want %v", pid, alive, err, code, want)
			}
		}
	})

	t.Run("exit record", func(t *testing.T) {
		state := dir + "/exit"
		if err := os.WriteFile(state+".exit", []byte("7 10.00 10.50 12.50\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(state+".killed", 0o755); err != nil {
			t.Fatal(err)
		}
		f := sandbox.NewFrame("exit")
		out, _ := run(t, f, "/bin/bash", exitScript, state)
		code, killed, rec, err := readExitRecord(f, out, false, 0)
		if want := (runRecord{sinceLaunch: 2500 * time.Millisecond, sinceWatchdog: 2 * time.Second}); err != nil || code != 7 || !killed || rec != want {
			t.Errorf("readExitRecord = %d, %v, %+v, %v; want 7, true, %+v", code, killed, rec, err, want)
		}
		// What the frame keeps out: read whole, the banner is the record's
		// first field.
		if _, _, _, err := parseExit(out); err == nil {
			t.Errorf("parseExit read %q, banner and all, as a record", out)
		}
	})

	t.Run("export probe", func(t *testing.T) {
		if err := os.Mkdir(dir+"/present", 0o755); err != nil {
			t.Fatal(err)
		}
		for root, want := range map[string]string{dir + "/present": "P", dir + "/absent/deeper": "M"} {
			for _, shell := range []string{"sh", "/bin/bash"} {
				f := sandbox.NewFrame("export")
				out, _ := run(t, f, shell, exportProbe, root)
				if answer, framed, short := f.Cut(out, false); !framed || short || strings.TrimSpace(answer) != want {
					t.Errorf("%s: probe %s = %q, %v, %v; want %s", shell, root, answer, framed, short, want)
				}
			}
		}
	})

	t.Run("write refusal reason", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the write bit, so the create is not refused")
		}
		locked := dir + "/locked"
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		f := sandbox.NewFrame("write")
		path := locked + "/f"
		out, code := run(t, f, "/bin/bash", writeScript, path, locked, "0", gopath.Join(locked, sandbox.TempName()))
		reason, framed, _ := f.Cut(out, false)
		if code != sandbox.ExitPathNotWritable || !framed || reason != "Permission denied" {
			t.Errorf("exit %d, reason %q (framed %v); want %d and the strerror alone", code, reason, framed, sandbox.ExitPathNotWritable)
		}
	})
}

// readExitRecord reads what reached the output of exitScript's frame: a record
// whose end line a lost stream dropped reads as far as it got; a stream with
// no begin line is a lost answer — no record, the kill's code, as an empty
// stream always was — when nothing else reached it, or when it ends partway
// through the begin line, past its newline, after a banner or not; one with
// something else and no begin line — a banner's ended line among it — or of a
// reader that exited non-zero with no frame is no record to parse; and one the
// cap cut before the record's end line — before its begin line, whatever it
// ends in, or inside the record, where it may have cut a number — is the
// startup's failure, not a record.
func TestReadExitRecordReadsInsideTheFrame(t *testing.T) {
	f := sandbox.NewFrame("exit")
	begin, end := f.Lines()
	// Another frame's begin line: this one's but for the nonce's first digit.
	other := "\nmap-exit-begin-1"
	if strings.HasPrefix(begin, other) {
		other = "\nmap-exit-begin-0"
	}
	// whole is the record `1.0 1.5 2.0` makes.
	whole := runRecord{sinceLaunch: time.Second, sinceWatchdog: 500 * time.Millisecond}
	for _, c := range []struct {
		name, out string
		truncated bool
		// exit is the reader's own exit status.
		exit   int
		code   int
		killed bool
		rec    runRecord
		fails  bool
		// startup is a failure the cap made: a *sandbox.StartupOutputError
		// whose command had run.
		startup bool
	}{
		{"whole, banner and trap around it", "welcome 3 " + begin + "K 0 1.0 1.5 2.0\n" + end + "exit 9", false, 0, 0, true, whole, false, false},
		{"end line lost", begin + "K 0 1.0 1.5 2.0\n", false, 0, 0, true, whole, false, false},
		{"end line half lost", begin + " 5 1.0 1.5 2.0\n" + end[:6], false, 0, 5, false, whole, false, false},
		{"the record's tail lost", begin + " 5 1.0 1.5", false, 0, 5, false, runRecord{}, false, false},
		{"nothing after the begin line", begin, false, 0, sigkillExit, false, runRecord{}, false, false},
		// A reader the stream lost, framed or not, killed on its way out.
		{"the record whole, the reader killed", begin + "K 0 1.0 1.5 2.0\n", false, 137, 0, true, whole, false, false},
		{"nothing at all", "", false, 0, sigkillExit, false, runRecord{}, false, false},
		{"cut inside the begin line", begin[:9], false, 0, sigkillExit, false, runRecord{}, false, false},
		{"cut inside the begin line, after a banner", "welcome 0 1.0 1.5 2.0" + begin[:len(begin)-1], false, 0, sigkillExit, false, runRecord{}, false, false},
		{"cut two bytes into the begin line, after a banner", "welcome 0 1.0 1.5 2.0" + begin[:2], false, 0, sigkillExit, false, runRecord{}, false, false},
		{"something, but no begin line", "welcome 0 1.0 1.5 2.0", false, 0, 0, false, runRecord{}, true, false},
		{"something, and another frame's begin line cut short", "welcome 0 1.0 1.5 2.0" + other, false, 0, 0, false, runRecord{}, true, false},
		// A banner that ended its line, then nothing: a newline alone is no
		// part of a begin line, and the stream is the banner's.
		{"a banner, its line ended, then nothing", "welcome 0 1.0 1.5 2.0\n", false, 0, 0, false, runRecord{}, true, false},
		// A reader that exited non-zero with no frame — a startup that failed
		// under errexit before the script began — is no record, whatever it
		// printed: not the kill's 137 for a command that may have exited 7.
		{"a banner, then the reader exits 1", "welcome\n", false, 1, 0, false, runRecord{}, true, false},
		{"nothing, the reader exits 1", "", false, 1, 0, false, runRecord{}, true, false},
		{"partway into the begin line, the reader exits 1", "welcome" + begin[:9], false, 1, 0, false, runRecord{}, true, false},
		// A startup that floods past the cap pushes the record out: the
		// startup's failure, though its tail ends in a newline, as a lost
		// begin line's would.
		{"a flood the cap cut before any begin line", strings.Repeat("y\n", 1000), true, 0, 0, false, runRecord{}, true, true},
		// One the cap cut after the begin line may have cut a number: a
		// deadline's kill, K 137, kept as K 13 is no exit 13.
		{"a record the cap cut inside its code", "flood " + begin + "K 13", true, 0, 0, false, runRecord{}, true, true},
		{"a record the cap cut before its end line", "flood " + begin + "K 137 12.3 12.4 15.9\n", true, 0, 0, false, runRecord{}, true, true},
		{"a record the cap cut inside its end line", "flood " + begin + "K 137 12.3 12.4 15.9\n" + end[:len(end)/2], true, 0, 0, false, runRecord{}, true, true},
		// One the cap cut only past its end line is whole: an EXIT trap's flood.
		{"a record the cap cut past its end line", begin + "K 137 12.3 12.4 15.9\n" + end + "exit flood", true, 0, sigkillExit, true, runRecord{sinceLaunch: 3600 * time.Millisecond, sinceWatchdog: 3500 * time.Millisecond}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, killed, rec, err := readExitRecord(f, c.out, c.truncated, c.exit)
			if (err != nil) != c.fails || !c.fails && (code != c.code || killed != c.killed || rec != c.rec) {
				t.Errorf("readExitRecord(%q) = %d, %v, %+v, %v; want %d, %v, %+v, failing %v", c.out, code, killed, rec, err, c.code, c.killed, c.rec, c.fails)
			}
			var startup *sandbox.StartupOutputError
			if got := errors.As(err, &startup) && startup.Ran; got != c.startup {
				t.Errorf("readExitRecord(%q) = %v; a StartupOutputError of a command that ran: %v, want %v", c.out, err, got, c.startup)
			}
		})
	}
}

// aliveVerdict is the liveness probe's answer only when it reached the output
// whole: a verdict the stream or the cap cut short, or none, is an error,
// which the probe's callers read as still running.
func TestAliveVerdictNeedsTheWholeFrame(t *testing.T) {
	f := sandbox.NewFrame("alive")
	begin, end := f.Lines()
	for _, c := range []struct {
		out       string
		truncated bool
		alive     bool
		fails     bool
	}{
		{"D " + begin + "A\n" + end + "D", false, true, false},
		{"A " + begin + "D\n" + end + "A", false, false, false},
		{begin + "A\n", false, false, true},
		{begin + "A", true, false, true},
		{"A", false, false, true},
	} {
		if alive, err := aliveVerdict(f, c.out, c.truncated); (err != nil) != c.fails || alive != c.alive {
			t.Errorf("aliveVerdict(%q, %v) = %v, %v; want %v, failing %v", c.out, c.truncated, alive, err, c.alive, c.fails)
		}
	}
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

// mintRecorder records the mint-on-create and revoke-on-dismantle seams' calls
// for the gated tests.
type mintRecorder struct {
	generated  int
	persisted  []string // tokens handed to Persist
	persistErr error
	revoked    []domain.ID // sessions handed to Revoke
}

func (m *mintRecorder) Generate() string { m.generated++; return "gtk_unit_test_token" }

func (m *mintRecorder) Persist(_ context.Context, _ domain.ID, token string) error {
	if m.persistErr != nil {
		return m.persistErr
	}
	m.persisted = append(m.persisted, token)
	return nil
}

func (m *mintRecorder) Revoke(_ context.Context, sid domain.ID) error {
	m.revoked = append(m.revoked, sid)
	return nil
}

// revokerFunc adapts a func to sandbox.GateTokenRevoker, so a test can observe
// the cluster's state at the moment a revoke lands (the ordering guarantee).
type revokerFunc func(context.Context, domain.ID) error

func (f revokerFunc) Revoke(ctx context.Context, sid domain.ID) error { return f(ctx, sid) }

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
// gated create and revoking only for the ungated dismantle (#197).
func TestProvisionGateShapeMismatchRebuilds(t *testing.T) {
	m := &mintRecorder{}
	sid := domain.ID("sesn_reshape")
	p := fakeProvider(readyPod(sid)) // pre-gate shape, ready
	markPodsReadyOnCreate(p)
	spec := sandbox.Spec{SessionID: sid, Image: "img",
		Networking: domain.Networking{Type: domain.NetLimited}, Gate: gateSpecFixture(m),
		GateTokenRevoker: m}
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
	if len(m.revoked) != 0 {
		t.Errorf("gated reshape revoked %v; replacement is Ensure's revoke-on-re-mint, not Revoke", m.revoked)
	}

	// The reverse: the session no longer wants a gate — the gated pod is
	// replaced by a plain one, with no further minting, and the dismantled
	// gate's token is revoked exactly once (no replacement will re-mint it),
	// before the pod teardown — the ordering that lets a failed teardown
	// retry both instead of losing the trigger.
	revokes := 0
	specUngated := sandbox.Spec{SessionID: sid, Image: "img",
		GateTokenRevoker: revokerFunc(func(ctx context.Context, got domain.ID) error {
			if got != sid {
				t.Errorf("revoked %s, want %s", got, sid)
			}
			if _, gerr := p.client.cs.CoreV1().Pods("default").Get(ctx, podName(sid), metav1.GetOptions{}); gerr != nil {
				t.Errorf("revoke arrived with the gated pod already gone (%v); it must precede the teardown", gerr)
			}
			revokes++
			return nil
		})}
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
	if revokes != 1 {
		t.Errorf("ungated reshape revoked %d times, want exactly once", revokes)
	}
	if len(m.revoked) != 0 {
		t.Errorf("the gated spec's revoker was called (%v); only the ungated reshape revokes", m.revoked)
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

// refusedBulk asks the pod with the batch's own bookkeeping in argv — the
// manifest as $1 — and Refusal turns what the script answers into the caller's
// error. The exec is faked in two halves, because client-go's SPDY stream has no
// seam to fake it whole: an API server that records the argv and refuses the
// upgrade (refusedBulk then answers nil, and the caller keeps its own error),
// and that same argv run under a real bash on a staged batch, standing in for
// the pod. Its answer is the single write's: a target that is a directory.
func TestTheRefusedBulkScriptAnswersInTheSingleWritesTerms(t *testing.T) {
	var mu sync.Mutex
	var argvs [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			mu.Lock()
			argvs = append(argvs, r.URL.Query()["command"])
			mu.Unlock()
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	rc := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("build the clientset: %v", err)
	}
	dir := t.TempDir()
	pd := &pod{client: &client{cs: cs, rest: rc, namespace: "default"}, name: "map-sesn-x", workdir: dir}

	if err := os.MkdirAll(dir+"/adir", 0o755); err != nil {
		t.Fatalf("stage a directory target: %v", err)
	}
	b, err := sandbox.NewBulkWrite(dir, []sandbox.FileWrite{
		{Path: dir + "/fine.txt", Data: []byte("x")},
		{Path: dir + "/adir", Data: []byte("clobber")},
	})
	if err != nil {
		t.Fatalf("NewBulkWrite: %v", err)
	}
	// What the delivery landed before it was refused: the bookkeeping.
	var archive bytes.Buffer
	if err := b.Archive(&archive); err != nil {
		t.Fatalf("build the archive: %v", err)
	}
	tr := tar.NewReader(&archive)
	for _, path := range []string{b.Manifest, b.DirList} {
		if _, err := tr.Next(); err != nil {
			t.Fatalf("read the bookkeeping: %v", err)
		}
		data, _ := io.ReadAll(tr)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("stage %s: %v", path, err)
		}
	}

	if err := pd.refusedBulk(context.Background(), b); err != nil {
		t.Errorf("refusedBulk over an exec that could not run = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(argvs) != 1 {
		t.Fatalf("the pod was asked %d times, want once", len(argvs))
	}
	want := []string{"/bin/bash", "-c", sandbox.Script(bulkRefusedScript), "map-bulk-write", b.Manifest, b.DirList}
	if !slices.Equal(argvs[0], want) {
		t.Fatalf("the pod was asked to run %q, want the refused script over the batch's bookkeeping", argvs[0])
	}

	var stderr bytes.Buffer
	cmd := exec.Command(argvs[0][0], argvs[0][1:]...)
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run the refused script: %v", err)
		}
		code = ee.ExitCode()
	}
	if code != sandbox.ExitPathIsDirectory {
		t.Fatalf("the script exited %d, want ExitPathIsDirectory (%d); stderr: %s",
			code, sandbox.ExitPathIsDirectory, stderr.String())
	}
	if err := b.Refusal("k8s", code, stderr.String()); !errors.Is(err, sandbox.ErrIsDirectory) ||
		!strings.Contains(err.Error(), dir+"/adir") {
		t.Errorf("Refusal = %v, want ErrIsDirectory naming %s/adir", err, dir)
	}
}

// The write script spells the unreplaceable refusal as a literal, and no
// unprivileged host test can make the real script take that branch — the
// temporary file lands in the target's own directory, and a device node's or
// mount point's parent is not writable without root. So the literal is pinned
// to its constant by text instead: renumbering ExitPathNotReplaceable fails
// here rather than in a live session. The probe's own answers are pinned
// against a real shell in the shared package's TestUnreplaceableShell.
//
// The probe is asked twice — ahead of the temporary file (drained, so a
// refused body cannot deadlock the exec stream, #303) and again after the
// body has landed, ahead of the rename (the freshness a pre-transfer answer
// cannot give — no drain, `tee` has consumed the body by then). The order is
// pinned along with the spellings: a race between the two asks is not
// something a contract row can stage deterministically, so this text is the
// guard against either ask quietly moving or vanishing.
func TestWriteScriptSpellsTheUnreplaceableExit(t *testing.T) {
	early := `if __map_unreplaceable "$1"; then cat >/dev/null; exit ` +
		strconv.Itoa(sandbox.ExitPathNotReplaceable) + `; fi`
	late := `if __map_unreplaceable "$1"; then rm -f "$4"; exit ` +
		strconv.Itoa(sandbox.ExitPathNotReplaceable) + `; fi`
	ei := strings.Index(writeScript, early)
	li := strings.Index(writeScript, late)
	if ei < 0 || li < 0 {
		t.Fatalf("writeScript must spell both unreplaceable refusals — early (drained) %q at %d, late (pre-rename) %q at %d — a missing one drifted from sandbox.ExitPathNotReplaceable or lost its ask", early, ei, late, li)
	}
	ti := strings.Index(writeScript, `tee "$4"`)
	mi := strings.Index(writeScript, `mv -f "$4"`)
	if !(ei < ti && ti < li && li < mi) {
		t.Fatalf("the early ask must sit ahead of tee and the late ask between tee and the rename (early %d, tee %d, late %d, mv %d) — the reorder is what keeps the refusal both reachable under a read-only root and fresh at the move", ei, ti, li, mi)
	}
}

// Every exit the write script can take with the body still unread must drain
// it first: an undrained early exit only races its code home for bodies small
// enough to already be in flight, and beyond the exec stream's flow-control
// window the call deadlocks until the pod dies (#303 drained the refusal
// branches; #304 is the failure branches doing the same). Unlike the refusal
// branches, all three failure branches can be staged from the host's bash, so
// the drain is pinned by behavior rather than by literal: the script reads
// from a pipe whose producer is the verdict. `head -c 1M /dev/zero` blocks
// once the pipe buffer fills, so a script that exits with the body unread
// kills the producer with SIGPIPE (exit 141), where a script that drained
// lets it finish (exit 0).
func TestWriteScriptDrainsEveryEarlyExit(t *testing.T) {
	// drainRun mirrors TestWriteScriptVerifiesDeliveredLength's runScript, but
	// feeds the script through the pipe and reports both ends — the script's
	// exit code and the producer's — plus whatever the script printed, which
	// is how a classified refusal carries its reason out (plan 23).
	drainRun := func(t *testing.T, env []string, path, tmp string) (script, producer int, reason string) {
		t.Helper()
		f := gopath.Join(t.TempDir(), "writescript.sh")
		if err := os.WriteFile(f, []byte(writeScript), 0o755); err != nil {
			t.Fatalf("stage the script: %v", err)
		}
		outFile := gopath.Join(t.TempDir(), "stdout")
		cmd := exec.Command("/bin/bash", "-c",
			`head -c 1048576 /dev/zero | /bin/bash "$1" "$2" "$3" "$4" "$5" > "$6"; echo "${PIPESTATUS[0]} ${PIPESTATUS[1]}"`,
			"map-drain", f, path, gopath.Dir(path), "1048576", tmp, outFile)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run writeScript through the pipe: %v", err)
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) != 2 {
			t.Fatalf("PIPESTATUS report = %q, want two fields", out)
		}
		producer, err = strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("producer exit %q: %v", fields[0], err)
		}
		script, err = strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("script exit %q: %v", fields[1], err)
		}
		ob, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("read the script's stdout: %v", err)
		}
		return script, producer, string(ob)
	}
	gone := func(t *testing.T, p string) {
		t.Helper()
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived (%v), want it removed", p, err)
		}
	}

	// The `mkdir -p` failure: a regular file blocking the parent path fails it
	// with ENOTDIR for root and non-root alike, and __map_path_fault classifies.
	t.Run("PathBlockedByAFile", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/plain", []byte("in the way"), 0o644); err != nil {
			t.Fatalf("stage the blocker: %v", err)
		}
		path := dir + "/plain/deeper/child"
		script, producer, _ := drainRun(t, nil, path, gopath.Join(gopath.Dir(path), sandbox.TempName()))
		if script != sandbox.ExitPathNotDirectory {
			t.Errorf("script exit %d, want %d (ExitPathNotDirectory)", script, sandbox.ExitPathNotDirectory)
		}
		if producer != 0 {
			t.Errorf("producer exit %d, want 0 — the classification exited with the body unread", producer)
		}
	})

	// The `: >` failure: a temporary whose name an existing directory already
	// holds cannot be created, for root and non-root alike (EISDIR), which is
	// the same branch an unwritable or full parent takes. The refusal is
	// classified (plan 23): exit 20, with the shell's own strerror text as
	// the reason on stdout — the sandbox's equivalent of the errno the
	// reference toolset maps.
	//
	// The `mkdir -p` branch has no sibling row here on purpose: its failure is
	// EROFS, which cannot be staged inside a host tempdir. Its cover is the
	// cross-backend contract row, which runs against a real read-only root
	// (sandboxtest/contract.go) — nothing here asserts on that branch.
	t.Run("TemporaryCannotBeCreated", func(t *testing.T) {
		dir := t.TempDir()
		tmp := gopath.Join(dir, sandbox.TempName())
		if err := os.Mkdir(tmp, 0o755); err != nil {
			t.Fatalf("stage the squatting directory: %v", err)
		}
		script, producer, reason := drainRun(t, nil, dir+"/target", tmp)
		if script != sandbox.ExitPathNotWritable {
			t.Errorf("script exit %d, want %d (ExitPathNotWritable)", script, sandbox.ExitPathNotWritable)
		}
		if producer != 0 {
			t.Errorf("producer exit %d, want 0 — the failed create exited with the body unread", producer)
		}
		if reason != "Is a directory" {
			t.Errorf("reason = %q, want the shell's own %q", reason, "Is a directory")
		}
	})

	// The mid-stream `tee` failure: a shimmed tee that dies after consuming a
	// slice of the body, as a real one does when the filesystem fills under it.
	// The branch must shed the temporary and then drain what tee left.
	t.Run("TeeDiesMidStream", func(t *testing.T) {
		bin := t.TempDir()
		if err := os.WriteFile(bin+"/tee", []byte("#!/bin/sh\nhead -c 1024 >/dev/null\nexit 1\n"), 0o755); err != nil {
			t.Fatalf("stage the tee shim: %v", err)
		}
		env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		dir := t.TempDir()
		tmp := gopath.Join(dir, sandbox.TempName())
		script, producer, _ := drainRun(t, env, dir+"/target", tmp)
		if script != 1 {
			t.Errorf("script exit %d, want 1", script)
		}
		if producer != 0 {
			t.Errorf("producer exit %d, want 0 — the failed stream exited with its remainder unread", producer)
		}
		gone(t, tmp)
	})
}

// A write with nothing to write opens no stdin stream (#318). The stall it
// removes is the cluster's, so no cluster can be made to stage it; what this
// pins is the request the provider makes, which is the whole of its side of the
// bargain. Same fake API server as the shed row below, and for the same reason:
// an exec is not a typed call, so only the server the SPDY executor dials can
// see what was asked. The upgrade it refuses is immaterial — the question is
// what the query said before the refusal.
//
// The one-byte write is not decoration: without it this row passes just as well
// against a provider that never asks for stdin at all, which would strand every
// write that has bytes to deliver.
// execsAsked stands an API server up in front of a pod and answers what its
// execs asked for. Every request is refused, which is immaterial: the question
// these rows put is what the query said before the refusal, and a refusal is the
// cheapest way to reach it without a cluster. The returned func collects the
// `stdin` parameter of each exec, in order.
func execsAsked(t *testing.T) (*pod, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			mu.Lock()
			asked = append(asked, r.URL.Query().Get("stdin"))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	rc := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("build the clientset: %v", err)
	}
	pd := &pod{
		client:  &client{cs: cs, rest: rc, namespace: "default"},
		name:    "map-sesn-x",
		workdir: sandbox.DefaultWorkdir,
	}
	return pd, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func TestAnEmptyWriteAsksForNoStdinStream(t *testing.T) {
	pd, execs := execsAsked(t)
	ctx := context.Background()
	// Both fail on the refused upgrade; what they asked for is already recorded.
	_ = pd.WriteFile(ctx, sandbox.DefaultWorkdir+"/empty.txt", nil)
	_ = pd.WriteFile(ctx, sandbox.DefaultWorkdir+"/one.txt", []byte("x"))

	asked := execs()
	if len(asked) != 2 {
		t.Fatalf("the pod was asked to exec %d times, want 2 (one write each)", len(asked))
	}
	if asked[0] == "true" {
		t.Error("a zero-byte write asked for a stdin stream — nothing will ever be written to it, and the pod's side of that exec is what #318 hung on")
	}
	if asked[1] != "true" {
		t.Errorf("a one-byte write asked stdin=%q, want true — its bytes travel on that stream", asked[1])
	}
}

// The stream a zero-byte write does not open is also the thing that would have
// counted its bytes, so a caller whose size disagrees with its reader has to be
// refused before the write starts rather than by the count that no longer
// happens. `size must equal the number of bytes src yields: a short or long
// stream is an error, not a silently truncated file` — internal/sandbox's own
// contract, which the docker backend keeps through its tar writer, and which
// this would otherwise answer by landing an empty file over the target.
//
// The pod is never asked, which is the half that matters beyond the error: the
// refusal has to leave the target holding what it held, and it can only promise
// that by happening before anything in the pod is created.
func TestAZeroSizedWriteWithBytesToDeliverIsRefused(t *testing.T) {
	pd, execs := execsAsked(t)
	path := sandbox.DefaultWorkdir + "/mismatch.txt"
	err := pd.WriteFileStream(context.Background(), path, strings.NewReader("x"), 0)
	if err == nil || !strings.Contains(err.Error(), "short write") {
		t.Errorf("WriteFileStream(size 0, one byte to read) = %v, want the short-write refusal", err)
	}
	if asked := execs(); len(asked) != 0 {
		t.Errorf("the pod was asked to exec %d times for a write that could not be honoured — the target must keep what it held", len(asked))
	}
}

// A batch that failed *because* its caller went away is the one whose residue
// most needs shedding — up to ten thousand members' bytes sitting in the pod
// until it dies — so the shed runs off the caller's context, on a budget of its
// own (#316). The docker backend's own cleanups were detached for this reason in
// #310; this is its k8s twin, and it is a behavioural claim rather than a
// cosmetic one: with the caller's cancellation inherited, client-go fails the
// request before it is written and the pod is never asked to shed at all.
//
// The clientset fake cannot see this. An exec is not a typed call — client.exec
// builds a SPDY executor over the REST config — so no reactor is ever consulted
// and no fake action is recorded. What can see it is the API server the executor
// dials, so the row stands one up and watches for the request. The upgrade it
// refuses is immaterial: the shed ignores its own failure, and what is asserted
// is that the pod was asked.
func TestTheBatchesShedOutlivesTheCallerThatWentAway(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			mu.Lock()
			commands = append(commands, strings.Join(r.URL.Query()["command"], " "))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rc := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("build the clientset: %v", err)
	}
	pd := &pod{
		client:  &client{cs: cs, rest: rc, namespace: "default"},
		name:    "map-sesn-x",
		workdir: sandbox.DefaultWorkdir,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Driven through WriteFiles rather than at discardBulk directly, because
	// detaching the shed's context is worth nothing if the cancelled path never
	// reaches it — and it did not: a caller that goes away mid-delivery returns
	// from deliverBulk's error branch, which shed nothing at all.
	if err := pd.WriteFiles(ctx, []sandbox.FileWrite{
		{Path: sandbox.DefaultWorkdir + "/a.txt", Data: []byte("x")},
	}); err == nil {
		t.Fatal("WriteFiles returned nil for a caller that was already gone")
	}

	mu.Lock()
	defer mu.Unlock()
	// One request, and it is the shed: the delivery's own exec never reaches the
	// pod, its context being dead before the request is written — which is
	// exactly what makes the shed's detachment the only reason anything arrives.
	if len(commands) != 1 {
		t.Fatalf("the pod was asked %d times (%q), want the shed to have run on a context the caller already canceled",
			len(commands), commands)
	}
	if !strings.Contains(commands[0], "__map_bulk_discard") {
		t.Errorf("the exec ran %q, want the batch's own shed", commands[0])
	}
	if !strings.Contains(commands[0], sandbox.TempPrefix) {
		t.Errorf("the exec ran %q, want it given a batch's bookkeeping paths", commands[0])
	}
}
