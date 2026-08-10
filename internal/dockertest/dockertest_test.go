package dockertest

import (
	"errors"
	"fmt"
	"os/exec"
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

	if exists(t, aged) {
		t.Errorf("aged container %s survived the sweep", aged)
	}
	if !exists(t, fresh) {
		t.Errorf("fresh container %s was reaped; a concurrent suite's live fixture would have been too", fresh)
	}
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
	t.Cleanup(func() { removeContainer(id) })
	return id
}

func exists(t *testing.T, id string) bool {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--quiet",
		"--filter", "id="+id).Output()
	if err != nil {
		t.Fatalf("look up container %s: %v", id, err)
	}
	return strings.TrimSpace(string(out)) != ""
}
