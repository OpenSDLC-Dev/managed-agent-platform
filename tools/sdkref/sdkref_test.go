package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sym(names ...string) []string { return names }

func sameLocator(a, b Locator) bool {
	if a.Kind != b.Kind || a.File != b.File || a.Path != b.Path ||
		a.From != b.From || a.To != b.To || a.Reason != b.Reason ||
		len(a.Symbols) != len(b.Symbols) {
		return false
	}
	for i := range a.Symbols {
		if a.Symbols[i] != b.Symbols[i] {
			return false
		}
	}
	return true
}

// TestParseCitation pins the grammar docs/plan/51_sdk-reference-binding.md
// settles on: a temporal form, a source, a tag, and a locator inside that source
// at that tag. Each case is one thing the grammar must accept or must not, and
// the "must not" half matters more — a parser that shrugs at a malformed
// citation hands rung 1 an empty finding list, which reads as a clean corpus.
func TestParseCitation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want *Citation // nil means "not a citation in this grammar"
	}{
		{
			name: "a symbol anchor, the default form",
			in:   "checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaManagedAgentsWebFetchToolConfig",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.66.0",
				Loc: Locator{Kind: "symbol", File: "betaagent.go", Symbols: sym("BetaManagedAgentsWebFetchToolConfig")}},
		},
		{
			name: "two symbols in one clause, the plan's own live example",
			in: "checked against anthropic-sdk-go v1.70.1 — betaagent.go " +
				"BetaManagedAgentsWebFetchToolConfig and BetaManagedAgentsWebSearchToolConfig",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "symbol", File: "betaagent.go", Symbols: sym(
					"BetaManagedAgentsWebFetchToolConfig", "BetaManagedAgentsWebSearchToolConfig")}},
		},
		{
			name: "a method qualified by its receiver",
			in:   "since anthropic-sdk-go v1.63.0 — betaagent.go BetaAgentNewParams.MarshalJSON",
			want: &Citation{Form: "since", Source: "anthropic-sdk-go", Tag: "v1.63.0",
				Loc: Locator{Kind: "symbol", File: "betaagent.go", Symbols: sym("BetaAgentNewParams.MarshalJSON")}},
		},
		{
			name: "an api.md anchor, which plan 51 requires and which is not Go",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md BetaEnvironmentWorkService.Stop",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "symbol", File: "api.md", Symbols: sym("BetaEnvironmentWorkService.Stop")}},
		},
		{
			name: "a capitalised form, as a citation opening a sentence writes it",
			in:   "Since anthropic-sdk-go v1.63.0 — betaagent.go BetaAgentNewParams",
			want: &Citation{Form: "since", Source: "anthropic-sdk-go", Tag: "v1.63.0",
				Loc: Locator{Kind: "symbol", File: "betaagent.go", Symbols: sym("BetaAgentNewParams")}},
		},
		{
			name: "a spec's path followed by a symbol",
			in:   "checked against anthropic-sdk-go v1.70.1 — scripts/mock-spec.json.gz BetaSessionNewParams",
			want: nil,
		},
		{
			name: "an absent-at claim, the negative polarity",
			in:   "absent at anthropic-sdk-go v1.70.1 — skills.go resolveSkillVersion",
			want: &Citation{Form: "absent at", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "symbol", File: "skills.go", Symbols: sym("resolveSkillVersion")}},
		},
		{
			name: "a stamped span with its earned reason",
			in:   "checked against anthropic-sdk-go v1.66.0 — betasessionevent.go:2931-2980 (span: crosses-declarations)",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.66.0",
				Loc: Locator{Kind: "span", File: "betasessionevent.go", From: 2931, To: 2980, Reason: "crosses-declarations"}},
		},
		{
			name: "a schema path into the bundled spec",
			in:   "checked against anthropic-sdk-go v1.70.1 — spec components.schemas.BetaSession",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "schema", File: "spec", Path: "components.schemas.BetaSession"}},
		},
		{
			name: "go-jose, the other module in the build graph",
			in:   "since go-jose v4.1.4 — jwks.go JSONWebKeySet.Key",
			want: &Citation{Form: "since", Source: "go-jose", Tag: "v4.1.4",
				Loc: Locator{Kind: "symbol", File: "jwks.go", Symbols: sym("JSONWebKeySet.Key")}},
		},
		{
			name: "anthropic-cli, in the grammar but not resolvable",
			in:   "checked against anthropic-cli v0.5.0 — main.go run",
			want: &Citation{Form: "checked against", Source: "anthropic-cli", Tag: "v0.5.0",
				Loc: Locator{Kind: "symbol", File: "main.go", Symbols: sym("run")}},
		},
		{
			name: "the corpus form this plan replaces is not a citation yet",
			in:   "anthropic-sdk-go v1.66.0 lib/environments/poller.go:492-518",
			want: nil,
		},
		{
			name: "prose that names a source but cites nothing",
			in:   "the anthropic-sdk-go checkout tracks the API's tip",
			want: nil,
		},
		{
			name: "prose where a symbol should be is not a symbol",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go any prose at all",
			want: nil,
		},
		{
			name: "non-go on the bundled spec by the path the corpus actually writes",
			in:   "checked against anthropic-sdk-go v1.70.1 — scripts/mock-spec.json.gz:120-140 (span: non-go)",
			want: nil,
		},
		{
			name: "a reversed range is no range",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:5-1 (span: crosses-declarations)",
			want: nil,
		},
		{
			name: "a range that overflows an int is no range",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:99999999999999999999-1 (span: no-unique-name)",
			want: nil,
		},
		{
			name: "a range starting before line one is no range",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:0-4 (span: crosses-declarations)",
			want: nil,
		},
		{
			// What falsifies a span is what its range encloses, and a range
			// that holds nothing reads the same whether it never did or no
			// longer does.
			name: "an absent-at claim takes no span",
			in:   "absent at anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: crosses-declarations)",
			want: nil,
		},
		{
			name: "non-go over Go source is refused by the extension alone",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: non-go)",
			want: nil,
		},
		{
			name: "and a declaration reason over a file no Go parser reads",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:1-2 (span: no-unique-name)",
			want: nil,
		},
		{
			name: "non-go over prose, the one kind of file it can be true of",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:1-2 (span: non-go)",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "span", File: "api.md", From: 1, To: 2, Reason: "non-go"}},
		},
		{
			name: "and over plain text",
			in:   "checked against anthropic-sdk-go v1.70.1 — NOTES.txt:1-2 (span: non-go)",
			want: &Citation{Form: "checked against", Source: "anthropic-sdk-go", Tag: "v1.70.1",
				Loc: Locator{Kind: "span", File: "NOTES.txt", From: 1, To: 2, Reason: "non-go"}},
		},
		{
			// Plan 51 keeps `non-go` narrow: a structured file is as readable as
			// the spec, and a span over one would keep its numbers forever.
			name: "non-go over a structured file is refused",
			in:   "checked against anthropic-sdk-go v1.70.1 — release-please-config.json:1-2 (span: non-go)",
			want: nil,
		},
		{
			// Rung 2 compares a stamp with the pin as text, so this would never
			// be the pin and never be judged.
			name: "a tag with a leading zero is not a release",
			in:   "checked against anthropic-sdk-go v1.070.1 — betaagent.go BetaAgentNewParams",
			want: nil,
		},
		{
			name: "nor in its major or patch number",
			in:   "checked against anthropic-sdk-go v01.70.01 — betaagent.go BetaAgentNewParams",
			want: nil,
		},
		{
			name: "a zero is a number, not a leading zero",
			in:   "checked against go-jose v0.0.0 — jwk.go JSONWebKey",
			want: &Citation{Form: "checked against", Source: "go-jose", Tag: "v0.0.0",
				Loc: Locator{Kind: "symbol", File: "jwk.go", Symbols: sym("JSONWebKey")}},
		},
		{
			name: "a schema path belongs to the source that bundles the spec",
			in:   "checked against go-jose v4.1.4 — spec components.schemas.JSONWebKey",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCitation(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ParseCitation(%q) = %+v, want nil", tc.in, got)
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("ParseCitation(%q) = nil, want %+v", tc.in, tc.want)
			}
			if got.Form != tc.want.Form || got.Source != tc.want.Source ||
				got.Tag != tc.want.Tag || !sameLocator(got.Loc, tc.want.Loc) {
				t.Errorf("ParseCitation(%q) =\n  %+v\nwant\n  %+v", tc.in, *got, *tc.want)
			}
		})
	}
}

