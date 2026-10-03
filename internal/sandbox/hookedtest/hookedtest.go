// Package hookedtest provisions sandboxes from a hooked image — one whose
// `ENV BASH_ENV` file prints on both streams without ending its lines, leaves
// the directory the shell started in, and sets an EXIT trap that prints on both
// after whatever the shell ran — on both sandbox backends: the Docker daemon
// and the Kubernetes cluster the tests run against. It is how a platform script
// that parses an exec's output is held to answer through an image's startup
// output on each (#860; sandbox.Frame).
//
// A missing daemon or cluster is a hard failure, as it is for the backends'
// own contract tests. On a kind cluster the image is loaded into its nodes
// (`kind load`, as the gate fixture does) and removed from them afterwards; a
// cluster that shares the Docker daemon's image store (Docker Desktop's) needs
// neither.
package hookedtest

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
)

// Hook is the hooked image's BASH_ENV file: a banner on stdout and one on
// stderr, neither ending its line, a `cd /`, and an EXIT trap that prints on
// both streams after the script.
const Hook = `printf 'welcome to the image '; printf 'stderr banner ' >&2; cd /; ` +
	`trap "printf 'exit banner '; printf 'exit stderr ' >&2" EXIT` + "\n"

// Banners are the words Hook prints, which a check reading the output of a
// command the image's startup reaches — the bash tool's — strips first.
var Banners = []string{"welcome to the image ", "stderr banner ", "exit banner ", "exit stderr "}

// Unbanner is s with Hook's words taken out.
func Unbanner(s string) string {
	for _, b := range Banners {
		s = strings.ReplaceAll(s, b, "")
	}
	return s
}

// baseImage is the image the hooked one is built on: the image the backends'
// contract tests run, /bin/bash and a GNU userland.
const baseImage = "debian:stable-slim"

// Backend is one sandbox backend with the hooked image visible to it.
type Backend struct {
	Name     string
	Provider sandbox.Provider
	Image    string
}

// Backends builds the hooked image and returns the Docker backend and the
// Kubernetes one (MAP_K8S_CONTEXT, else the kubeconfig's current context;
// MAP_K8S_NAMESPACE), each able to run it.
func Backends(t *testing.T) []Backend {
	t.Helper()
	image := dockertest.ImageFrom(t, "hooked", "FROM "+baseImage+"\n"+
		"RUN echo "+base64.StdEncoding.EncodeToString([]byte(Hook))+" | base64 -d > /etc/map-hook.sh\n"+
		"ENV BASH_ENV=/etc/map-hook.sh\n", "--host", docker.DaemonHost())
	dp, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("hooked sandboxes require Docker: %v", err)
	}
	kp, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("hooked sandboxes require a Kubernetes cluster: %v", err)
	}
	loadIntoKind(t, image)
	return []Backend{{Name: "docker", Provider: dp, Image: image}, {Name: "k8s", Provider: kp, Image: image}}
}

// Provision provisions a sandbox from the hooked image on b, with h as its
// hardening, destroyed when the test ends. The session id is the sandbox's.
func (b Backend) Provision(t *testing.T, h sandbox.Hardening) (sandbox.Sandbox, domain.ID) {
	t.Helper()
	sid := domain.NewID(domain.PrefixSession)
	sb, err := b.Provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  sid,
		Image:      b.Image,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  h,
	})
	if err != nil {
		t.Fatalf("%s: provision the hooked image: %v", b.Name, err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("%s: destroy: %v", b.Name, err)
		}
	})
	return sb, sid
}

// loadIntoKind makes image visible to a kind cluster's nodes, whose containerd
// cannot see the Docker daemon's images, and removes it from them when the
// test is done. `docker save --platform` keeps the archive single-platform (a
// multi-arch manifest breaks `kind load` on darwin; an older docker without
// the flag falls back to a plain save). A context that is not kind's is taken
// to share the daemon's image store, as Docker Desktop's does; any other
// cluster needs the image loaded by hand.
func loadIntoKind(t *testing.T, image string) {
	t.Helper()
	kubeCtx := os.Getenv("MAP_K8S_CONTEXT")
	if kubeCtx == "" {
		cfg, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
		if err != nil {
			t.Fatalf("load kubeconfig: %v", err)
		}
		kubeCtx = cfg.CurrentContext
	}
	cluster, ok := strings.CutPrefix(kubeCtx, "kind-")
	if !ok {
		return
	}
	host := []string{"--host", docker.DaemonHost()}
	tar := filepath.Join(t.TempDir(), "hooked.tar")
	if out, err := exec.Command("docker", append(host, "save", "--platform", "linux/"+runtime.GOARCH, "-o", tar, image)...).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("docker", append(host, "save", "-o", tar, image)...).CombinedOutput(); err2 != nil {
			t.Fatalf("docker save %s: %v\n%s\nfallback: %v\n%s", image, err, out, err2, out2)
		}
	}
	if out, err := exec.Command("kind", "load", "image-archive", tar, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("kind load image-archive: %v\n%s", err, out)
	}
	// A pod's container may outlive its pod's deletion for a moment, and the
	// image cannot be removed under it, so each node is asked a few times.
	t.Cleanup(func() {
		nodes, err := exec.Command("kind", "get", "nodes", "--name", cluster).Output()
		if err != nil {
			t.Errorf("kind get nodes: %v", err)
			return
		}
		for _, node := range strings.Fields(string(nodes)) {
			var out []byte
			for range 10 {
				if out, err = exec.Command("docker", append(host, "exec", node, "crictl", "rmi", "docker.io/library/"+image)...).CombinedOutput(); err == nil {
					break
				}
				time.Sleep(3 * time.Second)
			}
			if err != nil {
				t.Errorf("remove %s from kind node %s: %v\n%s", image, node, err, out)
			}
		}
	})
}
