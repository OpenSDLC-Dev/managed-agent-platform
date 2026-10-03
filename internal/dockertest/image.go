package dockertest

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
)

// ImageFrom builds an image from a Dockerfile, read from stdin with no build
// context, for one test, and removes it when the test is done. The tag is
// "map-<name>-test:<nonce>", this call's alone: two tests or two parallel
// runs building the same Dockerfile share its layers through the build cache
// and never its name, so one removing its tag cannot take an image another is
// still running containers from — docker rmi only untags while another tag
// holds the image. docker is the CLI's global flags, such as "--host", for a
// test that must name the daemon its provider uses.
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
