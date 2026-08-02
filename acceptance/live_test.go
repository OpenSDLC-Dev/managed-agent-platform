package acceptance

import (
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// acceptanceEnv consents to the live leg. Its own tier variable, not
// modeltest's RUN_LIVE_MODEL_TESTS: that one buys a single turn for cents,
// this leg buys whole agent sessions for minutes and dollars, and the tier
// contract's rule is that opting into the cheap smoke must not silently buy
// the expensive suite. Any non-empty value opts in, like every other tier.
const acceptanceEnv = "RUN_LIVE_ACCEPTANCE_TESTS"

// TestLiveDefineOutcomesAcceptance is plan 21's paid acceptance leg: the same
// runDCF harness the rehearsal proves, pointed at an externally running stack
// (deploy/compose) whose brain calls a real model. Consent follows the
// modeltest tier convention with a variable of its own —
// RUN_LIVE_ACCEPTANCE_TESTS opts in (see acceptanceEnv for why it is not
// RUN_LIVE_MODEL_TESTS), and once opted in, missing configuration fails
// rather than skips. The target is configured by environment only (never
// .env, which modeltest reserves for MODEL_*):
//
//	ACCEPTANCE_BASE_URL  the stack's API root (default http://localhost:8080)
//	ACCEPTANCE_API_KEY   its management key (compose: CONTROLPLANE_API_KEY)
//	ACCEPTANCE_MODEL     the agent model string the stack's routing resolves
//
// Both rubric variants run, each as its own full agent session; expect minutes
// and real model spend. Sandbox containers this leg creates on the target
// stack's daemon are named map-<session_id> and are the operator's to remove
// once the run is recorded.
func TestLiveDefineOutcomesAcceptance(t *testing.T) {
	if os.Getenv(acceptanceEnv) == "" {
		t.Skipf("%s is not set: skipping the live acceptance leg (no model is called)", acceptanceEnv)
	}
	base := os.Getenv("ACCEPTANCE_BASE_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	key := os.Getenv("ACCEPTANCE_API_KEY")
	model := os.Getenv("ACCEPTANCE_MODEL")
	if key == "" || model == "" {
		t.Fatalf("%s opted into the live acceptance leg but ACCEPTANCE_API_KEY or ACCEPTANCE_MODEL is unset: "+
			"set them to the running stack's management key and agent model string (deploy/compose/README.md)",
			acceptanceEnv)
	}
	client := anthropic.NewClient(option.WithAPIKey(key), option.WithBaseURL(base))

	for _, tc := range []struct {
		name       string
		fileRubric bool
	}{
		{"file-rubric", true},
		{"text-rubric", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runDCF(t, client, nil, model, tc.fileRubric, 25*time.Minute)
			t.Logf("session %s: outcome %s reached %q after %d evaluation cycle(s); %d deliverable(s); ongoing heartbeat seen: %v",
				run.SessionID, run.OutcomeID, run.Terminal, len(run.EndResults), len(run.Files), run.SawOngoing)
			for _, fm := range run.Files {
				t.Logf("deliverable: %s (%s, %d bytes)", fm.Filename, fm.MimeType, fm.SizeBytes)
			}
			t.Logf("grader explanation: %s", run.Explanation)
		})
	}
}