// testRequires is a go.mod's requirements as the shape tests see them: two
// projects this grammar does not govern, a governed source, and the MCP go-sdk.
var testRequires = Required([]string{"k8s.io/api", "cloud.google.com/go/storage",
	"github.com/go-jose/go-jose/v4", "github.com/modelcontextprotocol/go-sdk"})

// TestShapeFindings pins rung 1, which is always decidable. A candidate is
// anything that reaches for this grammar — a source name, a tag, a coordinate —
// so that a citation cannot escape the rung by being malformed enough to look
// like prose.
func TestShapeFindings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		rules []string // the Rule of each finding, in order
	}{
		{
			name:  "a well-formed symbol anchor is clean",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams.*",
			rules: nil,
		},
		{
			name: "two symbols joined by and are one clean citation",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go " +
				"BetaManagedAgentsWebFetchToolConfig and BetaManagedAgentsWebSearchToolConfig.*",
			rules: nil,
		},
		{
			name:  "a tag with no temporal form",
			in:    "*Evidence: anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams.*",
			rules: []string{"undated"},
		},
		{
			name:  "a bare line number is not a locator",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — betaagent.go:484.*",
			rules: []string{"bare-line"},
		},
		{
			name:  "a span with no reason has not earned the fallback",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — betaagent.go:484-570.*",
			rules: []string{"span-unreasoned"},
		},
		{
			name:  "a reason outside the closed set",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — betaagent.go:484-570 (span: generated).*",
			rules: []string{"span-reason-unknown"},
		},
		{
			name:  "a span over the bundled spec is refused, because it is a tree",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — spec:120-140 (span: non-go).*",
			rules: []string{"span-on-spec"},
		},
		{
			name: "by the path the corpus really writes, not only the token",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — " +
				"scripts/mock-spec.json.gz:120-140 (span: non-go).*",
			rules: []string{"span-on-spec"},
		},
		{
			// Refusing only `non-go` closed one door and left the next one open:
			// `crosses-declarations` is just as irrefutable over a document no Go
			// parser reads, and would let the spec citations keep their spans.
			name: "and whatever reason it gives, not only non-go",
			in: "*Evidence: checked against anthropic-sdk-go v1.66.0 — " +
				"scripts/mock-spec.json.gz:22765-22770 (span: crosses-declarations).*",
			rules: []string{"span-on-spec"},
		},
		{
			// The reference's own document is a tree for the same reason the
			// bundled copy is, and the registry cites it by name — so a span
			// over it must not slip past as conforming while the identical span
			// over `spec` is refused.
			name: "and the reference's OpenAPI document, which is a tree too",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — " +
				"anthropic-openapi.yml:22765-22770 (span: non-go).*",
			rules: []string{"span-on-spec"},
		},
		{
			// Comments and registry lines alike date a claim against a release
			// without saying whose. There is no source token for a head to match,
			// and most carry no coordinate either.
			name:  "a temporal form and a tag, with no source between them",
			in:    "// …the reference toolset's answer since v1.63.0 (tools/agenttoolset/fs.go).",
			rules: []string{"source-unnamed"},
		},
		{
			// Only a cut at the next citation's head can leave a connective
			// dangling, so that is the only place the word may be dropped.
			// Stripping it everywhere truncated a legal anchor into a locator
			// with no symbol at all — and reported the citation as unreadable
			// when it was the reader that was wrong.
			name:  "a symbol whose name is a connective survives the trim",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go and.*",
			rules: nil,
		},
		{
			name:  "but a form followed by a source and a tag is not sourceless",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: nil,
		},
		{
			// A citation that opens a sentence is capitalised. The registry writes
			// one that way, and a case-sensitive pattern was silent about it.
			name:  "a capitalised temporal form with no source is still sourceless",
			in:    "*Evidence: Since v1.62.0 the SDK's client loop retries it.*",
			rules: []string{"source-unnamed"},
		},
		{
			name:  "and a capitalised conforming citation is still conforming",
			in:    "*Evidence: Checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: nil,
		},
		{
			// A word that merely ends in a form dates nothing, so the version is
			// as bare as if the word were not there.
			name:  "a form inside a longer word is not a form",
			in:    "the shape is unchecked against v1.63.0",
			rules: []string{"bare-tag"},
		},
		{
			// The plan's own opening example. No named rule recognised it, which
			// is why a version is a candidate whatever surrounds it.
			name:  "a version with neither a form nor a source",
			in:    "// the object is ours, not the SDK's: at the pinned v1.66.0 it carries text",
			rules: []string{"bare-tag"},
		},
		{
			name:  "a version after a file, in a parenthesis",
			in:    "// the reference's rule (agenttoolset/skillarchive.go, v1.63.0).",
			rules: []string{"bare-tag"},
		},
		{
			name:  "a version written without its patch number",
			in:    "*the unbuilt v1.62 surface (#430).*",
			rules: []string{"bare-tag"},
		},
		{
			// One claim about two releases is one edit, so one finding.
			name:  "two versions in one clause report once",
			in:    "*the type-index links, exactly as v1.61.0 and v1.63.1 did.*",
			rules: []string{"bare-tag"},
		},
		{
			name:  "a form, then a word that is not a source, then a version",
			in:    "// value-preserving (since SDK v1.60.0 the marshaler compacts it)",
			rules: []string{"source-unnamed"},
		},
		{
			// The steering-document guard reads both of these as dated, and the
			// two gates must not disagree about one sentence. Neither is a
			// locator away from conforming, though: the head is written wrong,
			// and asking for a locator first would only send the author back.
			name:  "a form, then a backticked source and tag",
			in:    "*Evidence: checked against `anthropic-sdk-go v1.63.0` in the SDK.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "a form, then a source's possessive and a tag",
			in:    "*which it has read since anthropic-sdk-go's v1.63.0 client.*",
			rules: []string{"head-malformed"},
		},
		{
			// go-jose's module path, because the SDK's contains the grammar name
			// and a head would match inside it before the path was ever read.
			name:  "a governed source named by its module path is still that source",
			in:    "// github.com/go-jose/go-jose/v4 v4.1.4 declares it",
			rules: []string{"undated"},
		},
		{
			name:  "another project's module path attributes the version to it",
			in:    "*a scheduling setting, not a pod-spec field (k8s.io/api v0.36.2 PodSpec).*",
			rules: nil,
		},
		{
			// A module path starts with a domain. Without that, every file path
			// written before a version would attribute it away.
			name:  "a file path in front of a version is not a module path",
			in:    "// read from tools/agenttoolset/fs.go v1.63.0 onwards",
			rules: []string{"bare-tag"},
		},
		{
			name:  "and so does a module path joined to its version by @",
			in:    "// fixed upstream in cloud.google.com/go/storage@v1.56.0",
			rules: nil,
		},
		{
			// #729 holds the decision to leave the MCP go-sdk alone.
			name:  "a project this grammar leaves alone, by name",
			in:    "// `\"tools\": [null]` is legal JSON that go-sdk v1.7.0 panics on",
			rules: nil,
		},
		{
			name:  "the spec named with no tag at all",
			in:    "*Evidence: the OpenAPI spec's BetaManagedAgentsEnvironmentNotFoundError.*",
			rules: []string{"untagged"},
		},
		{
			name:  "a source and the bundled spec's own path with no tag",
			in:    "// (anthropic-sdk-go scripts/mock-spec.json.gz BetaSessionNewParams)",
			rules: []string{"untagged"},
		},
		{
			name:  "the spec token followed by a bare name takes a schema path",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — spec BetaSession.*",
			rules: []string{"symbol-on-spec"},
		},
		{
			name:  "nor does it make a dated citation out of an undated one",
			in:    "*Evidence: unchecked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: []string{"undated"},
		},
		{
			// The commonest way this corpus cites the SDK in passing, and before
			// this rule no rung could see one: no tag, and no line number either.
			name:  "a source and a file with no tag between them",
			in:    "// \"1 to 1000\" (anthropic-sdk-go betafile.go BetaFileListParams)",
			rules: []string{"untagged"},
		},
		{
			// Admitted as a symbol, this resolved against a Go type that merely
			// shares the schema's name, and passed.
			name: "a spec named by its path takes a schema path, not a symbol",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — " +
				"scripts/mock-spec.json.gz BetaSessionNewParams.*",
			rules: []string{"symbol-on-spec"},
		},
		{
			name:  "a stamped claim with no locator at all is dated, not undated",
			in:    "*…which is all its typed schema has read since anthropic-sdk-go v1.68.0 moved it.*",
			rules: []string{"locator-missing"},
		},
		{
			name:  "a host and a port is not a coordinate into any governed source",
			in:    "*Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent; `nexus.infra:8080` is the in-cluster spelling.*",
			rules: []string{"undated"},
		},
		{
			name:  "nor is a quoted path out of a harness reference",
			in:    "*Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent; `src/utils/file.ts:390-437` shows the shape.*",
			rules: []string{"undated"},
		},
		{
			name:  "the reference's OpenAPI document reaches for this grammar too",
			in:    "*Evidence: anthropic-openapi.yml:22765 (\"a small per-schedule jitter\").*",
			rules: []string{"bare-line"},
		},
		{
			name:  "the unmigrated corpus form reports once, not twice",
			in:    "*Evidence: anthropic-sdk-go v1.66.0 lib/environments/poller.go:492-518 (the workaround).*",
			rules: []string{"undated"},
		},
		{
			name:  "a bare continuation inherits no stamp and is its own finding",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — poller.go Poller.Wait, worker_test.go:198-200.*",
			rules: []string{"bare-line"},
		},
		{
			// The registry writes most of its coordinates this way, each
			// hanging off a filename several clauses back. Today the undated
			// clause enclosing them consumes them whole; the moment slice 2
			// rewrites that head they are orphaned, so the rung has to see them
			// on their own.
			name:  "a continuation that inherits its filename too",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaenvironment.go BetaEnvironment, :859-860.*",
			rules: []string{"bare-line"},
		},
		{
			name: "a continuation inside the locator makes the whole clause unreadable",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaenvironment.go BetaEnvironment " +
				"and :859-860.*",
			rules: []string{"locator-unreadable"},
		},
		{
			name:  "a coordinate into a file that is not Go rots the same way",
			in:    "*Evidence: checked against anthropic-sdk-go v1.66.0 — api.md BetaEnvironmentWorkService.Stop, api.md:884.*",
			rules: []string{"bare-line"},
		},
		{
			name:  "the spec named by epithet is still a source at a tag",
			in:    "*Evidence: the v1.70.1 OpenAPI spec's `BetaManagedAgentsCreateSessionParams`.*",
			rules: []string{"undated"},
		},
		{
			name:  "prose about a source, with no coordinate and no tag, is not a candidate",
			in:    "The SDK and CLI checkouts track the API's tip and can run ahead of the pin.",
			rules: nil,
		},
		{
			name:  "an in-repo coordinate is out of scope and reports nothing",
			in:    "*Evidence: internal/api/workapi.go toWire.*",
			rules: nil,
		},
		{
			name:  "prose where a symbol should be is reported, not consumed",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go any prose at all.*",
			rules: []string{"locator-unreadable"},
		},
		{
			name:  "and a package inside a module go.mod requires",
			in:    "// k8s.io/api/core/v1 v0.36.2 has no such field",
			rules: nil,
		},
		{
			// Shaped like a module path, required by nobody: a documentation
			// host carried a governed tag away without a finding.
			name:  "a name that only looks like a module path attributes nothing",
			in:    "// see platform.claude.com/sdk v1.70.1 for the field",
			rules: []string{"bare-tag"},
		},
		{
			// A path ending in a source's name names that source.
			name:  "nor does a pkg.go.dev link into the SDK itself",
			in:    "// pkg.go.dev/github.com/anthropics/anthropic-sdk-go@v1.70.1#BetaSession",
			rules: []string{"undated"},
		},
		{
			name:  "anthropic-cli's module path names anthropic-cli, which go.mod does not require",
			in:    "// github.com/anthropics/anthropic-cli v1.2.0 prints it",
			rules: []string{"undated"},
		},
		{
			// The clause before it ends at the abbreviation's full stop, and
			// the form in front of the version is inside that clause. Asking
			// whether the whole quote was spoken for dropped the version.
			name: "a version dated inside the clause an abbreviation ended",
			in: "*Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent checked against e.g. " +
				"v1.60.0 there.*",
			rules: []string{"undated", "source-unnamed"},
		},
		{
			name:  "a dated head with a capital V has its locator already",
			in:    "*Evidence: since anthropic-sdk-go V1.70.1 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one with no patch number",
			in:    "*Evidence: since anthropic-sdk-go v1.70 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one with a pre-release suffix",
			in:    "*Evidence: since anthropic-sdk-go v1.70.1-beta.1 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one quoting its source and tag",
			in:    "*Evidence: since `anthropic-sdk-go v1.70.1` — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one with a hyphen for the em dash",
			in:    "*Evidence: since anthropic-sdk-go v1.70.1 - betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one naming a schema path after a hyphen",
			in:    "*Evidence: since anthropic-sdk-go v1.70.1 - spec components.schemas.BetaSession.*",
			rules: []string{"head-malformed"},
		},
		{
			// The registry and the comments write an em dash as an aside far
			// more often than as this grammar's separator.
			name:  "but an em dash in prose is not a locator",
			in:    "// read since anthropic-sdk-go v1.68.0 moved it — and the docs agree",
			rules: []string{"locator-missing"},
		},
		{
			// With no locator to find, the tag alone says the head is wrong —
			// and the edit it needs is the whole grammar, not only a locator.
			name:  "a misspelt tag with no locator is still a misspelt head",
			in:    "// read since anthropic-sdk-go v1.68 moved it",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and so is a pre-release suffix with no locator",
			in:    "// read since anthropic-sdk-go v1.68.0-beta.1 moved it",
			rules: []string{"head-malformed"},
		},
		{
			// The spec's own file is caught by name as well, so this needs a
			// compressed JSON file the SDK does not ship as its spec.
			name:  "a source and another json.gz path with no tag",
			in:    "// (anthropic-sdk-go fixtures/wire.json.gz BetaSessionNewParams)",
			rules: []string{"untagged"},
		},
		{
			name:  "an absent-at span",
			in:    "*Evidence: absent at anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: crosses-declarations).*",
			rules: []string{"span-absent"},
		},
		{
			name:  "non-go over a Go file",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: non-go).*",
			rules: []string{"span-reason-impossible"},
		},
		{
			name:  "a declaration reason over a file no Go parser reads",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — api.md:1-2 (span: crosses-declarations).*",
			rules: []string{"span-reason-impossible"},
		},
		{
			name:  "a coordinate into a compressed JSON file",
			in:    "*Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent; fixtures/wire.json.gz:12 shows it.*",
			rules: []string{"undated", "bare-line"},
		},
		{
			name:  "the bundled spec's file, with no source and no tag",
			in:    "// the bundled scripts/mock-spec.json.gz bounds it at 256",
			rules: []string{"untagged"},
		},
		{
			name:  "a dated head whose tag has a leading zero",
			in:    "*Evidence: since anthropic-sdk-go v01.70.1 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one with no locator",
			in:    "// read since anthropic-sdk-go v1.070.1 moved it",
			rules: []string{"head-malformed"},
		},
		{
			// The SDK's module path ends in the grammar's name for it. Read from
			// that name, the head had no form in front of it and was told to add
			// one it already had.
			name:  "a dated claim naming the SDK by its module path",
			in:    "*Evidence: since github.com/anthropics/anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "a dated claim naming the spec by what it is",
			in:    "*Evidence: checked against the v1.70.1 OpenAPI spec's BetaSession.*",
			rules: []string{"source-unnamed"},
		},
		{
			name:  "and one naming its source only as a possessive",
			in:    "*Evidence: since anthropic-sdk-go's v1.70.1 OpenAPI spec.*",
			rules: []string{"head-malformed"},
		},
		{
			// Every other pattern reads two numbers at least, so this was silent.
			name:  "a dated head whose tag has only its major number",
			in:    "*Evidence: since anthropic-sdk-go v1 — betaagent.go BetaAgent.*",
			rules: []string{"head-malformed"},
		},
		{
			name:  "and one ending a sentence",
			in:    "// its shape has held since go-jose v4. The rest is ours",
			rules: []string{"head-malformed"},
		},
		{
			name:  "a major version named with no form dates nothing",
			in:    "// go-jose v4 parses the key set",
			rules: nil,
		},
		{
			// The colon is missing, so there is no span for the reason to be
			// about, and the file it names is not the whole head.
			name:  "a span reason after a range with no colon",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go 3-6 (span: non-go).*",
			rules: []string{"locator-unreadable"},
		},
		{
			name: "and one over the bundled spec is still a span over the spec",
			in: "*Evidence: checked against anthropic-sdk-go v1.70.1 — " +
				"scripts/mock-spec.json.gz 120-140 (span: non-go).*",
			rules: []string{"span-on-spec"},
		},
		{
			// No reason makes this legal, so telling its author to add one would
			// only send them back.
			name:  "an absent-at span with no reason",
			in:    "*Evidence: absent at anthropic-sdk-go v1.70.1 — betaagent.go:3-6.*",
			rules: []string{"span-absent"},
		},
		{
			name:  "a schema path for a source that bundles no spec",
			in:    "*Evidence: checked against go-jose v4.1.4 — spec components.schemas.JSONWebKey.*",
			rules: []string{"spec-not-bundled"},
		},
		{
			name:  "non-go over a structured file",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — release-please-config.json:1-2 (span: non-go).*",
			rules: []string{"span-reason-impossible"},
		},
		{
			name:  "non-go over prose is conforming",
			in:    "*Evidence: checked against anthropic-sdk-go v1.70.1 — api.md:1-2 (span: non-go).*",
			rules: nil,
		},
		{
			name:  "a negated form is not a citation, however well the rest parses",
			in:    "*Evidence: NOT CHECKED AGAINST anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: []string{"negated-form"},
		},
		{
			name:  "nor with an auxiliary between",
			in:    "// this has not been checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent",
			rules: []string{"negated-form"},
		},
		{
			name:  "nor with no locator",
			in:    "// never checked against anthropic-sdk-go v1.70.1, only read",
			rules: []string{"negated-form"},
		},
		{
			name:  "nor with no source",
			in:    "// it wasn’t checked against v1.70.1 at all",
			rules: []string{"negated-form"},
		},
		{
			name:  "nor naming the spec by what it is",
			in:    "*Evidence: no longer since the v1.70.1 OpenAPI spec.*",
			rules: []string{"negated-form"},
		},
		{
			name:  "nor with only a major number",
			in:    "*Evidence: not since go-jose v4 — jwk.go JSONWebKey.*",
			rules: []string{"negated-form"},
		},
		{
			name:  "a negation further back in the sentence is prose",
			in:    "*Evidence: not the point; checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: nil,
		},
		{
			name:  "a word ending in a negation negates nothing",
			in:    "*Evidence: the knot since anthropic-sdk-go v1.70.1 — betaagent.go BetaAgent.*",
			rules: nil,
		},
		{
			// The steering-document guard reads a possessive in any case.
			name:  "a capitalised possessive before a tag",
			in:    "// read since SDK'S v1.63.0 client",
			rules: []string{"source-unnamed"},
		},
		{
			name:  "a project left alone, named as a capitalised possessive",
			in:    "// go-sdk'S v1.7.0 panics on it",
			rules: nil,
		},
		{
			name:  "another major version of a module go.mod requires is not that module",
			in:    "// github.com/modelcontextprotocol/go-sdk/v2 v2.0.0 panics on it",
			rules: []string{"bare-tag"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, f := range (Scanner{Requires: testRequires}).Line(tc.in) {
				got = append(got, f.Rule)
			}
			if strings.Join(got, ",") != strings.Join(tc.rules, ",") {
				t.Errorf("Shape(%q) rules = %v, want %v", tc.in, got, tc.rules)
			}
		})
	}
}

