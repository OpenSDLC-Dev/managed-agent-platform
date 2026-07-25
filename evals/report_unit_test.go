package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// withScratchRecorder points the artifacts at a temp directory and empties the
// run-wide recorder, restoring both afterwards.
//
// The restore is not tidiness. These tests and a live eval run share one process
// when both are opted in, and recordTrial flushes on every trial: an unrestored
// artifactsDir would send that run's artifacts into a deleted temp directory
// (os.MkdirAll silently recreates it, so nothing would even complain), and
// unrestored records would seed its report.json with these fixtures' fake
// trials.
//
// Today nothing bites: TestEvals is the binary's first test and its trials are
// serial, so every live flush is done before these fixtures touch the recorder.
// That is an accident of declaration order, which Go guarantees nothing about —
// `-shuffle`, `-count` above 1, or a future recording test declared before this
// file is all it takes.
func withScratchRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldRep, oldSecrets, oldSwept := artifactsDir, recorder.rep, recorder.secrets, recorder.swept
	artifactsDir = dir
	recorder.rep, recorder.secrets, recorder.swept = report{}, nil, false
	t.Cleanup(func() {
		artifactsDir = oldDir
		recorder.rep, recorder.secrets, recorder.swept = oldRep, oldSecrets, oldSwept
	})
	return dir
}

// A run that wedges until `go test -timeout` fires never returns from m.Run, so
// TestMain's writeArtifacts is unreachable and the run most in need of evidence
// is the one that leaves none. The defence is that every trial's artifacts are
// already on disk by the time the next one starts, which is what this pins:
// recordTrial alone, with no end-of-run write, must leave a readable report.
func TestRecordTrialFlushesArtifactsWithoutTheEndOfRunWrite(t *testing.T) {
	dir := withScratchRecorder(t)

	recordTrial(record{Task: "first", Session: "sesn_1", Pass: true})
	recordTrial(record{
		Task: "second", Session: "sesn_2", Pass: false,
		Failures: []failure{{Grader: "g", Class: string(Platform), Error: "boom"}},
		events:   []map[string]any{{"type": "session.error"}},
	})

	rendered, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("no report.json after two recorded trials: %v", err)
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("report.json is missing trial %q: %s", want, rendered)
		}
	}
	if _, err := os.ReadFile(filepath.Join(dir, "summary.md")); err != nil {
		t.Errorf("no summary.md after two recorded trials: %v", err)
	}
	// The failed trial's transcript is the artifact triage actually opens.
	if _, err := os.ReadFile(filepath.Join(dir, "transcript-second-sesn_2.json")); err != nil {
		t.Errorf("no transcript for the failed trial: %v", err)
	}
}

