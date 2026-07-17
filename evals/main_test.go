// Package evals is the end-to-end eval suite: whole sessions driven through the
// public REST API against a real model endpoint and real Docker sandboxes, with
// deterministic code-based graders. It is the tier that answers "does the whole
// thing still work", which no unit or contract test can — every other loop test
// in this repo scripts the provider.
//
// It exists only as tests. `go test` already supplies subtests, timeouts and
// panic-safe t.Cleanup, so a runner binary would only duplicate them.
//
// Cost: this tier calls a real model and starts real containers. It is opt-in
// through RUN_EVALS (see TestMain and modeltest's contract) — an ordinary
// `go test ./...` runs the offline unit tests here and nothing else.
package evals

import (
	"os"
	"os/exec"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// evalImage is the sandbox base image for the whole run, pinned in one place.
//
// Debian-slim underneath, so it is the same bash and coreutils userland the
// toolset's scripts probe for — verified: /bin/bash exists at the exact path
// the docker provider requires, glob's stat/sort/xargs are present, and grep's
// PCRE probe passes so -P is kept rather than downgraded to -E. Python is here
// for exactly one task (fib-quickstart); every other task is image-agnostic.
const evalImage = "python:3.12-slim"

// TestMain gates before pgtest.Main, and the order is the point.
//
// pgtest.Main starts a Postgres container unconditionally and waits up to two
// minutes for it — before m.Run, so before any test can skip. Calling it first
// would make every `make verify` pay a container start for a suite that then
// skips every test. The gate below is the same one modeltest.Endpoint applies
// per test (any non-empty RUN_EVALS opts in), kept in agreement by asking
// modeltest rather than re-spelling the rule here.
//
// Offline tests still run in the skipped case: m.Run is called either way, and
// the graders and report have unit tests that need no model, no Postgres and no
// Docker. Those are what keep this package honest on an ordinary PR.
func TestMain(m *testing.M) {
	if !modeltest.TierEnabled(modeltest.EvalsEnv) {
		os.Exit(m.Run())
	}
	// One pull for the whole run: trials that raced N first-run pulls of the
	// same image would each pay for it, and a slow pull inside a trial reads as
	// a slow agent.
	if out, err := exec.Command("docker", "pull", evalImage).CombinedOutput(); err != nil {
		// Not fatal here — a trial that needs the image fails with its own
		// message, and this keeps a transient registry blip from failing a run
		// whose image is already cached locally.
		os.Stderr.WriteString("evals: pre-pull of " + evalImage + " failed, continuing: " +
			err.Error() + "\n" + string(out) + "\n")
	}
	// pgtest.Main runs the suite and returns its exit code, so the artifacts are
	// written here — after every trial has recorded itself, and whether the run
	// was green or red. A red run is the one whose transcripts someone needs.
	code := pgtest.Main(m)
	if err := writeArtifacts(); err != nil {
		os.Stderr.WriteString("evals: writing artifacts failed: " + err.Error() + "\n")
	}
	os.Exit(code)
}