// TestAnUntaggedSpecIsToldToTakeASchemaPath. A source written in front of the
// spec's own file is untagged like any file, but the edit it needs is a schema
// path: advice asking for a symbol would send the migrator to the one form the
// grammar refuses for a spec.
func TestAnUntaggedSpecIsToldToTakeASchemaPath(t *testing.T) {
	for in, want := range map[string]string{
		"// (anthropic-sdk-go scripts/mock-spec.json.gz BetaSessionNewParams)": "spec <schema path>",
		"// (anthropic-sdk-go betafile.go BetaFileListParams)":                 "<file> <symbol>",
	} {
		if got := Shape(in); len(got) != 1 || !strings.Contains(got[0].Msg, want) {
			t.Errorf("Shape(%q) = %v, want one finding advising %q", in, got, want)
		}
	}
}

// TestAnImpossibleReasonSaysWhyByKind. A span reason the file's kind
// contradicts is one rule, but the edit depends on the kind: a Go file has a
// symbol to name, a structured file has no span at all, and prose has no
// declarations for a declaration reason to be about.
func TestAnImpossibleReasonSaysWhyByKind(t *testing.T) {
	const head = "checked against anthropic-sdk-go v1.70.1 — "
	for in, want := range map[string]string{
		head + "betaagent.go:3-6 (span: non-go)":               "it is Go",
		head + "release-please-config.json:1-2 (span: non-go)": "`non-go` is for prose alone",
		head + "api.md:1-2 (span: crosses-declarations)":       "no Go parser reads",
	} {
		if got := Shape(in); len(got) != 1 || got[0].Rule != "span-reason-impossible" ||
			!strings.Contains(got[0].Msg, want) {
			t.Errorf("Shape(%q) = %v, want span-reason-impossible saying %q", in, got, want)
		}
	}
}