// The transcript sweep clears a PRIOR run's leftovers, and must happen once per
// run rather than once per flush. Repeating it would delete transcripts already
// safely on disk at the start of every trial's flush — a window in which a run
// that wedged would lose exactly the evidence the per-trial flush is for. These
// two halves are one rule: the first flush sweeps, later flushes do not.
func TestTranscriptSweepHappensOncePerRun(t *testing.T) {
	dir := withScratchRecorder(t)
	stale := filepath.Join(dir, "transcript-gone-sesn_old.json")
	if err := os.WriteFile(stale, []byte(`["a prior run"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	recordTrial(record{Task: "first", Session: "sesn_1", Pass: true})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the first flush left a prior run's transcript in place (err=%v)", err)
	}

	// A file the sweep would take if it ran again. Nothing in a real run creates
	// one, which is the point: it stands in for a transcript an earlier trial in
	// THIS run had already written.
	survivor := filepath.Join(dir, "transcript-earlier-sesn_1.json")
	if err := os.WriteFile(survivor, []byte(`["this run"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordTrial(record{Task: "second", Session: "sesn_2", Pass: true})
	if _, err := os.Stat(survivor); err != nil {
		t.Errorf("a later flush swept a transcript this run had already written: %v", err)
	}
}

func TestScrubRedactsConfiguredSecrets(t *testing.T) {
	cfg := modeltest.Config{
		Protocol: "openai",
		BaseURL:  "https://user:s3cr3t@gw.example.com/v1?api_key=sk-url-key",
		APIKey:   "sk-header-key",
		Model:    "m",
	}
	secrets := secretsOf(cfg)
	// A transcript that quoted the request URL, the way a Go transport error to
	// an unreachable endpoint would, plus the header key echoed elsewhere.
	dirty := []byte(`{"error":"Post \"https://user:s3cr3t@gw.example.com/v1?api_key=sk-url-key\": dial tcp: refused","key":"sk-header-key"}`)
	got := string(scrub(dirty, secrets))
	for _, leak := range []string{"sk-url-key", "s3cr3t", "sk-header-key", "api_key=sk-url-key"} {
		if strings.Contains(got, leak) {
			t.Errorf("scrub left %q in the artifact: %q", leak, got)
		}
	}
	if !strings.Contains(got, "gw.example.com") {
		t.Errorf("scrub should keep the host visible: %q", got)
	}
}

func TestScrubSurvivesJSONEncoding(t *testing.T) {
	// The realistic path: a session.error quoting a URL whose multi-parameter
	// query carries the credential, rendered through the same no-HTML-escape
	// encoder the artifacts use. Default encoding/json turns the "&" separator
	// into &, which would leave the raw-substring scrub matching nothing.
	cfg := modeltest.Config{BaseURL: "http://127.0.0.1:1/v1?api_key=sk-url-key&tenant=acme"}
	events := []map[string]any{{
		"type":  "session.error",
		"error": map[string]any{"message": `Post "http://127.0.0.1:1/v1?api_key=sk-url-key&tenant=acme": dial refused`},
	}}
	rendered, err := marshalIndentJSON(events)
	if err != nil {
		t.Fatal(err)
	}
	got := string(scrub(rendered, secretsOf(cfg)))
	if strings.Contains(got, "sk-url-key") {
		t.Errorf("scrub left the query credential in JSON-encoded output: %q", got)
	}
}

func TestSecretsOfCoversMaskedUserinfo(t *testing.T) {
	// Go masks the password in transport errors (user:pass → user:***), so the
	// username must be scrubbed on its own to catch a credential-bearing user.
	cfg := modeltest.Config{BaseURL: "http://sk-user-token:pw@127.0.0.1:1"}
	masked := []byte(`{"error":"Post \"http://sk-user-token:***@127.0.0.1:1\": refused"}`)
	got := string(scrub(masked, secretsOf(cfg)))
	if strings.Contains(got, "sk-user-token") {
		t.Errorf("scrub left the masked-form username: %q", got)
	}
}

func TestSecretsOfMalformedURL(t *testing.T) {
	// An unparseable base URL yields no parts to pick out, so the whole raw
	// value is the secret — a transport error would quote it verbatim.
	cfg := modeltest.Config{BaseURL: "://user:pw@gw?api_key=url-secret"}
	dirty := []byte(`{"error":"parse \"://user:pw@gw?api_key=url-secret\": missing protocol scheme"}`)
	got := string(scrub(dirty, secretsOf(cfg)))
	for _, leak := range []string{"url-secret", "user:pw"} {
		if strings.Contains(got, leak) {
			t.Errorf("scrub left %q from a malformed base URL: %q", leak, got)
		}
	}
}

func TestEndpointHostHidesCredential(t *testing.T) {
	// The one property that matters: a base URL with a credential in it must
	// never survive into the report. Host is host:port only.
	cases := []struct {
		in   string
		want string
	}{
		{"https://user:secret@gw.example.com:8443/v1", "gw.example.com:8443"},
		{"https://gw.example.com/v1?api_key=sk-live-abc", "gw.example.com"},
		{"http://127.0.0.1:4000", "127.0.0.1:4000"},
		{"://nonsense", ""},
	}
	for _, c := range cases {
		got := endpointHost(c.in)
		if got != c.want {
			t.Errorf("endpointHost(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "sk-live") {
			t.Fatalf("endpointHost(%q) leaked a credential: %q", c.in, got)
		}
	}
}

func TestRenderSummaryCountsAndDetail(t *testing.T) {
	rep := report{
		Model:    "test-model",
		Endpoint: "gw.example.com:443",
		Records: []record{
			{Task: "fib-quickstart", Session: "sesn_a", Pass: true,
				ElapsedMS: 4200, ToolCalls: 3,
				Tokens: tokens{Input: 100, Output: 40}},
			{Task: "echo-notool", Session: "sesn_b", Pass: false,
				ElapsedMS: 1800, ToolCalls: 1,
				Tokens:   tokens{Input: 20, Output: 5},
				Failures: []failure{{Grader: "no-tool-use", Class: "M", Error: "1 tool call(s): bash"}}},
		},
	}
	out := renderSummary(rep)

	for _, want := range []string{
		"1/2 passed",
		"`test-model`",
		"`gw.example.com:443`",
		"| fib-quickstart | PASS |",
		"| echo-notool | FAIL |",
		"1×M",             // the failure-class tally in the table
		"## echo-notool",  // detail section for the failed trial only
		"[M] no-tool-use", // the failure line, class first
		"transcript-echo-notool-sesn_b.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
	// A passing trial gets no detail section.
	if strings.Contains(out, "## fib-quickstart") {
		t.Error("summary should not emit a detail section for a passing trial")
	}
	// Aggregate token line.
	if !strings.Contains(out, "120 in / 45 out") {
		t.Errorf("summary missing aggregate tokens\n---\n%s", out)
	}
}

func TestFailureClasses(t *testing.T) {
	if got := failureClasses(nil); got != "—" {
		t.Errorf("no failures = %q, want em-dash", got)
	}
	// Classes are sorted so the tally is stable across runs; M sorts before P.
	got := failureClasses([]failure{
		{Class: "P"}, {Class: "M"}, {Class: "P"},
	})
	if got != "1×M 2×P" {
		t.Errorf("failureClasses = %q, want \"1×M 2×P\"", got)
	}
}
