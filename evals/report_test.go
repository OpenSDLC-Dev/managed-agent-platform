package evals

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// The run's artifacts. `go test` output says which task failed; this says why,
// in a form someone can read after the fact without re-running (and re-paying
// for) the suite. That is the discipline the evals writeup argues for: the
// transcript is the primary artifact, and a summary nobody can inspect is a
// score, not evidence.
const artifactsDir = "artifacts"

// failure is one grader's verdict on one trial.
type failure struct {
	Grader string `json:"grader"`
	Class  string `json:"class"`
	Error  string `json:"error"`
}

// tokens is a trial's model spend.
type tokens struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
}

// record is one trial's outcome.
type record struct {
	Task      string    `json:"task"`
	Session   string    `json:"session"`
	Pass      bool      `json:"pass"`
	ElapsedMS int64     `json:"elapsed_ms"`
	ToolCalls int       `json:"tool_calls"`
	Tokens    tokens    `json:"tokens"`
	Failures  []failure `json:"failures,omitempty"`

	// events is the whole transcript, dumped to its own file when the trial
	// fails. Unexported so it stays out of report.json, which is meant to stay
	// small enough to read.
	events []map[string]any
}

// report is what a run produces.
type report struct {
	// Model is MODEL_ID, and Endpoint is the endpoint's host and port only.
	//
	// Host only, deliberately: MODEL_BASE_URL may carry a credential in its
	// userinfo or query string, and this file is written to disk and pasted into
	// issues. url.URL.Host excludes both, so the report can say which endpoint
	// was exercised without ever being able to leak the key that reached it.
	Model    string   `json:"model"`
	Endpoint string   `json:"endpoint"`
	Records  []record `json:"records"`
}

// recorder collects trial outcomes across the run. Guarded because trials are
// serial today but nothing in the design requires that, and a data race
// discovered by a future -parallel would be a maddening way to learn it.
var recorder struct {
	mu      sync.Mutex
	rep     report
	secrets []string // scrubbed from every artifact before it is written
}

func recordMeta(cfg modeltest.Config) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.rep.Model = cfg.Model
	recorder.rep.Endpoint = endpointHost(cfg.BaseURL)
	recorder.secrets = secretsOf(cfg)
}

func recordTrial(rec record) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.rep.Records = append(recorder.rep.Records, rec)
}

// endpointHost reduces a base URL to host:port, dropping userinfo, path and
// query — see report.Endpoint. An unparseable URL yields "" rather than the raw
// string: the fallback must not be "print whatever was in the variable".
func endpointHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// secretsOf is the set of substrings that must never reach an on-disk artifact.
//
// The endpoint field is already reduced to host:port, but a transcript is not:
// a failed model request lands a session.error on the log, and Go's transport
// error quotes the request URL (net/http's stripPassword redacts only the
// userinfo password, never the query string). So a credential in
// MODEL_BASE_URL's query — some gateways take ?api_key=… — would otherwise ride
// that error into report.json, summary.md and the failed-trial transcript, all
// of which get pasted into issues. The API key is header-only, and the adapters
// now redact it from anything an endpoint echoes back, but it is scrubbed here
// too as defence in depth — this suite must not depend on that. Every non-empty
// piece is replaced before any artifact is written (see scrub); artifacts are rendered
// without HTML escaping so an "&" in a multi-parameter query stays literal and
// the raw substring still matches.
//
// The userinfo is captured three ways because Go's error masks the password
// (user:pass → user:***): the full pair covers a raw appearance, the username
// survives the masking, and the password is scrubbed for the raw case too. A
// base URL that will not parse yields no parts to pick out, so the whole raw
// value is treated as the secret — a transport error would quote it verbatim,
// and the endpoint field is already blank because endpointHost fails the same
// parse.
func secretsOf(cfg modeltest.Config) []string {
	var s []string
	add := func(v string) {
		if v != "" {
			s = append(s, v)
		}
	}
	add(cfg.APIKey)
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		add(cfg.BaseURL)
		return s
	}
	add(u.RawQuery)
	if u.User != nil {
		add(u.User.String())
		add(u.User.Username())
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
	}
	return s
}

