package evals

import (
	"testing"
)

// The retry gate: a failed attempt earns the task's one retry exactly when the
// model alone is implicated — every failure Either or Model class. Any other
// class is a reason to stand: Platform is the signal the suite exists for
// (plan 02's classing line), and an empty or unknown class is a harness bug,
// not a licence to reroll it.
func TestRetryEarnedOnlyForModelClassFailures(t *testing.T) {
	f := func(class string) failure { return failure{Grader: "g", Class: class, Error: "e"} }
	for name, tc := range map[string]struct {
		failures []failure
		want     bool
	}{
		"no failures":         {nil, false},
		"either only":         {[]failure{f("E")}, true},
		"model only":          {[]failure{f("M")}, true},
		"either and model":    {[]failure{f("E"), f("M")}, true},
		"platform only":       {[]failure{f("P")}, false},
		"platform among them": {[]failure{f("E"), f("P")}, false},
		"an empty class":      {[]failure{f("")}, false},
		"an unknown class":    {[]failure{f("E"), f("X")}, false},
	} {
		if got := retryEarned(tc.failures); got != tc.want {
			t.Errorf("%s: retryEarned = %v, want %v", name, got, tc.want)
		}
	}
}

// The retry flow itself, offline through verdict's grade seam: a first attempt
// failing on model behavior is recorded as the superseded attempt 1 (flushed
// before the retry starts), the retry runs exactly once as attempt 2, and a
// passing retry greens the task — this test reddening is itself the assertion
// that the superseded failures go to the log, not to t.Errorf.
func TestVerdictRecordsBothAttemptsOfARetriedTask(t *testing.T) {
	withScratchRecorder(t)

	var calls []int
	verdict(t, func(attempt int) (*record, []failure) {
		calls = append(calls, attempt)
		if len(calls) == 1 {
			fs := []failure{{Grader: "final-message-has:x", Class: "E", Error: "refused"}}
			return &record{Task: "task", Session: "sesn_1", Attempt: attempt, Failures: fs}, fs
		}
		return &record{Task: "task", Session: "sesn_2", Attempt: attempt}, nil
	})

	if len(calls) != 2 || calls[0] != 0 || calls[1] != 2 {
		t.Fatalf("grade calls = %v, want [0 2] — one first attempt, one retry", calls)
	}
	recs := recorder.rep.Records
	if len(recs) != 2 {
		t.Fatalf("recorded %d records, want the superseded attempt and the retry", len(recs))
	}
	if recs[0].Attempt != 1 || recs[0].Pass || recs[0].Session != "sesn_1" {
		t.Errorf("first record = %+v, want the superseded attempt 1, failed", recs[0])
	}
	if recs[1].Attempt != 2 || !recs[1].Pass || recs[1].Session != "sesn_2" {
		t.Errorf("second record = %+v, want the passing attempt 2", recs[1])
	}
}

// The common path stays the common shape: a clean first attempt records once,
// as the sole attempt, and never consults the retry.
func TestVerdictCleanAttemptRecordsOnce(t *testing.T) {
	withScratchRecorder(t)

	var calls []int
	verdict(t, func(attempt int) (*record, []failure) {
		calls = append(calls, attempt)
		return &record{Task: "task", Session: "sesn_1", Attempt: attempt}, nil
	})

	if len(calls) != 1 {
		t.Fatalf("grade calls = %v, want exactly one attempt", calls)
	}
	recs := recorder.rep.Records
	if len(recs) != 1 || recs[0].Attempt != 0 || !recs[0].Pass {
		t.Fatalf("records = %+v, want one passing sole-attempt record", recs)
	}
}
