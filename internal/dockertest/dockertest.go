// Package dockertest is the shared half of this repo's Dockerized test
// fixtures: it labels the container a fixture starts, and reaps the ones a
// killed run left behind. pgtest, blobtest, secretstest and gcstest each start
// one container per test binary and remove it in a deferred call — and a defer
// does not run when the process is killed. Ctrl-C, an IDE's stop button, go
// test's own timeout panic, a laptop suspending mid-run: any of them leaves the
// container running, and its anonymous volume growing, forever. One machine
// accumulated twenty stray Postgres containers holding 12.6 GB that way (#346).
// Nothing else reaps them — the fixtures shell out to docker rather than going
// through testcontainers, so there is no ryuk sidecar on the machine to notice.
//
// This is not github.com/ory/dockertest. Production code must never import it.
package dockertest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ownerLabel names the fixture that started a container ("pgtest", ...). The
// sweep filters on it, and it is what tells whoever finds a stray which suite
// left it — the identification #346's reporter had to make by hand, from
// POSTGRES_PASSWORD=test and the absence of a compose project label.
const ownerLabel = "dev.opensdlc.managed-agent-platform.test-fixture"

// startedLabel carries the start time in Unix seconds, read from the clock of
// the host that started the container — the same clock the sweep judges it
// against. Asking the daemon for its own creation timestamp instead would
// compare two clocks, and a Docker Desktop VM's clock drifts from its host's
// across a laptop suspend, which is one of the ways a run gets killed.
//
// One clock is still one assumption: that it only moves forward smoothly. A
// jump larger than strayAge — an NTP correction, or a resume from a suspend
// that outlasted it — makes a live peer look ancient to a binary starting after
// the jump. No label-based scheme escapes that. The reader is a different
// process, so Go's monotonic clock cannot cross the two, and reading the
// daemon's clock at both ends only moves the same discontinuity into the
// daemon's VM. What bounds it is that the jump has to exceed strayAge, and that
// a run already interrupted by a six-hour clock jump is not a run that was
// going to pass.
const startedLabel = ownerLabel + "-started"

// strayAge is how long a fixture container must have been alive before a later
// run may treat it as a corpse rather than a peer. `go test ./...` runs the
// fixture-using package binaries concurrently — eleven of them use pgtest — so
// removing every labelled container on sight would destroy a sibling suite's
// live database mid-run, trading a disk-space annoyance for a flaky suite. Age
// separates the two without any coordination between binaries, which the
// alternatives need: a per-invocation id injected by a Makefile wrapper, or a
// sweep outside the test binaries, both stop working the moment someone runs
// `go test ./internal/store` directly.
//
// Six hours is three times the longest run this repo sanctions — `make eval`
// gives its binaries -timeout 120m and they use pgtest — and what it reaps is a
// leak noticed hours or days later, so a wide margin costs nothing. The price
// of the margin is that a stray outlives the run that killed it until some run
// happens six hours later; disk is reclaimed late, but never by hand.
const strayAge = 6 * time.Hour

// sweepBudget bounds one whole sweep — the listing and every removal under it.
// Housekeeping is not what a test run is for, and this one happens before
// go test's own alarm exists, so it gets a wall-clock allowance rather than the
// right to take as long as the daemon does. A minute clears a realistic backlog
// several times over (a `docker rm -f` of a fixture is well under a second),
// and a backlog it cannot clear is one the next run continues.
const sweepBudget = time.Minute

// RunArgs builds the `docker run` argument list for a detached fixture
// container of the named harness, labelled so SweepStrays can find it once the
// process that started it is gone:
//
//	exec.Command("docker", dockertest.RunArgs("pgtest",
//		"-e", "POSTGRES_PASSWORD=test", "-p", "127.0.0.1:0:5432", image)...)
//
// Every fixture starts its container through this, so none of them can start an
// unlabelled one — the invariant the sweep's completeness rests on.
func RunArgs(harness string, rest ...string) []string {
	return append([]string{"run", "-d",
		"--label", ownerLabel + "=" + harness,
		"--label", startedLabel + "=" + strconv.FormatInt(time.Now().Unix(), 10),
	}, rest...)
}

