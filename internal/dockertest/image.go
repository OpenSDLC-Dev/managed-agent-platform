package dockertest

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Host is a Docker daemon address for a test outside internal/sandbox/docker
// to hand both its provider (docker.Config.Host) and every docker CLI call it
// makes (`--host`), so the two reach one daemon: left alone the CLI follows the
// active `docker context`, which the provider never reads (#627). It is the
// provider's own default — DOCKER_HOST, then the well-known socket — though
// what keeps the two together is that both are given it, not that it matches.
func Host() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// ImageFrom builds an image from a Dockerfile, read from stdin with no build
// context, for one test, and removes it when the test is done. The tag is
// "map-<name>-test:<nonce>", this call's alone: two tests or two parallel
// runs building the same Dockerfile share its layers through the build cache
// and never its name, so one removing its tag cannot take an image another is
// still running containers from — docker rmi only untags while another tag
// holds the image. docker is the CLI's global flags: "--host" and the address
// the test's provider uses (Host, or the sandbox/docker package's own
// DaemonHostForTest), which every caller passes, so the image is built on the
// daemon that will run it.
func ImageFrom(t testing.TB, name, dockerfile string, docker ...string) string {
	t.Helper()
	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	image := "map-" + name + "-test:" + hex.EncodeToString(nonce[:])
	cli := func(arg ...string) *exec.Cmd {
		return exec.Command("docker", append(append([]string{}, docker...), arg...)...)
	}
	build := cli("build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(dockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", image, err, out)
	}
	t.Cleanup(func() {
		if out, err := cli("rmi", image).CombinedOutput(); err != nil {
			t.Errorf("remove %s: %v\n%s", image, err, out)
		}
	})
	return image
}
