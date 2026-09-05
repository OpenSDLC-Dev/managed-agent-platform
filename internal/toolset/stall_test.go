package toolset_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// TestStallFloorRisesWithTheLongestStep: the floor exists to keep a budget from
// sitting under a single silent step, so it has to follow the longest step the
// caller knows about — not just the one every deployment has. The executor's
// repository clone is that other step: one clone is a single silent interval of
// exactly RepoCloneTimeout, an operator-raisable knob (#383).
func TestStallFloorRisesWithTheLongestStep(t *testing.T) {
	if got, want := toolset.StallFloor(0), toolset.MaxTimeout+time.Minute; got != want {
		t.Errorf("StallFloor(0) = %s, want %s (one bash tool plus the kill and the answer)", got, want)
	}
	if got, want := toolset.StallFloor(time.Minute), toolset.MaxTimeout+time.Minute; got != want {
		t.Errorf("StallFloor(1m) = %s, want %s (a shorter step cannot lower the floor)", got, want)
	}
	if got, want := toolset.StallFloor(45*time.Minute), 46*time.Minute; got != want {
		t.Errorf("StallFloor(45m) = %s, want %s (a longer step raises it)", got, want)
	}
}

// TestStallFloorSaturatesRatherThanWrapping: the floor adds a minute to a step
// whose length an operator sets, and time.ParseDuration accepts every duration
// up to the largest int64 of nanoseconds — so a step within a minute of that
// wraps the sum negative, and a negative floor is one every budget clears. The
// guard would not fail loudly, it would invert: the check reads as satisfied and
// the default goes on cancelling the step on every reclaim (#383).
func TestStallFloorSaturatesRatherThanWrapping(t *testing.T) {
	maxDur := time.Duration(math.MaxInt64)
	for _, step := range []time.Duration{maxDur, maxDur - time.Second, maxDur - time.Minute + 1} {
		if floor := toolset.StallFloor(step); floor < step {
			t.Errorf("StallFloor(%s) = %s, want a floor no lower than the step itself", step, floor)
		}
	}
	// And the refusals built on it stay refusals rather than becoming a pass.
	if err := toolset.CheckStallDefault("STALL", "STEP", 30*time.Minute, maxDur); err == nil {
		t.Error("CheckStallDefault(30m default, max-duration step) = nil, want a refusal")
	}
	if _, err := toolset.ParseStallTimeout("STALL", "30m", maxDur); err == nil {
		t.Error("ParseStallTimeout(30m, max-duration step) = nil error, want a refusal")
	}
}

// TestCheckStallDefaultGuardsTheBudgetNobodyTyped: the floor has to hold for the
// default too, and that is the case that actually happens — raising a clone
// timeout for a monorepo is a thing an operator does, and touching a knob about
// stalls is not.
func TestCheckStallDefaultGuardsTheBudgetNobodyTyped(t *testing.T) {
	const def = 30 * time.Minute
	if err := toolset.CheckStallDefault("STALL", "STEP", def, 45*time.Minute); err == nil {
		t.Error("CheckStallDefault(30m default, 45m step) = nil, want a refusal")
	} else {
		for _, want := range []string{"STALL", "STEP", "46m0s", "45m0s", "30m0s"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to carry %q", err, want)
			}
		}
	}
	// Exactly clearing the floor starts, as does a step the default already
	// covers and the plain deployment that names no step at all.
	for _, step := range []time.Duration{29 * time.Minute, 5 * time.Minute, 0} {
		if err := toolset.CheckStallDefault("STALL", "STEP", def, step); err != nil {
			t.Errorf("CheckStallDefault(30m default, %s step) = %v, want it accepted", step, err)
		}
	}
}

// TestParseStallTimeoutRefusals pins each refusal apart from the others: an
// operator who typed "30mm" must not be told about its sign, and one who set a
// budget under a single step must be told the number that would work.
func TestParseStallTimeoutRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       string
		longestStep time.Duration
		want        string // a substring the refusal must carry
	}{
		{"malformed", "30mm", 0, "must be a Go duration"},
		{"negative", "-30m", 0, "must be a positive Go duration"},
		{"zero", "0", 0, "must be a positive Go duration"},
		{"under the floor", "10m59s", 0, "must be at least 11m0s"},
		{"under a raised floor", "30m", 45 * time.Minute, "must be at least 46m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := toolset.ParseStallTimeout("STALL", tc.value, tc.longestStep)
			if err == nil {
				t.Fatalf("ParseStallTimeout(%q) = nil error, want a refusal", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to carry %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "STALL ") {
				t.Errorf("refusal = %q, want it to name the variable it is about", err)
			}
		})
	}
}

// TestParseStallTimeoutAcceptsTheFloorItself: the floor is a minimum, not an
// exclusive bound — a deployment that computed it and set exactly that must
// start.
func TestParseStallTimeoutAcceptsTheFloorItself(t *testing.T) {
	for _, tc := range []struct {
		value       string
		longestStep time.Duration
		want        time.Duration
	}{
		{"11m", 0, 11 * time.Minute},
		{"30m", 0, 30 * time.Minute},
		{"46m", 45 * time.Minute, 46 * time.Minute},
	} {
		got, err := toolset.ParseStallTimeout("STALL", tc.value, tc.longestStep)
		if err != nil {
			t.Errorf("ParseStallTimeout(%q, longest=%s) = %v, want it accepted", tc.value, tc.longestStep, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseStallTimeout(%q) = %s, want %s", tc.value, got, tc.want)
		}
	}
}

// TestLongestStepNamesTheKnobThatSetTheFloor: the refusals name one knob, so a
// caller with several operator-raisable steps has to hand over the one that
// actually raised the floor — telling an operator about their clone budget
// when it was the package-install budget that pushed the floor past their
// stall setting is a refusal they cannot act on (plan 40 decision 5).
func TestLongestStepNamesTheKnobThatSetTheFloor(t *testing.T) {
	clone := toolset.NamedStep{Name: "EXECUTOR_REPO_CLONE_TIMEOUT", D: 5 * time.Minute}
	install := toolset.NamedStep{Name: "EXECUTOR_PACKAGE_INSTALL_TIMEOUT", D: 45 * time.Minute}
	if got := toolset.LongestStep(clone, install); got != install {
		t.Errorf("LongestStep(5m clone, 45m install) = %+v, want the install", got)
	}
	if got := toolset.LongestStep(install, clone); got != install {
		t.Errorf("LongestStep(45m install, 5m clone) = %+v, want the install whatever the order", got)
	}
	// A tie goes to the earlier step: both are equally the floor, and picking
	// by argument order at least makes the refusal reproducible.
	same := toolset.NamedStep{Name: "OTHER", D: 5 * time.Minute}
	if got := toolset.LongestStep(clone, same); got != clone {
		t.Errorf("LongestStep(tie) = %+v, want the first of the two", got)
	}
	// One step, and one that is not set at all: still answered by name, so a
	// caller never has to special-case the count.
	if got := toolset.LongestStep(clone); got != clone {
		t.Errorf("LongestStep(one) = %+v, want it back", got)
	}
	unset := toolset.NamedStep{Name: "UNSET"}
	if got := toolset.LongestStep(unset); got != unset {
		t.Errorf("LongestStep(zero-duration step) = %+v, want it back named", got)
	}
}
