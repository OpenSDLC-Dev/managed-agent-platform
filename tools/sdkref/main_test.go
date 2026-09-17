package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheCorpusPassesFail is plan 51's gate: rungs 1 and 2 over the registry and
// the Go comments, through the -fail a developer runs, so the gate and the
// command line cannot disagree about what passes. Exit 2 fails it as surely as
// exit 1 — a run that could not open a cited module judged nothing it cites.
func TestTheCorpusPassesFail(t *testing.T) {
	var out, errOut strings.Builder
	if code := run([]string{"-root", repoRoot(t), "-fail"}, &out, &errOut); code != 0 {
		t.Errorf("sdkref -fail exited %d over the corpus; each finding names the edit it "+
			"needs:\n%s%s", code, out.String(), errOut.String())
	}
}

// TestFailChecksBothRungs holds the command line to what its own help text says.
// The probe document is shape-clean on purpose: its only defect is that the
// symbol does not resolve at the pin, so a -fail that ran shape alone would exit
// 0 over a corpus whose every anchor had stopped resolving — and this flag is
// what TestTheCorpusPassesFail holds the real corpus to.
func TestFailChecksBothRungs(t *testing.T) {
	root := repoRoot(t)
	// The probe is stamped at whatever go.mod pins today. Written as a literal
	// version, the next bump would make it a tag rung 2 skips — and this test
	// would fail on the tool doing exactly what it should.
	sdk, err := Module(root, SDKModule)
	if err != nil {
		t.Fatalf("resolving the pin offline: %v", err)
	}
	probe := filepath.Join(t.TempDir(), "probe.md")
	body := "- **probe** — *Evidence: checked against anthropic-sdk-go " +
		sdk.Version + " — betaagent.go NoSuchSymbolAnywhereInTheSDK.*\n"
	if err := os.WriteFile(probe, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"-root", root, "-file", probe, "-comments=false"}

	var out strings.Builder
	if code := run(base, &out, io.Discard); code != 0 {
		t.Errorf("rung 1 alone exited %d over a shape-clean document:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 citation(s) in the grammar, 0 finding(s)") {
		t.Errorf("rung 1 did not read the probe as one clean citation:\n%s", out.String())
	}

	out.Reset()
	if code := run(append(base, "-fail"), &out, io.Discard); code == 0 {
		t.Errorf("-fail exited 0, but the citation's symbol does not resolve at the pin. "+
			"The help text promises rungs 1 and 2; this ran only rung 1.\n%s", out.String())
	}
	if !strings.Contains(out.String(), "vanished-at-stamp") {
		t.Errorf("-fail printed no rung 2 finding:\n%s", out.String())
	}
}

// TestReportFailsOnlyOnAnUndispositionedTransition is the exit code the bump
// workflow reads. The probe's anchor is stamped before the pin and names nothing
// the SDK declares, so the pin reports it gone; with its disposition beside it,
// the same anchor has been read. Rung 2 judges neither, since neither anchor it
// could fail on is stamped at the pin and wrong — which is also why the gate's
// -fail passes both: a transition is not the gate's to read.
func TestReportFailsOnlyOnAnUndispositionedTransition(t *testing.T) {
	root := repoRoot(t)
	sdk, err := Module(root, SDKModule)
	if err != nil {
		t.Fatalf("resolving the pin offline: %v", err)
	}
	const anchor = "checked against anthropic-sdk-go v1.0.0 — betaagent.go NoSuchSymbolAnywhereInTheSDK"
	for _, tc := range []struct {
		name, evidence string
		want           int
	}{
		{"awaiting a disposition", anchor, 1},
		{"dispositioned", anchor + " and absent at anthropic-sdk-go " + sdk.Version +
			" — betaagent.go NoSuchSymbolAnywhereInTheSDK", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := filepath.Join(t.TempDir(), "probe.md")
			body := "- **probe** — *Evidence: " + tc.evidence + ".*\n"
			if err := os.WriteFile(probe, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			base := []string{"-root", root, "-file", probe, "-comments=false"}
			var out, errOut strings.Builder
			if code := run(append(base, "-report"), &out, &errOut); code != tc.want {
				t.Errorf("-report exited %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, tc.want, out.String(), errOut.String())
			}
			out.Reset()
			if code := run(append(base, "-fail"), &out, io.Discard); code != 0 {
				t.Errorf("-fail exited %d, want 0: the gate does not read transitions\n%s",
					code, out.String())
			}
		})
	}
}

// TestReportDoesNotPassOverASourceItCouldNotOpen. -report's exit 0 says no
// transition awaits a disposition, which it cannot say of a source it never
// opened: the go-jose citation below reaches no rung that could find one.
func TestReportDoesNotPassOverASourceItCouldNotOpen(t *testing.T) {
	root, _ := probeModule(t, "- **probe** — *Evidence: checked against go-jose v4.0.5 — jwk.go JSONWebKey.*\n")
	var out, errOut strings.Builder
	code := run([]string{"-root", root, "-file", "probe.md", "-comments=false", "-report"}, &out, &errOut)
	if code != exitUnavailable {
		t.Errorf("-report exited %d over a citation of a source it could not open, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitUnavailable, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "go-jose") {
		t.Errorf("stderr does not name the source that went unchecked:\n%s", errOut.String())
	}
}

// TestHelpIsNotAFailure. Exit 2 means "I could not look", and a caller that
// asked for the usage and got it has nothing a script should branch on.
func TestHelpIsNotAFailure(t *testing.T) {
	var errOut strings.Builder
	if code := run([]string{"-h"}, io.Discard, &errOut); code != 0 {
		t.Errorf("-h exited %d, want 0", code)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("-h printed no usage:\n%s", errOut.String())
	}
	if code := run([]string{"-no-such-flag"}, io.Discard, io.Discard); code != exitUnavailable {
		t.Errorf("an unknown flag exited %d, want %d", code, exitUnavailable)
	}
}

// TestAMissingDocumentIsNotACleanRun. Exit 2 is reserved for "I could not look",
// so that a run whose input was missing cannot be read as a run that found
// nothing wrong.
func TestAMissingDocumentIsNotACleanRun(t *testing.T) {
	var out, errOut strings.Builder
	code := run([]string{"-root", repoRoot(t), "-file", "docs/no-such-file.md"}, &out, &errOut)
	if code != exitUnavailable {
		t.Errorf("run over a missing document exited %d, want %d", code, exitUnavailable)
	}
}

// TestFailDoesNotPassOverASourceItCouldNotOpen. Rung 2 judges only tags it can
// read, so a governed source this machine cannot resolve has all its citations
// skipped — and a -fail that exited 0 on that would certify a corpus half of
// which it never looked at. The probe module resolves the SDK through a
// `replace` and requires nothing else, so go-jose is genuinely unreachable
// rather than stubbed.
func TestFailDoesNotPassOverASourceItCouldNotOpen(t *testing.T) {
	root, _ := probeModule(t, "- **probe** — *Evidence: checked against go-jose v4.0.5 — jwk.go JSONWebKey.*\n")

	var out, errOut strings.Builder
	code := run([]string{"-root", root, "-file", "probe.md", "-comments=false", "-fail"}, &out, &errOut)
	if code != exitUnavailable {
		t.Errorf("-fail exited %d over a citation of a source it could not open, want %d: "+
			"rung 2 skipped it, and exit 0 would report that skip as a pass\nstdout:\n%s\nstderr:\n%s",
			code, exitUnavailable, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "go-jose") {
		t.Errorf("stderr does not name the source that went unchecked:\n%s", errOut.String())
	}
	// The SDK is replaced here but not cited, so it does not decide the exit —
	// and it still changes what the run means, which -fail has no report to say.
	if !strings.Contains(errOut.String(), "caveat: anthropic-sdk-go") {
		t.Errorf("stderr does not carry the run's caveats:\n%s", errOut.String())
	}
}

// TestFailDoesNotCertifyAReplacedSource. The probe module reaches the SDK
// through a `replace`, so rung 2 judges whatever tree that names while quoting
// the tag. The citation resolves there and would pass — which is exactly what a
// -fail must not report as the pinned SDK passing.
func TestFailDoesNotCertifyAReplacedSource(t *testing.T) {
	root, pin := probeModule(t, "")
	body := "- **probe** — *Evidence: checked against anthropic-sdk-go " + pin +
		" — betasession.go BetaSessionService.*\n"
	if err := os.WriteFile(filepath.Join(root, "probe.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	code := run([]string{"-root", root, "-file", "probe.md", "-comments=false", "-fail"}, &out, &errOut)
	if code != exitUnavailable {
		t.Errorf("-fail exited %d over a citation of a replaced source, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitUnavailable, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "is replaced by") {
		t.Errorf("stderr does not say the source was replaced:\n%s", errOut.String())
	}
}

// probeModule writes a module that reaches the pinned SDK through a `replace`
// and requires nothing else, with probe.md holding body, and tracks both in git.
// It returns the root and the pin. The go directive is this repository's and the
// requirement is the pin: a newer directive than the toolchain would send go
// looking for another toolchain, and neither may be a literal a bump outdates.
func probeModule(t *testing.T, body string) (string, string) {
	t.Helper()
	sdk, err := Module(repoRoot(t), SDKModule)
	if err != nil {
		t.Fatalf("resolving the pin offline: %v", err)
	}
	root := t.TempDir()
	mod := "module probe\n\ngo " + goDirective(t, repoRoot(t)) + "\n\nrequire " + SDKModule + " " +
		sdk.Version + "\n\nreplace " + SDKModule + " => " + sdk.Dir + "\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTree(t, root)
	return root, sdk.Version
}

// goDirective reads the go directive from a module's go.mod.
func goDirective(t *testing.T, root string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if v, ok := strings.CutPrefix(line, "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("go.mod has no go directive")
	return ""
}

// TestTheReportNamesTheRepositorysOwnCoordinates, end to end: a coordinate
// into a file this repository tracks is checked by no rung, and -report has to
// say so by name rather than leave it out.
func TestTheReportNamesTheRepositorysOwnCoordinates(t *testing.T) {
	root := repoRoot(t)
	probe := filepath.Join(t.TempDir(), "probe.md")
	const body = "- **probe** — *Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent; " +
		"internal/api/server.go:12.*\n"
	if err := os.WriteFile(probe, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	run([]string{"-root", root, "-file", probe, "-comments=false", "-report"}, &out, io.Discard)
	report := out.String()
	at := strings.Index(report, "not checked, and why")
	if at < 0 || !strings.Contains(report[at:], "internal/api/server.go:12") {
		t.Errorf("the report does not name our own coordinate under what went unchecked:\n%s", report)
	}
}