// TestAClauseStopsAtTheNextCitation is the case that made the two halves of one
// scan disagree: written side by side, the first citation swallowed the second,
// so rung 1 saw one malformed clause while Citations saw one well-formed one and
// each was silent about the other's.
func TestAClauseStopsAtTheNextCitation(t *testing.T) {
	const src = "checked against anthropic-sdk-go v1.70.1 — betaagent.go Foo " +
		"and checked against anthropic-sdk-go v1.63.0 — betafile.go Bar"
	shape := ShapeAll(src)
	cites := Citations(src)
	if len(cites) != 2 {
		t.Errorf("Citations = %d, want both: %+v", len(cites), cites)
	}
	if len(shape) != 0 {
		t.Errorf("ShapeAll = %v, want none: both clauses conform", shape)
	}
	if len(cites) == 2 && (cites[0].Loc.Symbols[0] != "Foo" || cites[1].Loc.Symbols[0] != "Bar") {
		t.Errorf("Citations resolved to %q and %q", cites[0].Loc.Desc(), cites[1].Loc.Desc())
	}
}

// TestAParentheticalDoesNotCutTheClause pins the parenthesis tracking in
// clause(). Without it the comma inside the aside ends the locator and rung 1
// reports half a symbol as the whole citation — a message that sends the
// migrator to the wrong text.
func TestAParentheticalDoesNotCutTheClause(t *testing.T) {
	const src = "checked against anthropic-sdk-go v1.70.1 — betadeployment.go BetaDeploymentService (New, Update)"
	got := ShapeAll(src)
	if len(got) != 1 {
		t.Fatalf("ShapeAll = %v, want the one unreadable locator", got)
	}
	if !strings.Contains(got[0].Msg, "(New, Update)") {
		t.Errorf("the finding stops short of the parenthetical, so it names a locator the "+
			"document does not contain:\n  %s", got[0].Msg)
	}
}

