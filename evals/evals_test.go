package evals

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// TestEvals is the suite. One stack for the run, one session per attempt —
// which is one per task, except the retried task that used two.
//
// The gate is modeltest.Endpoint rather than a bare env check, so an opted-in
// run with a rotted .env fails here instead of skipping — a suite that quietly
// skips itself when its credentials expire is not a safety net.
//
// Trials run serially. That is a real cost (minutes, not seconds) and it buys
// two things worth more: a rate-limited endpoint cannot turn a product
// regression into a flake, and a failed run's Docker and log output belong to
// one trial, which is what makes the transcripts readable.
func TestEvals(t *testing.T) {
	cfg := modeltest.Endpoint(t, modeltest.EvalsEnv)
	recordMeta(cfg)

	s := newStack(t, cfg)
	for _, task := range tasks() {
		t.Run(task.ID, func(t *testing.T) {
			runAndGrade(t, s, task)
		})
	}
}

// runAndGrade drives a task to its verdict: one trial, and — when the first
// attempt fails on model behavior alone — the one retry plan 02 reserves for
// model non-compliance ([M], extended to [E] since the evidence cannot separate
// the two either), reported rather than silent. The superseded attempt keeps
// its record, its transcript and a log line; only the fresh attempt's verdict
// reds the run. Any Platform-class failure is the signal this suite exists for
// and is never retried, and an abort (a t.Fatal) has already reddened the test
// before any retry could be weighed.
func runAndGrade(t *testing.T, s *stack, task Task) {
	rec, failures := gradeAttempt(t, s, task, 0)
	if !retryEarned(failures) {
		rec.Pass = len(failures) == 0
		recordTrial(*rec)
		failFor(t, failures)
		return
	}
	rec.Attempt, rec.Pass = 1, false
	recordTrial(*rec)
	for _, f := range failures {
		t.Logf("attempt 1 [%s] %s: %s — model non-compliance, retrying", f.Class, f.Grader, f.Error)
	}
	retry, failures := gradeAttempt(t, s, task, 2)
	retry.Pass = len(failures) == 0
	recordTrial(*retry)
	failFor(t, failures)
}

// retryEarned is the retry gate: at least one failure, none of them Platform
// class. A clean attempt has nothing to retry, and a Platform failure must
// stand exactly once — retrying our own bug would be measuring the dice
// instead of the platform.
func retryEarned(fs []failure) bool {
	if len(fs) == 0 {
		return false
	}
	for _, f := range fs {
		if f.Class == string(Platform) {
			return false
		}
	}
	return true
}

// failFor reds the test with the class-first line per failure — the terminal
// spelling of what the record already carries. The class leads the message: it
// is the first thing a reader needs to know, because it decides whether this
// is a bug to fix or a model to re-prompt.
func failFor(t *testing.T, fs []failure) {
	for _, f := range fs {
		t.Errorf("[%s] %s: %s", f.Class, f.Grader, f.Error)
	}
}

// gradeAttempt drives one trial and grades it, returning the failures for the
// caller to judge — red the run, or spend the task's one retry — and
// guaranteeing the trial reaches the report even when it aborts. attempt is
// stamped into the record up front (0 for a first attempt; the caller upgrades
// it to 1 only if a retry supersedes it) so an abort mid-retry still records
// which attempt it was.
//
// The abort matters because t.Fatal is runtime.Goexit: it unwinds the goroutine
// running deferred functions but nothing else. runTrial fatals on a drive
// failure (a turn that never goes idle — a timeout — or an API error), and a
// grader can fatal through a helper. On either, the caller never gets to record
// the attempt — the unwind skips it — so the deferred record turns that silent
// drop into a recorded Platform failure instead; the `completed` flag is what
// tells the abort apart from a clean finish whose recording is the caller's.
//
// On an abort the happy-path transcript capture below is skipped too, so the
// defer fetches the transcript best-effort: a drive timeout is the failure the
// artifacts most need to make inspectable, and without this its transcript would
// be dropped (writeArtifacts skips a record whose events are nil). The fetch is
// non-fatal because it runs inside the unwinding t.Fatal.
func gradeAttempt(t *testing.T, s *stack, task Task, attempt int) (*record, []failure) {
	rec := &record{Task: task.ID, Attempt: attempt}
	completed := false
	defer func() {
		if completed {
			return
		}
		rec.Failures = append(rec.Failures, failure{
			Grader: "trial-aborted", Class: string(Platform),
			Error: "the trial aborted before grading finished (a drive timeout or API " +
				"error, or a grader's t.Fatal); see the go test output for the fatal error",
		})
		if rec.Session != "" && rec.events == nil {
			rec.events = s.tryListEvents(rec.Session)
		}
		rec.Pass = false
		recordTrial(*rec)
	}()

	// runTrial stamps rec.Session as soon as the session exists, so even a drive
	// that fatals before returning leaves the record pointing at the session
	// whose container and logs hold the evidence.
	tr := runTrial(t, s, task, rec)
	rec.ElapsedMS = tr.Elapsed.Milliseconds()
	// Both families: an MCP call is a tool call to anyone reading this report,
	// and the one trial whose whole point is a tool call would otherwise read zero.
	rec.ToolCalls = countToolUse(tr, "") + len(eventsOfType(tr, "agent.mcp_tool_use"))
	rec.Tokens = sumTokens(tr)
	rec.events = tr.Events

	// Every grader runs even after one fails: a trial that stopped at the first
	// failure would report "the file was wrong" and hide that the session also
	// errored and the tokens were never counted. Triage wants the whole picture,
	// and the run has already been paid for.
	var failures []failure
	for _, g := range append(corePack(task), task.Graders...) {
		if err := g.Check(t, tr); err != nil {
			failures = append(failures, failure{
				Grader: g.Name, Class: string(g.Class), Error: err.Error(),
			})
		}
	}
	rec.Failures = failures
	completed = true
	return rec, failures
}

// sumTokens totals the trial's model spend from the transcript's
// span.model_request_end events — the same events (and the same accessor) the
// usage-accounted grader asserts are populated, so the report cannot show
// plausible numbers for a run whose accounting was broken. Grader calls are
// the one model spend that rides no span.model_request_end: their usage is on
// span.outcome_evaluation_end (under "usage", not "model_usage"), so outcome
// trials count both or the report hides their most expensive calls.
func sumTokens(tr *Trial) tokens {
	var out tokens
	add := func(typ, key string) {
		for _, ev := range eventsOfType(tr, typ) {
			in, o, ok := usageAt(ev, key)
			if !ok {
				continue
			}
			out.Input += int64(in)
			out.Output += int64(o)
		}
	}
	add("span.model_request_end", "model_usage")
	add("span.outcome_evaluation_end", "usage")
	return out
}