// marshalIndentJSON renders v as indented JSON without HTML-escaping &, < and >.
// That escaping exists for JSON embedded in a web page; these artifacts are read
// as files, and it would only defeat the scrub, which matches raw substrings — a
// query credential's "&" separator would become & and slip past. Encode
// appends the trailing newline the files want.
func marshalIndentJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// scrub replaces every known secret substring with a redaction marker. It runs
// over the fully rendered bytes of each artifact, so it catches a credential
// wherever it surfaced — an error string, a nested event, a URL — rather than
// trusting each call site to have redacted its own.
//
// Residual: it matches raw substrings, so a secret containing a character JSON
// must escape (a `"` or `\`) could still slip the match inside a JSON artifact
// even with HTML escaping off. API keys and query strings essentially never
// carry those; the realistic separators (`&`, `<`, `>`) are handled by
// marshalIndentJSON.
func scrub(b []byte, secrets []string) []byte {
	s := string(b)
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return []byte(s)
}

// writeArtifacts renders the run to evals/artifacts/. Errors are returned rather
// than fataled: the tests' own verdict is the run's verdict, and a report that
// could not be written must not turn a green run red.
func writeArtifacts() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	// Nothing recorded means an offline run (the live suite skipped before it
	// could record a meta line or a trial): write no artifacts.
	if len(recorder.rep.Records) == 0 {
		return nil
	}
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return err
	}
	// Clear a prior run's per-failure transcripts so the directory reflects only
	// this run. report.json and summary.md are overwritten below, but transcripts
	// are named per session and would otherwise accumulate — a stale one sitting
	// beside a fresh green summary reads as a failure of the run that just passed.
	if old, err := filepath.Glob(filepath.Join(artifactsDir, "transcript-*.json")); err == nil {
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	// Every artifact is scrubbed of known secrets on its way to disk (see
	// secretsOf): the scrub runs over the final rendered bytes, so a credential
	// is caught wherever it surfaced rather than at each call site.
	raw, err := marshalIndentJSON(recorder.rep)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "report.json"),
		scrub(raw, recorder.secrets), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "summary.md"),
		scrub([]byte(renderSummary(recorder.rep)), recorder.secrets), 0o644); err != nil {
		return err
	}
	// One transcript per failed trial. Only failures, because a passing
	// trial's transcript is a few hundred KB nobody will read, and the point is
	// to make the failures inspectable.
	for _, rec := range recorder.rep.Records {
		if rec.Pass || rec.events == nil {
			continue
		}
		dump, err := marshalIndentJSON(rec.events)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("transcript-%s-%s.json", rec.Task, rec.Session)
		if err := os.WriteFile(filepath.Join(artifactsDir, name),
			scrub(dump, recorder.secrets), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// renderSummary is a pure function of the report, which is what lets it be
// tested without a model, a database or Docker.
func renderSummary(rep report) string {
	var b strings.Builder
	pass := 0
	var totalMS int64
	var tok tokens
	for _, r := range rep.Records {
		if r.Pass {
			pass++
		}
		totalMS += r.ElapsedMS
		tok.Input += r.Tokens.Input
		tok.Output += r.Tokens.Output
	}

	fmt.Fprintf(&b, "# Eval run\n\n")
	fmt.Fprintf(&b, "- Model: `%s`\n", rep.Model)
	fmt.Fprintf(&b, "- Endpoint: `%s`\n", rep.Endpoint)
	fmt.Fprintf(&b, "- Result: **%d/%d passed**\n", pass, len(rep.Records))
	fmt.Fprintf(&b, "- Elapsed: %.1fs · Tokens: %d in / %d out\n\n",
		float64(totalMS)/1000, tok.Input, tok.Output)

	fmt.Fprintf(&b, "| Task | Result | Time | Tools | Failures |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|\n")
	for _, r := range rep.Records {
		result := "PASS"
		if !r.Pass {
			result = "FAIL"
		}
		classes := failureClasses(r.Failures)
		fmt.Fprintf(&b, "| %s | %s | %.1fs | %d | %s |\n",
			r.Task, result, float64(r.ElapsedMS)/1000, r.ToolCalls, classes)
	}

	// The detail section only lists failures — a green run's summary is the
	// table and nothing else.
	for _, r := range rep.Records {
		if r.Pass {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (session `%s`)\n\n", r.Task, r.Session)
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "- **[%s] %s** — %s\n", f.Class, f.Grader, f.Error)
		}
		fmt.Fprintf(&b, "\nTranscript: `transcript-%s-%s.json`\n", r.Task, r.Session)
	}
	return b.String()
}

// failureClasses summarizes a trial's failures as a class tally, so the table
// answers the only question worth asking of a red row: is this our bug (P) or
// the model wandering (M)?
func failureClasses(fs []failure) string {
	if len(fs) == 0 {
		return "—"
	}
	n := map[string]int{}
	for _, f := range fs {
		n[f.Class]++
	}
	classes := make([]string, 0, len(n))
	for c := range n {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%d×%s", n[c], c))
	}
	return strings.Join(parts, " ")
}