// TestAnInRepoCoordinateIsOutOfScope pins the one piece of state the scanner
// has. The registry cites the SDK's `internal/` and ours in the same clause, so
// only the tree can tell them apart; a scanner that lost the predicate would
// report our own files as unstamped citations into someone else's source.
func TestAnInRepoCoordinateIsOutOfScope(t *testing.T) {
	const src = "*Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go Foo; " +
		"internal/apierror/apierror.go:29 and internal/api/server.go:699.*"
	ours := func(p string) bool { return p == "internal/api/server.go" }
	var got []string
	for _, f := range (Scanner{InRepo: ours}).Line(src) {
		got = append(got, f.Msg)
	}
	if len(got) != 1 {
		t.Fatalf("Scanner.Line = %v, want only the SDK's coordinate", got)
	}
	if !strings.Contains(got[0], "internal/apierror/apierror.go:29") {
		t.Errorf("the reported coordinate is %q, want the SDK's", got[0])
	}
	if none := (Scanner{}).Line(src); len(none) != 2 {
		t.Errorf("with no predicate both coordinates are external and both report; got %v", none)
	}
}

// TestTheRegistryAndCommentsAreReadable is the rung that runs the real files
// inside make verify, the way tools/registrycheck's own test does.
//
// Counting findings would be a floor, not an assertion: a detector that had
// regressed to one rule would still clear it. So the real check is differential
// — two probe lines are appended to the corpus, one that must be reported and
// one that must not, and the delta has to be exactly the first. That survives
// the migration slices 2 and 3 perform, because it does not depend on how much
// of the corpus is still unmigrated.
func TestTheRegistryAndCommentsAreReadable(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		t.Fatalf("read %s: %v", File, err)
	}
	files, err := Tracked(root)
	if err != nil {
		t.Fatalf("indexing %s: %v", root, err)
	}
	paths, err := Requires(root)
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	inRepo, requires := InSet(files), Required(paths)
	scanner := Scanner{InRepo: inRepo, Requires: requires}
	findings := scanner.All(string(src))
	t.Logf("%s: %d shape finding(s) over the unmigrated corpus", File, len(findings))

	// One probe per kind of candidate, each on its own line, so a detector that
	// had regressed to recognising any one shape — or only the probe itself —
	// cannot clear it. The lines after them must add nothing: a migrated
	// citation, and versions the text attributes to projects this grammar does
	// not govern.
	probes := []struct{ rule, line string }{
		{"undated", "- **p1** — *Evidence: anthropic-sdk-go v1.70.1 betasession.go:544-550 (sentinel).*"},
		{"undated", "- **p2** — *Evidence: the v1.70.1 OpenAPI spec's `BetaSentinelParams`.*"},
		{"source-unnamed", "- **p3** — the reference's answer since v1.63.0 (sentinel)."},
		{"untagged", "- **p4** — (anthropic-sdk-go betasentinel.go BetaSentinel)."},
		{"locator-missing", "- **p5** — which its schema has read since anthropic-sdk-go v1.68.0 moved it."},
		{"bare-line", "- **p6** — *Evidence: checked against anthropic-sdk-go v1.70.1 — betasession.go BetaSession, :859-860.*"},
		{"bare-tag", "- **p7** — at the pinned v1.66.0 the sentinel is absent."},
		{"untagged", "- **p8** — *Evidence: the OpenAPI spec's `BetaSentinelParams`.*"},
		{"head-malformed", "- **p9** — checked against `anthropic-sdk-go v1.63.0` (sentinel)."},
		{"head-malformed", "- **p10** — *Evidence: since anthropic-sdk-go V1.70.1 — betasentinel.go BetaSentinel.*"},
		{"undated", "- **p11** — pkg.go.dev/github.com/anthropics/anthropic-sdk-go@v1.70.1#BetaSentinel."},
	}
	const clean = "\n- **probe** — *Evidence: checked against anthropic-sdk-go v1.70.1 — betasession.go BetaSession.*" +
		"\n- **probe** — go-sdk v1.7.0 panics on it, and k8s.io/api v0.36.2 has no such field."
	appended := string(src)
	for _, p := range probes {
		appended += "\n" + p.line
	}
	probed := scanner.All(appended + clean)
	if len(probed) != len(findings)+len(probes) {
		t.Fatalf("appending %d unmigrated probes and two clean lines moved the finding "+
			"count from %d to %d, want exactly %d more: the detector misses one kind of "+
			"candidate or reports a line it has no business with",
			len(probes), len(findings), len(probed), len(probes))
	}
	for i, p := range probes {
		got := probed[len(findings)+i]
		if got.Rule != p.rule || got.Line != strings.Count(string(src), "\n")+2+i {
			t.Errorf("probe %d reported as %s, want rule %s on its own line", i+1, got, p.rule)
		}
	}

	// The comment half reads the files git tracks, so the list has to be all of
	// them. Asked of git a second way, by pathspec, so the check does not share
	// the filtering it checks.
	cmd := exec.Command("git", "ls-files", "--", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var goFiles int
	for _, f := range files {
		if strings.HasSuffix(f, ".go") {
			goFiles++
		}
	}
	if want := strings.Count(string(out), "\n"); goFiles != want {
		t.Errorf("Tracked lists %d Go file(s), git lists %d: the comment half would leave "+
			"the difference unread", goFiles, want)
	}
	comments, cites, _ := GoComments(root, files, inRepo, requires)
	t.Logf("Go comments: %d shape finding(s), %d citation(s) already in the grammar",
		len(comments), len(cites))

	// The same differential over the comments, driven through GoComments rather
	// than the scanner, so a read that skipped files or mislaid their findings
	// fails here. The probe file lives outside the tree and is reached by a path
	// relative to it, which is all GoComments asks of a path.
	const body = `package probe

// p1 the shape the reference settled on is betasession.go:912.
//
// p2 the reference toolset's answer since v1.63.0.
//
// p3 the SDK's own check (anthropic-sdk-go betafile.go BetaFileListParams).
//
// p4 at the pinned v1.66.0 the field is absent.
//
// p5 the SDK's bundled spec constrains it.
//
// clean: checked against anthropic-sdk-go v1.70.1 — betasession.go BetaSession,
// go-sdk v1.7.0 panics on it, and listening on x.com:443 and nexus.infra:8080.
func P() {}
`
	path := filepath.Join(t.TempDir(), "probe.go")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	rel = filepath.ToSlash(rel)
	withProbe, _, _ := GoComments(root, append(files[:len(files):len(files)], rel), inRepo, requires)
	want := []struct {
		rule string
		line int
	}{{"bare-line", 3}, {"source-unnamed", 5}, {"untagged", 7}, {"bare-tag", 9}, {"untagged", 11}}
	if len(withProbe) != len(comments)+len(want) {
		t.Fatalf("a probe file moved the comment findings from %d to %d, want exactly %d "+
			"more:\n%v", len(comments), len(withProbe), len(want), withProbe[min(len(comments), len(withProbe)):])
	}
	for i, w := range want {
		got := withProbe[len(comments)+i]
		if got.File != rel || got.Rule != w.rule || got.Line != w.line {
			t.Errorf("probe finding %d = %s, want %s at %s:%d", i+1, got, w.rule, rel, w.line)
		}
	}
	var samples []Finding
	samples = append(samples, findings[:min(3, len(findings))]...)
	samples = append(samples, comments[:min(3, len(comments))]...)
	for _, f := range samples {
		t.Logf("  sample: %s", f)
	}
}

