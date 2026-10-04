// Package hookedtest provisions sandboxes from a hooked image — one whose
// `ENV BASH_ENV` file is sandboxtest.BannerHook: it prints on both streams
// without ending its lines, leaves the directory the shell started in, and
// sets an EXIT trap that prints on both after whatever the shell ran — on both
// sandbox backends: the Docker daemon and the Kubernetes cluster the tests run
// against. It is how a platform script that parses an exec's output is held
// to answer through an image's startup output on each (#860; sandbox.Frame).
//
// A missing daemon or cluster is a hard failure, as it is for the backends'
// own contract tests. On a kind cluster the image is loaded into its nodes and
// removed from them afterwards (sandboxtest.LoadIntoKind, RemoveFromKind); a
// cluster that shares the Docker daemon's image store (Docker Desktop's) needs
// neither.
package hookedtest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dockertest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// baseImage is the image the hooked one is built on: the image the backends'
// contract tests run, /bin/bash and a GNU userland.
const baseImage = "debian:stable-slim"

// Backend is one sandbox backend with the hooked image visible to it.
type Backend struct {
	Name     string
	Provider sandbox.Provider
	Image    string
}

// Backends builds the hooked image (Image, with sandboxtest.BannerHook) and
// returns the Docker backend and the Kubernetes one (MAP_K8S_CONTEXT, else the
// kubeconfig's current context; MAP_K8S_NAMESPACE), each able to run it.
func Backends(t *testing.T) []Backend {
	t.Helper()
	image := Image(t, sandboxtest.BannerHook)
	dp, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("hooked sandboxes require Docker: %v", err)
	}
	kp, err := k8s.New(k8s.Config{Context: os.Getenv("MAP_K8S_CONTEXT"), Namespace: os.Getenv("MAP_K8S_NAMESPACE")})
	if err != nil {
		t.Fatalf("hooked sandboxes require a Kubernetes cluster: %v", err)
	}
	return []Backend{{Name: "docker", Provider: dp, Image: image}, {Name: "k8s", Provider: kp, Image: image}}
}

// Image builds an image whose `ENV BASH_ENV` file is hook, on the base the
// backends' contract tests run, makes it visible to the Kubernetes cluster the
// tests run against, and removes it from both when the test is done.
//
// Each call builds an image of its own: the same Dockerfile would build the
// same image ID in every package that asks at once, sharing layers through the
// build cache, and removing one package's from a kind node takes the image ID,
// every package's tag with it, out from under pods still running from it. A
// label carrying a nonce makes the ID this call's alone, its layers still
// shared, so the removal when the test ends takes this call's image and nothing
// another package runs.
func Image(t *testing.T, hook string) string {
	t.Helper()
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	host := []string{"--host", docker.DaemonHost()}
	image := dockertest.ImageFrom(t, "hooked", "FROM "+baseImage+"\n"+
		"LABEL map.hooked.build="+hex.EncodeToString(nonce[:])+"\n"+
		"RUN echo "+base64.StdEncoding.EncodeToString([]byte(hook))+" | base64 -d > /etc/map-hook.sh\n"+
		"ENV BASH_ENV=/etc/map-hook.sh\n", host...)
	if cluster := sandboxtest.LoadIntoKind(t, sandboxtest.KubeContext(t), image, host...); cluster != "" {
		t.Cleanup(func() { sandboxtest.RemoveFromKind(t, cluster, image, host...) })
	}
	return image
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
