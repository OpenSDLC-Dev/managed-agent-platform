package sandboxtest

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

// KindLoad is an image LoadIntoKind put on a kind cluster's nodes: under its
// tag, and under the record `kind load` leaves beside it,
// `import-<date>@sha256:<digest>`, named for the archive's own index, which
// removing the tag leaves behind (Remove).
type KindLoad struct {
	cluster, ref, index string
	docker              []string
}

// LoadIntoKind makes a locally built image visible to a kind cluster's nodes,
// whose containerd cannot see the Docker daemon's images, and answers what it
// loaded, or nil for a context that is not kind's. Such a context is taken to
// share the daemon's image store, as Docker Desktop's does; any other cluster
// (minikube, k3d, remote) needs the image loaded by hand. `docker save
// --platform` keeps the archive single-platform (a multi-arch manifest breaks
// `kind load` on darwin; an older docker without the flag falls back to a
// plain save). docker is the docker CLI's global flags — "--host" and the
// daemon's address — as dockertest.ImageFrom takes them.
func LoadIntoKind(t testing.TB, kubeCtx, image string, docker ...string) *KindLoad {
	t.Helper()
	cluster, ok := strings.CutPrefix(kubeCtx, "kind-")
	if !ok {
		return nil
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	save := func(arg ...string) ([]byte, error) {
		return exec.Command("docker", append(append(append([]string{}, docker...), "save"), arg...)...).CombinedOutput()
	}
	if out, err := save("--platform", "linux/"+runtime.GOARCH, "-o", archive, image); err != nil {
		if out2, err2 := save("-o", archive, image); err2 != nil {
			t.Fatalf("docker save %s: %v\n%s\nfallback: %v\n%s", image, err, out, err2, out2)
		}
	}
	index, err := archiveIndex(archive)
	if err != nil {
		t.Fatalf("read the archive of %s: %v", image, err)
	}
	if out, err := exec.Command("kind", "load", "image-archive", archive, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("kind load image-archive: %v\n%s", err, out)
	}
	// containerd names a bare local tag as the Docker Hub library image it
	// would be.
	ref := image
	if !strings.Contains(ref, "/") {
		ref = "docker.io/library/" + ref
	}
	return &KindLoad{cluster: cluster, ref: ref, index: index, docker: docker}
}

// archiveIndex is the digest of a saved image archive's index.json, which is
// the digest `kind load` names its import record for.
func archiveIndex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			return "", fmt.Errorf("no index.json: %w", err)
		}
		if h.Name == "index.json" {
			sum := sha256.New()
			if _, err := io.Copy(sum, tr); err != nil {
				return "", err
			}
			return "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
		}
	}
}

// Refs is every image reference the cluster's nodes hold of the load: its tag
// and its import record. After Remove it is none.
func (l *KindLoad) Refs(t testing.TB) []string {
	t.Helper()
	var refs []string
	for _, node := range l.nodes(t) {
		out, err := exec.Command("docker", append(append([]string{}, l.docker...), "exec", node, "ctr", "-n", "k8s.io", "images", "ls", "-q")...).Output()
		if err != nil {
			t.Fatalf("list kind node %s's images: %v", node, err)
		}
		for _, ref := range strings.Fields(string(out)) {
			if ref == l.ref || strings.HasSuffix(ref, "@"+l.index) {
				refs = append(refs, node+": "+ref)
			}
		}
	}
	return refs
}

// Remove takes the load off every node of the kind cluster: its tag, with
// crictl, which removes an image by its ID, every tag of it with it — so the
// caller must own the whole image, one built for this test alone, never one
// another package may be running pods from — and its import record, which
// crictl leaves, with ctr. A pod's container can outlive its pod's deletion
// for a moment and holds the image while it does, so each node is asked a few
// times. It says so if anything of the load is left.
func (l *KindLoad) Remove(t testing.TB) {
	t.Helper()
	for _, node := range l.nodes(t) {
		run := func(arg ...string) ([]byte, error) {
			return exec.Command("docker", append(append(append([]string{}, l.docker...), "exec", node), arg...)...).CombinedOutput()
		}
		var out []byte
		var err error
		for range 10 {
			if out, err = run("crictl", "rmi", l.ref); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			t.Errorf("remove %s from kind node %s: %v\n%s", l.ref, node, err, out)
		}
		listed, err := run("ctr", "-n", "k8s.io", "images", "ls", "-q")
		if err != nil {
			t.Errorf("list kind node %s's images: %v\n%s", node, err, listed)
			continue
		}
		for _, ref := range strings.Fields(string(listed)) {
			if strings.HasSuffix(ref, "@"+l.index) {
				if out, err := run("ctr", "-n", "k8s.io", "images", "rm", ref); err != nil {
					t.Errorf("remove %s from kind node %s: %v\n%s", ref, node, err, out)
				}
			}
		}
	}
	if left := l.Refs(t); len(left) > 0 {
		t.Errorf("the kind nodes still hold %v of %s", left, l.ref)
	}
}

func (l *KindLoad) nodes(t testing.TB) []string {
	t.Helper()
	out, err := exec.Command("kind", "get", "nodes", "--name", l.cluster).Output()
	if err != nil {
		t.Fatalf("kind get nodes: %v", err)
	}
	return strings.Fields(string(out))
}