// TestRungTwoRunsOverTheRealCorpus is what slice 4 turns into the gate. It runs
// here already, reporting rather than failing, because a rung that only ever saw
// fixtures would be flipped to failing on the day it first met the corpus.
func TestRungTwoRunsOverTheRealCorpus(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		t.Fatalf("read %s: %v", File, err)
	}
	files, err := Tracked(root)
	if err != nil {
		t.Fatalf("indexing %s: %v", root, err)
	}
	cites := Citations(string(src))
	_, comments, _ := GoComments(root, files, InSet(files), nil)
	env, err := NewEnv(root)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	all := append(cites, comments...)
	got := env.Resolution(all)
	t.Logf("rung 2 over %d real citation(s) at pin %s: %d finding(s)", len(all), env.Pin, len(got))

	// The corpus is not migrated yet, so on its own it may hand rung 2 nothing to
	// judge. Two probes at whatever go.mod pins today, naming what that tag's own
	// text declares — never a version or a symbol written here, which the next
	// bump would turn into a probe rung 2 skips or one that fails for its own
	// premise — make the run assert something: one resolves and must add
	// nothing, one does not and must add exactly one finding.
	sdk := realSDK(t)
	probes := Citations(
		"checked against anthropic-sdk-go " + env.Pin + " — " + sdk.file + " " + sdk.method + "\n" +
			"checked against anthropic-sdk-go " + env.Pin + " — " + sdk.file + " " + sdk.absent)
	if len(probes) != 2 {
		t.Fatalf("the probes did not parse: %+v", probes)
	}
	probed := env.Resolution(append(all, probes...))
	if len(probed) != len(got)+1 || probed[len(probed)-1].Rule != "vanished-at-stamp" {
		t.Errorf("adding one resolving and one vanished probe at the pin moved rung 2 from %d "+
			"to %d finding(s), want exactly one more, vanished-at-stamp: %v",
			len(got), len(probed), probed)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestAContinuationHungOffACoordinateWithASlash. The registry writes one of its
// continuations against the coordinate before it with a slash and nothing else
// between them — no space, no parenthesis. A scanner that required either read
// the pair as a single coordinate and said nothing about the second half.
func TestAContinuationHungOffACoordinateWithASlash(t *testing.T) {
	const line = "*Evidence: anthropic-sdk-go v1.70.1 betaagent.go BetaAgent; " +
		"betaagent.go:4301-4302/:4536-4537 (no agent-side ceiling).*"
	var quoted []string
	for _, f := range Shape(line) {
		if f.Rule == "bare-line" {
			quoted = append(quoted, f.Msg)
		}
	}
	if len(quoted) != 2 {
		t.Fatalf("Shape reported %d bare coordinates, want both halves of the pair: %v",
			len(quoted), quoted)
	}
	if !strings.Contains(quoted[1], `":4536-4537"`) {
		t.Errorf("the continuation after the slash was not reported on its own: %s", quoted[1])
	}
}
