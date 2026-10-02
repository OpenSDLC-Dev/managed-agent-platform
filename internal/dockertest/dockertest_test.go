package dockertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// plantImage is what the planted containers are made from — the tag pgtest
// pins, so a run of the whole suite pulls nothing it was not pulling already.
// Nothing here runs it: `docker create` never starts a container, so the image
// only has to exist.
const plantImage = "postgres:16-alpine"

func TestRunArgsLabelsEveryContainerAFixtureStarts(t *testing.T) {
	before := time.Now().Unix()
	args := RunArgs("pgtest", "-p", "127.0.0.1:0:5432", plantImage)
	after := time.Now().Unix()

	if got := args[:2]; !reflect.DeepEqual(got, []string{"run", "-d"}) {
		t.Errorf("args start %v, want the detached run the fixtures need", got)
	}
	if tail := args[len(args)-3:]; !reflect.DeepEqual(tail, []string{"-p", "127.0.0.1:0:5432", plantImage}) {
		t.Errorf("caller's arguments came through as %v", tail)
	}
	labels := labelsOf(args)
	if labels[ownerLabel] != "pgtest" {
		t.Errorf("owner label %q, want pgtest", labels[ownerLabel])
	}
	started, err := strconv.ParseInt(labels[startedLabel], 10, 64)
	if err != nil {
		t.Fatalf("started label %q does not parse: %v", labels[startedLabel], err)
	}
	if started < before || started > after {
		t.Errorf("started label %d outside [%d, %d]; the sweep judges ages against it",
			started, before, after)
	}
}

// labelsOf collects the --label pairs out of an argument list.
func labelsOf(args []string) map[string]string {
	labels := map[string]string{}
	for i, arg := range args {
		if arg != "--label" || i+1 >= len(args) {
			continue
		}
		key, value, _ := strings.Cut(args[i+1], "=")
		labels[key] = value
	}
	return labels
}

func TestStraysReapsTheAgedAndSparesEverythingElse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	ps := strings.Join([]string{
		"aaaa " + at(-strayAge-time.Second) + " pgtest",   // a corpse
		"bbbb " + at(-strayAge+time.Second) + " blobtest", // a peer, still inside the window
		"cccc " + at(-strayAge) + " secretstest",          // exactly at the bound: still a peer
		"dddd " + at(-9*time.Hour) + " gcstest",           // another corpse
		"eeee not-a-timestamp pgtest",                     // not ours to judge
		"ffff " + at(-9*time.Hour),                        // no owner value: the same
		// No start time, and an owner value whose first word parses as an
		// ancient one. Collapsing the empty column would slide that word into
		// the timestamp's place and destroy a container nothing here started.
		"gggg  1700000000 manual",
		"",
	}, "\n") + "\n"

	got := strays(ps, now)
	want := []stray{{id: "aaaa", owner: "pgtest"}, {id: "dddd", owner: "gcstest"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("strays = %v, want %v", got, want)
	}
	if s := strays("", now); s != nil {
		t.Errorf("strays of no output = %v, want none", s)
	}
}

// TestSweepStraysRemovesAnAgedContainerAndSparesAFreshOne is the one check that
// the docker invocation itself is right. The label names, the --filter and the
// --format template are strings no compiler checks, and a typo in any of them
// makes the sweep match nothing at all — a fix that looks applied and still
// leaks. Planted with `docker create`, so nothing has to start or be waited on.
func TestSweepStraysRemovesAnAgedContainerAndSparesAFreshOne(t *testing.T) {
	aged := plant(t, time.Now().Add(-2*strayAge))
	fresh := plant(t, time.Now())

	SweepStrays("dockertest")

	if !gone(context.Background(), aged) {
		t.Errorf("aged container %s survived the sweep", aged)
	}
	if gone(context.Background(), fresh) {
		t.Errorf("fresh container %s was reaped; a concurrent suite's live fixture would have been too", fresh)
	}
}

