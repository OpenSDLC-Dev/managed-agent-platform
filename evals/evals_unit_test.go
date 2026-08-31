package evals

import (
	"encoding/json"
	"strings"
	"testing"
)

// The retry gate: a failed attempt earns the task's one retry exactly when the
// model alone is implicated — every failure Either or Model class. Any
// Platform-class failure is the signal the suite exists for and is never
// retried (plan 02's classing line), and a clean attempt has nothing to retry.
func TestRetryEarnedOnlyForModelClassFailures(t *testing.T) {
	f := func(class Class) failure { return failure{Grader: "g", Class: string(class), Error: "e"} }
	for name, tc := range map[string]struct {
		failures []failure
		want     bool
	}{
		"no failures":         {nil, false},
		"either only":         {[]failure{f(Either)}, true},
		"model only":          {[]failure{f(Model)}, true},
		"either and model":    {[]failure{f(Either), f(Model)}, true},
		"platform only":       {[]failure{f(Platform)}, false},
		"platform among them": {[]failure{f(Either), f(Platform)}, false},
	} {
		if got := retryEarned(tc.failures); got != tc.want {
			t.Errorf("%s: retryEarned = %v, want %v", name, got, tc.want)
		}
	}
}

// A retried task appears as two records; the headline count must judge the
// final attempts only, while the spend lines still charge both — the run paid
// for both. The superseded attempt keeps its detail section: the failed first
// try's evidence is what triage reads.
func TestRenderSummaryReportsRetriedAttempts(t *testing.T) {
	rep := report{
		Model:    "test-model",
		Endpoint: "gw.example.com:443",
		Records: []record{
			{Task: "repo-answer", Session: "sesn_a1", Pass: false, Attempt: 1,
				ElapsedMS: 2000, Tokens: tokens{Input: 100, Output: 10},
				Failures: []failure{{Grader: "repo-passphrase-answered", Class: "E", Error: "refused"}}},
			{Task: "repo-answer", Session: "sesn_a2", Pass: true, Attempt: 2,
				ElapsedMS: 3000, Tokens: tokens{Input: 110, Output: 12}},
			{Task: "echo-notool", Session: "sesn_b", Pass: true,
				ElapsedMS: 1000, Tokens: tokens{Input: 20, Output: 5}},
		},
	}
	out := renderSummary(rep)

	for _, want := range []string{
		"2/2 passed", // final attempts only: the superseded attempt is no trial of its own
		"| repo-answer | FAIL (retried) |",
		"| repo-answer | PASS (retry) |",
		"| echo-notool | PASS |",
		"230 in / 27 out", // both attempts' spend counts
		"## repo-answer (session `sesn_a1`, attempt 1 — retried)",
		"[E] repo-passphrase-answered",
		"transcript-repo-answer-sesn_a1.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
	// The passing retry gets no detail section, like any passing record.
	if strings.Contains(out, "## repo-answer (session `sesn_a2`") {
		t.Error("summary should not emit a detail section for the passing retry")
	}
}

// A failed retry is judged like any failed trial, and its detail names the
// attempt so the two same-task sections read apart.
func TestRenderSummaryFailedRetry(t *testing.T) {
	rep := report{Records: []record{
		{Task: "mcp-answer", Session: "sesn_c1", Pass: false, Attempt: 1,
			Failures: []failure{{Grader: "final-message-has:x", Class: "E", Error: "empty"}}},
		{Task: "mcp-answer", Session: "sesn_c2", Pass: false, Attempt: 2,
			Failures: []failure{{Grader: "final-message-has:x", Class: "E", Error: "refused"}}},
	}}
	out := renderSummary(rep)
	for _, want := range []string{
		"0/1 passed",
		"| mcp-answer | FAIL (retried) |",
		"| mcp-answer | FAIL (retry) |",
		"## mcp-answer (session `sesn_c1`, attempt 1 — retried)",
		"## mcp-answer (session `sesn_c2`, attempt 2 — the retry)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

// attempt stays off the wire for the common case: a task that ran once
// serializes exactly as before the field existed.
func TestRecordAttemptOmittedWhenZero(t *testing.T) {
	plain, err := json.Marshal(record{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "attempt") {
		t.Errorf("a sole attempt should not serialize an attempt field: %s", plain)
	}
	retried, err := json.Marshal(record{Task: "t", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(retried), `"attempt":1`) {
		t.Errorf("a superseded attempt should serialize attempt:1: %s", retried)
	}
}
