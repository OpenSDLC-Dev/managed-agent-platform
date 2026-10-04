package sandboxtest

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// `import-<date>@sha256:<digest>`, named for the archive's own index.json,
// which removing the tag leaves behind (Remove). Every image loaded is the
// loading test's own — built for it, a nonce label making its ID, and so its
// index's digest, its alone (hookedtest.Image, the Kubernetes gate fixture) —
// so nothing another load holds shares either name, and Remove can take both.
//
// containerd is the record of what a node holds, and what Refs reads. The
// kubelet's CRI view (`crictl images --digests`) caches an image row of its
// own for the import record, which containerd removing the record does not
// clear: the row stays, naming the record's digest, until containerd restarts
// — which no test does. Refs, and so Remove, say so of such a row rather than
// fail on it.
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
// plain save, as one without an index.json in its archive — before Docker
// 25 — falls back to a load whose import record cannot be named, and so stays
// on the nodes). docker is the docker CLI's global flags — "--host" and the
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
	// containerd names a bare local tag as the Docker Hub library image it
	// would be.
	ref := image
	if !strings.Contains(ref, "/") {
		ref = "docker.io/library/" + ref
	}
	if index == "" {
		t.Logf("the archive of %s has no index.json (Docker before 25): the import record kind leaves for it cannot be named, and stays on the nodes", image)
	}
	if out, err := exec.Command("kind", "load", "image-archive", archive, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("kind load image-archive: %v\n%s", err, out)
	}
	return &KindLoad{cluster: cluster, ref: ref, index: index, docker: docker}
}

// archiveIndex is the digest of a saved image archive's index.json, which is
// the digest `kind load` names its import record for, or "" for an archive
// that has none.
func archiveIndex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
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

// names reports whether an image reference is one of the load's: its tag, or
// its import record — by digest, so the CRI view's normalized name of the
// record (docker.io/library/import-…) is one too.
func (l *KindLoad) names(ref string) bool {
	return ref == l.ref || l.index != "" && strings.HasSuffix(ref, "@"+l.index)
}

// Refs is every image reference the cluster's nodes' containerd holds of the
// load — its tag and its import record — each as "node: reference". After
// Remove it is empty. A row the CRI view still lists of the load that
// containerd no longer holds is no reference (KindLoad), and is reported.
func (l *KindLoad) Refs(t testing.TB) []string {
	t.Helper()
	return l.refsOn(t, l.nodes(t))
}

// Missing is what of the load a node of the cluster does not hold, each as
// "node: what": its tag, and its import record where the archive named one
// (LoadIntoKind) — none while the load is in place.
func (l *KindLoad) Missing(t testing.TB) []string {
	t.Helper()
	var missing []string
	for _, node := range l.nodes(t) {
		tag, record := false, false
		for _, ref := range l.containerdRefs(t, node) {
			switch {
			case ref == l.ref:
				tag = true
			case l.names(ref):
				record = true
			}
		}
		if !tag {
			missing = append(missing, node+": "+l.ref)
		}
		if l.index != "" && !record {
			missing = append(missing, node+": the import record @"+l.index)
		}
	}
	return missing
}

// Remove takes the load off every node of the kind cluster: its tag, with
// crictl, which removes an image by its ID, every tag of it with it — so the
// caller must own the whole image (KindLoad), never one another package may
// be running pods from — and then, with ctr, whatever containerd still holds
// under the load's names, its import record above all. A pod's container can
// outlive its pod's deletion for a moment and holds the image while it does,
// so each node is asked a few times. It fails the test if containerd still
// holds anything of the load, and says so of the CRI view's leftover row.
func (l *KindLoad) Remove(t testing.TB) {
	t.Helper()
	nodes := l.nodes(t)
	for _, node := range nodes {
		var out []byte
		var err error
		for range 10 {
			if out, err = l.on(node, "crictl", "rmi", l.ref); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			t.Errorf("remove %s from kind node %s: %v\n%s", l.ref, node, err, out)
		}
		for _, ref := range l.containerdRefs(t, node) {
			if !l.names(ref) {
				continue
			}
			if out, err := l.on(node, "ctr", "-n", "k8s.io", "images", "rm", ref); err != nil {
				t.Errorf("remove %s from kind node %s: %v\n%s", ref, node, err, out)
			}
		}
	}
	if left := l.refsOn(t, nodes); len(left) > 0 {
		t.Errorf("the kind nodes still hold %v of %s", left, l.ref)
	}
}

// refsOn is Refs over nodes: one containerd listing and one CRI listing a
// node, the CRI view's leftover rows reported (KindLoad).
func (l *KindLoad) refsOn(t testing.TB, nodes []string) []string {
	t.Helper()
	refs := []string{}
	for _, node := range nodes {
		holds := map[string]bool{}
		for _, ref := range l.containerdRefs(t, node) {
			if l.names(ref) {
				holds[ref] = true
				refs = append(refs, node+": "+ref)
			}
		}
		for _, ref := range l.criRefs(t, node) {
			if !holds[ref] {
				t.Logf("kind node %s's CRI view lists %s, which its containerd no longer holds: the kubelet's cache keeps the row until containerd restarts", node, ref)
			}
		}
	}
	return refs
}

// criRefs is every reference the node's CRI view (`crictl images --digests`)
// lists of the load (names), each as containerd names it: the import record
// bare, where the CRI view normalizes it to docker.io/library/import-….
func (l *KindLoad) criRefs(t testing.TB, node string) []string {
	t.Helper()
	out, err := l.on(node, "crictl", "images", "--digests", "-o", "json")
	if err != nil {
		t.Fatalf("list kind node %s's CRI images: %v\n%s", node, err, out)
	}
	var view struct {
		Images []struct {
			RepoTags    []string `json:"repoTags"`
			RepoDigests []string `json:"repoDigests"`
		} `json:"images"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("read kind node %s's CRI images: %v\n%s", node, err, out)
	}
	var refs []string
	for _, im := range view.Images {
		for _, ref := range append(im.RepoTags, im.RepoDigests...) {
			if !l.names(ref) {
				continue
			}
			if ref != l.ref {
				ref = strings.TrimPrefix(ref, "docker.io/library/")
			}
			refs = append(refs, ref)
		}
	}
	return refs
}

func (l *KindLoad) containerdRefs(t testing.TB, node string) []string {
	t.Helper()
	out, err := l.on(node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		t.Fatalf("list kind node %s's images: %v\n%s", node, err, out)
	}
	return strings.Fields(string(out))
}

func (l *KindLoad) on(node string, arg ...string) ([]byte, error) {
	return exec.Command("docker", append(append(append([]string{}, l.docker...), "exec", node), arg...)...).CombinedOutput()
}

func (l *KindLoad) nodes(t testing.TB) []string {
	t.Helper()
	out, err := exec.Command("kind", "get", "nodes", "--name", l.cluster).Output()
	if err != nil {
		t.Fatalf("kind get nodes: %v", err)
	}
	return strings.Fields(string(out))
}