// A sweep that loses to a sibling binary's is refused with "removal of
// container … is already in progress", and the container stays listed until the
// winner's removal finishes (#843). goneBefore is what waits that out, so it is
// checked against a container another process removes a moment later — and
// against one nobody removes, which must still come back as not gone once the
// deadline passes, or a removal that really failed would never be announced.
func TestGoneBeforeWaitsOutARemovalAnotherProcessHasUnderWay(t *testing.T) {
	removed := plant(t, time.Now())
	kept := plant(t, time.Now())
	if gone(context.Background(), removed) {
		t.Fatalf("container %s is gone before anything removed it", removed)
	}

	done := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		done <- exec.Command("docker", "rm", "-f", "-v", removed).Run()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !goneBefore(ctx, removed) {
		t.Errorf("container %s, removed a second into the wait, was not seen to go", removed)
	}
	if err := <-done; err != nil {
		t.Fatalf("the concurrent removal failed: %v", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if goneBefore(ctx, kept) {
		t.Errorf("container %s, which nothing removed, was reported gone", kept)
	}
}

// removeContainer is where the wait is wired in: a removal refused because
// another is already under way must wait for the winner rather than announce a
// failure on its first look, and one that nobody completes must still be
// announced (#843). Every other refusal has no winner to wait for, so it gets
// one look and, unless that shows the container gone, is announced at once —
// waiting it out instead would spend the sweep's budget on containers nobody is
// removing. A real daemon cannot be made to have a removal in flight at the
// moment this one runs, so a fake `docker` stands in: `rm` is refused with the
// given text, and `ps` lists the container for its first listedFor calls only.
func TestARemovalThatLosesARaceWaitsForTheWinnerRatherThanAnnouncingIt(t *testing.T) {
	const inProgress = "Error response from daemon: removal of container c0ffee is already in progress"
	for name, tc := range map[string]struct {
		refusal   string
		listedFor int
		within    time.Duration
		announced bool
		prompt    bool // answered after one look, without waiting out within
	}{
		"the winner's removal finishes": {refusal: inProgress, listedFor: 2, within: 10 * time.Second},
		"nobody's removal finishes": {refusal: inProgress, listedFor: 1_000_000, within: time.Second,
			announced: true},
		"any other refusal is announced at once": {refusal: "Error response from daemon: permission denied",
			listedFor: 1_000_000, within: 10 * time.Second, announced: true, prompt: true},
		"any other refusal of a container already gone is silent": {
			refusal:   "Error response from daemon: No such container: c0ffee",
			listedFor: 0, within: 10 * time.Second, prompt: true},
	} {
		t.Run(name, func(t *testing.T) {
			looks := fakeDocker(t, tc.refusal, tc.listedFor)
			ctx, cancel := context.WithTimeout(context.Background(), tc.within)
			defer cancel()

			var removed bool
			said := stderrOf(t, func() { removed = removeContainer(ctx, "dockertest", "c0ffee") })
			if removed {
				t.Errorf("a refused removal was reported as this call's own")
			}
			if announced := said != ""; announced != tc.announced {
				t.Errorf("announced = %v (%q), want %v", announced, said, tc.announced)
			}
			// Counted, not timed: a wall-clock bound flakes on a loaded machine,
			// and the claim is that nothing is waited for — one look, no poll.
			if n := looks(); tc.prompt && n != 1 {
				t.Errorf("looked %d times; a refusal with no winner to wait for must be "+
					"answered after one look", n)
			}
		})
	}
}

// fakeDocker puts a `docker` on PATH whose `rm` always fails with refusal, and
// whose `ps` lists the container for its first listedFor calls and then reports
// it gone. Builtins only, because PATH holds nothing else. It returns a count of
// the `ps` calls made so far.
func fakeDocker(t *testing.T, refusal string, listedFor int) (looks func() int) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "ps-calls")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
rm)
	echo %[3]q >&2
	exit 1 ;;
ps)
	n=0
	[ -f %[1]q ] && read n < %[1]q
	n=$((n + 1))
	echo "$n" > %[1]q
	[ "$n" -le %[2]d ] && echo c0ffee
	exit 0 ;;
esac
exit 2
`, calls, listedFor, refusal)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake docker: %v", err)
	}
	t.Setenv("PATH", dir)
	return func() int {
		b, err := os.ReadFile(calls)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatalf("read the fake docker's ps count: %v", err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatalf("parse the fake docker's ps count %q: %v", b, err)
		}
		return n
	}
}

// stderrOf runs f and returns what it wrote to os.Stderr, which is where
// removeContainer announces a container it could not clear.
func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	old := os.Stderr
	os.Stderr = w
	func() {
		defer func() { os.Stderr = old }()
		f()
	}()
	w.Close()
	said, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(said)
}

// plant creates a stopped container labelled as a fixture started at the given
// time, and removes it at test end. Being labelled, it is also reaped by a
// later run's sweep if this run is the one that gets killed.
func plant(t *testing.T, started time.Time) string {
	t.Helper()
	out, err := exec.Command("docker", "create",
		"--label", ownerLabel+"=dockertest",
		"--label", startedLabel+"="+strconv.FormatInt(started.Unix(), 10),
		plantImage).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, exitErr.Stderr)
		}
		t.Fatalf("this test requires Docker to plant a container: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { removeContainer(context.Background(), "dockertest", id) })
	return id
}
