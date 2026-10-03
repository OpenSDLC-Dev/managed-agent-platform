package sandboxtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// KubeContext is the kube context the Kubernetes tests run against —
// MAP_K8S_CONTEXT when set (local), otherwise the kubeconfig's current context
// (CI, where the kind-action sets it).
func KubeContext(t testing.TB) string {
	t.Helper()
	if ctx := os.Getenv("MAP_K8S_CONTEXT"); ctx != "" {
		return ctx
	}
	cfg, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	return cfg.CurrentContext
}

// LoadIntoKind makes a locally built image visible to a kind cluster's nodes,
// whose containerd cannot see the Docker daemon's images, and answers the
// cluster's name, or "" for a context that is not kind's. Such a context is
// taken to share the daemon's image store, as Docker Desktop's does; any other
// cluster (minikube, k3d, remote) needs the image loaded by hand. `docker save
// --platform` keeps the archive single-platform (a multi-arch manifest breaks
// `kind load` on darwin; an older docker without the flag falls back to a
// plain save). docker is the docker CLI's global flags — "--host" and the
// daemon's address — as dockertest.ImageFrom takes them.
func LoadIntoKind(t testing.TB, kubeCtx, image string, docker ...string) string {
	t.Helper()
	cluster, ok := strings.CutPrefix(kubeCtx, "kind-")
	if !ok {
		return ""
	}
	tar := filepath.Join(t.TempDir(), "image.tar")
	save := func(arg ...string) ([]byte, error) {
		return exec.Command("docker", append(append(append([]string{}, docker...), "save"), arg...)...).CombinedOutput()
	}
	if out, err := save("--platform", "linux/"+runtime.GOARCH, "-o", tar, image); err != nil {
		if out2, err2 := save("-o", tar, image); err2 != nil {
			t.Fatalf("docker save %s: %v\n%s\nfallback: %v\n%s", image, err, out, err2, out2)
		}
	}
	if out, err := exec.Command("kind", "load", "image-archive", tar, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("kind load image-archive: %v\n%s", err, out)
	}
	return cluster
}

// RemoveFromKind removes an image LoadIntoKind loaded from every node of the
// kind cluster. crictl removes an image by its ID, every tag of it with it, so
// the caller must own the whole image: one built for this test alone, never
// one another package may be running pods from. A pod's container can outlive
// its pod's deletion for a moment and holds the image while it does, so each
// node is asked a few times.
func RemoveFromKind(t testing.TB, cluster, image string, docker ...string) {
	t.Helper()
	nodes, err := exec.Command("kind", "get", "nodes", "--name", cluster).Output()
	if err != nil {
		t.Errorf("kind get nodes: %v", err)
		return
	}
	// containerd names a bare local tag as the Docker Hub library image it
	// would be.
	ref := image
	if !strings.Contains(ref, "/") {
		ref = "docker.io/library/" + ref
	}
	for _, node := range strings.Fields(string(nodes)) {
		var out []byte
		for range 10 {
			if out, err = exec.Command("docker", append(append([]string{}, docker...), "exec", node, "crictl", "rmi", ref)...).CombinedOutput(); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			t.Errorf("remove %s from kind node %s: %v\n%s", image, node, err, out)
		}
	}
}
