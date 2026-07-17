package evals

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// TestEvals is the suite. One stack for the run, one session per task.
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

// runAndGrade drives one trial and grades it, guaranteeing the trial reaches the
// report even when it aborts.
//
// The abort matters because t.Fatal is runtime.Goexit: it unwinds the goroutine
// running deferred functions but nothing else. runTrial fatals on a drive
// failure (a turn that never goes idle — a timeout — or an API error), and a
// grader can fatal through a helper. A trailing recordTrial would be unwound
// past on either, dropping the trial from report.json entirely — and a timeout
// is both the likeliest real failure and the one triage most needs to see. The
// deferred record turns that silent drop into a recorded Platform failure; the
// `completed` flag distinguishes a clean finish from an abort so the abort is
// not misreported as a pass with no graders run.
func runAndGrade(t *testing.T, s *stack, task Task) {
	rec := &record{Task: task.ID}
	completed := false
	defer func() {
		if !completed {
			rec.Failures = append(rec.Failures, failure{
				Grader: "trial-aborted", Class: string(Platform),
				Error: "the trial aborted before grading finished (a drive timeout or API " +
					"error, or a grader's t.Fatal); see the go test output for the fatal error",
			})
		}
		rec.Pass = len(rec.Failures) == 0
		recordTrial(*rec)
	}()

	// runTrial stamps rec.Session as soon as the session exists, so even a drive
	// that fatals before returning leaves the record pointing at the session
	// whose container and logs hold the evidence.
	tr := runTrial(t, s, task, rec)
	rec.ElapsedMS = tr.Elapsed.Milliseconds()
	rec.ToolCalls = countToolUse(tr, "")
	rec.Tokens = sumTokens(tr)
	rec.events = tr.Events

	// Every grader runs even after one fails: a trial that stopped at the first
	// failure would report "the file was wrong" and hide that the session also
	// errored and the tokens were never counted. Triage wants the whole picture,
	// and the run has already been paid for.
	for _, g := range append(corePack(task), task.Graders...) {
		if err := g.Check(t, tr); err != nil {
			rec.Failures = append(rec.Failures, failure{
				Grader: g.Name, Class: string(g.Class), Error: err.Error(),
			})
			// The class leads the message: it is the first thing a reader needs
			// to know, because it decides whether this is a bug to fix or a
			// model to re-prompt.
			t.Errorf("[%s] %s: %v", g.Class, g.Name, err)
		}
	}
	completed = true
}

// sumTokens totals the trial's model spend from the transcript's
// span.model_request_end events — the same events (and the same accessor) the
// usage-accounted grader asserts are populated, so the report cannot show
// plausible numbers for a run whose accounting was broken.
func sumTokens(tr *Trial) tokens {
	var out tokens
	for _, ev := range eventsOfType(tr, "span.model_request_end") {
		in, o, ok := modelUsage(ev)
		if !ok {
			continue
		}
		out.Input += int64(in)
		out.Output += int64(o)
	}
	return out
}
