package toolset

import (
	"fmt"
	"math"
	"time"
)

// The stall budget's floor, and the parsing that enforces it. It lives here
// rather than in each binary's main because both binaries need exactly the same
// refusals and main packages are outside the coverage gate — the guard was the
// one in #383 with no test at all until it moved here.

// DefaultStallBudget is the budget a binary uses when its operator sets none:
// three times the step every deployment has, so a slow image pull behind a slow
// mount behind a full-length `bash` still clears it while a wedge costs half an
// hour rather than the process's life.
//
// Derived rather than written out, and shared rather than copied, because both
// binaries want the same number for the same reason and a literal in each would
// let one follow a change to MaxTimeout while the other did not — the drift the
// refusals were moved here to prevent (#383).
const DefaultStallBudget = 3 * MaxTimeout

// StallFloor is the shortest stall budget a healthy run survives: the longest
// single step it may spend inside one silent interval, plus a minute.
//
// The minute is not slack. A step that hits its own cap answers *after* the
// cap: a sandbox backend waits a kill grace past the deadline for the command
// to die, and the result still has to come back over the transport. A floor of
// exactly the cap would cancel that healthy, timed-out step moments before it
// answered, and its use would stay unanswered on every reclaim.
//
// A budget under the floor does not degrade into a retry, it loops: every
// reclaim re-runs the same step and is cancelled at the same point, with no
// error ever reaching the session (#383). MaxTimeout is the step every
// deployment has — one `bash` call — and longestStep names any longer one a
// caller knows about, which for the executor is a repository clone: one clone
// is a single silent interval of exactly RepoCloneTimeout, an operator-raisable
// knob with no relation to this budget until it is given one. The steps only a
// deployment can measure — a cold image pull, a large checkpoint restore — stay
// its own to clear.
func StallFloor(longestStep time.Duration) time.Duration {
	if longestStep < MaxTimeout {
		longestStep = MaxTimeout
	}
	// A step within a minute of the largest representable duration would wrap
	// the sum negative, and a negative floor is one every budget clears: the
	// guard would not merely fail, it would invert, and RepoCloneTimeout takes
	// any duration that parses. Saturate instead — nothing clears a floor no
	// run can outlast, which is the honest answer for a step of that length.
	if longestStep > math.MaxInt64-time.Minute {
		return math.MaxInt64
	}
	return longestStep + time.Minute
}

// ParseStallTimeout reads a stall budget from the environment variable named by
// name, naming it in every refusal.
//
// Malformed fails startup rather than falling back to the default, and so does
// a non-positive value: it parses, but the consumer's own defaulting would
// replace it, so "-30m" or "0" would otherwise start a process whose bound is
// silently not the one configured. A typo and a negative are told apart —
// answering "30mm" with a complaint about its sign helps nobody. There is
// deliberately no off switch.
func ParseStallTimeout(name, value string, longestStep time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	if floor := StallFloor(longestStep); d < floor {
		return 0, fmt.Errorf("%s must be at least %s: one step of a healthy run may take %s, and a step that hits its cap still has to be killed and answer",
			name, floor, max(longestStep, MaxTimeout))
	}
	return d, nil
}

// CheckStallDefault refuses a default budget that the caller's longest step has
// outgrown, naming both knobs and the number that would work.
//
// The budget an operator does *not* set needs the same floor as the one they
// do, and it is the case that actually happens: raising a clone timeout for a
// monorepo is a thing an operator does, touching a knob about stalls is not, so
// the default would go on cancelling that clone on every reclaim — the exact
// loop the floor exists to prevent, and silently, since a default is never
// compared with anything (#383). Only a caller with a step an operator can
// lengthen needs this; a binary whose longest step is MaxTimeout cannot reach a
// floor its own default does not already clear.
func CheckStallDefault(name, stepName string, def, longestStep time.Duration) error {
	if floor := StallFloor(longestStep); def < floor {
		return fmt.Errorf("%s of %s needs %s set to at least %s: that step is a single silent interval, and the default %s would cancel it on every reclaim",
			stepName, longestStep, name, floor, def)
	}
	return nil
}