// SweepStrays force-removes every fixture container older than strayAge, with
// -v so the anonymous volume the image declares goes with it — the reason the
// leak cost gigabytes rather than megabytes. Call it at the top of a fixture's
// TestMain, before that run starts its own container: the next run is what
// cleans up after the killed one.
//
// harness only names the caller in the messages. The sweep reaps every
// fixture's strays whichever suite left them, since the age rule is the same
// for all of them and the label is this repo's alone.
//
// Listing is best effort and silent: a daemon that will not answer at all is
// about to be reported far better by the caller's own `docker run`, and a stray
// this run never sees is one the next run gets. Removal is not silent — see
// removeContainer.
//
// The whole sweep shares one deadline, rather than each docker call carrying
// its own. This runs inside TestMain, before m.Run, where `go test -timeout`'s
// alarm is not yet armed and nothing will interrupt a wedged daemon: with a
// per-call bound only, the twenty strays #346 reported could hold a test binary
// for a quarter of an hour and look like a hang rather than a leak. Giving up
// with strays left costs nothing, because every later run sweeps again.
func SweepStrays(harness string) {
	ctx, cancel := context.WithTimeout(context.Background(), sweepBudget)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all",
		"--filter", "label="+ownerLabel,
		"--format", `{{.ID}} {{.Label "`+startedLabel+`"}} {{.Label "`+ownerLabel+`"}}`).Output()
	if err != nil {
		return
	}
	for _, s := range strays(string(out), time.Now()) {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "%s: stray sweep ran out of its %s budget; the next run retries (#346)\n",
				harness, sweepBudget)
			return
		}
		if !removeContainer(ctx, harness, s.id) {
			continue
		}
		fmt.Fprintf(os.Stderr, "%s: reaped stray %s container %s left by a killed run (#346)\n",
			harness, s.owner, s.id)
	}
}

// removeContainer force-removes one container and the anonymous volumes it
// owns, and reports whether it was this call that removed it. Its own bound sits
// under the sweep's, so one wedged call cannot starve the strays behind it and
// the sweep as a whole still ends when its budget does.
//
// A failure is announced unless the container is simply gone, which is the
// expected way to lose: `go test ./...` has eleven TestMains sweeping the same
// corpse, and only one of them can win. Everything else — a daemon that refuses,
// a volume that will not delete — is a container this run could not clear.
//
// That announcement reaches a terminal only when the package fails or the suite
// runs under -v, since `go test ./...` buffers a passing package's output and
// prints just its ok line. So it is a breadcrumb for whoever is already looking
// rather than an alarm, and it is deliberately not made one: a stray this run
// could not clear is not a reason to redden a suite that otherwise passed. What
// actually bounds the leak is that every later run sweeps again.
func removeContainer(ctx context.Context, harness, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", id).CombinedOutput()
	if err == nil {
		return true
	}
	if !gone(ctx, id) {
		fmt.Fprintf(os.Stderr, "%s: reaping stray container %s: %v: %s\n",
			harness, id, err, strings.TrimSpace(string(out)))
	}
	return false
}

// gone reports whether the daemon no longer knows the container. A daemon that
// will not answer counts as still there, so an unreachable daemon is reported as
// the removal failure it is rather than mistaken for a lost race.
func gone(ctx context.Context, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet",
		"--filter", "id="+id).Output()
	return err == nil && strings.TrimSpace(string(out)) == ""
}

// stray is one container the sweep decided to remove.
type stray struct{ id, owner string }

// strays picks the corpses out of `docker ps` output — one line per container,
// "<id> <started unix seconds> <harness>". A line missing either label value,
// or carrying a timestamp that does not parse, is left alone: something other
// than RunArgs wrote that label, and destroying a container this package cannot
// account for is worse than leaving one behind.
//
// The split is positional (SplitN), not whitespace-collapsing (Fields), because
// a missing label renders as an empty column rather than no column: Fields
// closes the gap and slides the harness name into the timestamp's place, so a
// container labelled by something else — with no start time and a value whose
// first word happens to parse as a number — would be read as ancient and
// removed. SplitN leaves the empty column empty, where ParseInt rejects it, and
// keeps a harness name containing spaces intact for the message.
func strays(psOutput string, now time.Time) []stray {
	var found []stray
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(fields) < 3 {
			continue
		}
		started, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || now.Sub(time.Unix(started, 0)) <= strayAge {
			continue
		}
		found = append(found, stray{id: fields[0], owner: fields[2]})
	}
	return found
}
