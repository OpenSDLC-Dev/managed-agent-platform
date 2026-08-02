// Package acceptance replays the reference define-outcomes example —
// https://platform.claude.com/docs/en/managed-agents/define-outcomes — against
// this platform through the official Go SDK's typed client (plan 21's
// acceptance harness). It is the one suite that drives the platform the way a
// customer's program would: every request is built by the SDK, every response
// decodes through the SDK's typed structs, and a field the SDK does not
// recognize is a failure, not a curiosity.
//
// Two legs share one harness (runDCF):
//
//   - The rehearsal (TestRehearsalDCF*) runs in the ordinary `go test ./...`
//     path: the whole platform in-process over pgtest's Postgres and a real
//     Docker sandbox, with the model scripted. Deterministic, free, and it
//     guards the mechanics in CI so the live run is the only paid step.
//   - The live leg (TestLiveDefineOutcomesAcceptance) is opt-in via
//     RUN_LIVE_ACCEPTANCE_TESTS and drives an externally running stack (the compose
//     stack, docs/plan/21_outcomes.md's acceptance) whose brain calls a real
//     model. It is never part of `make test`'s default path.
//
// The evals suite (../evals) deliberately speaks raw map[string]any so a wire
// regression cannot hide inside a struct tag; this package is its complement,
// proving the same wire through the SDK a real integration would use.
package acceptance

import (
	"os"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestMain starts pgtest's Postgres unconditionally: unlike evals, whose live
// tier gates its setup, the rehearsal here runs on every ordinary `go test`,
// so the container is never paid for a suite that then skips everything.
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }
