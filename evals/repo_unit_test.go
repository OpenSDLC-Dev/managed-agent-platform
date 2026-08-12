package evals

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The oracle's clone error is scrubbed of the credential in both forms it can
// take, because go-git copies up to a kilobyte of a refusing host's response
// body into the error it returns and that error reaches a t.Fatalf — a public
// step log in CI.
//
// The base64 case is the one that earns the test. GitHub Actions masks a
// secret's literal bytes in a log and not its basic-auth encoding, so a host
// that echoed the Authorization header back would publish a perfectly usable
// credential through the one form the platform's own masking does not cover.
func TestScrubRepoTokenRemovesBothFormsOfTheCredential(t *testing.T) {
	const token = "github_pat_ExampleValueThatIsNotAToken"
	blob := base64.StdEncoding.EncodeToString([]byte(repoTokenUsername + ":" + token))
	msg := "unauthorized: Basic " + blob + " was rejected for " + token

	got := scrubRepoToken(msg, token)
	if strings.Contains(got, token) {
		t.Errorf("the raw token survived the scrub: %q", got)
	}
	if strings.Contains(got, blob) {
		t.Errorf("the basic-auth encoding survived the scrub: %q", got)
	}
	if !strings.Contains(got, "unauthorized") {
		t.Errorf("the scrub ate the message it was meant to preserve: %q", got)
	}

	// An unconfigured token must leave the message alone rather than redact
	// against the empty string, which matches at every position.
	if got := scrubRepoToken(msg, ""); got != msg {
		t.Errorf("scrubRepoToken with no token rewrote the message: %q", got)
	}
}

// The repo-root .env reader repo_test.go carries its own copy of, because
// modeltest's is unexported. Offline, so it runs on every `make verify`.
//
// Until #358 nothing asserted any of this. While the trial was registered the
// nightly at least executed the parser every run — it read the file, found
// nothing, and failed — and parked, nothing executed it at all, so a drift would
// have surfaced only on restore, on the one run nobody wants surprises in.
func TestParseRepoDotEnvReadsOnlyTheRepositoryKeys(t *testing.T) {
	const file = `# a comment
EVAL_GITHUB_REPO_URL=https://github.com/owner/repo
EVAL_GITHUB_REPO_TOKEN='github_pat_single_quoted'

# EVAL_GITHUB_REPO_URL=commented-out
MODEL_API_KEY=not-this-parser's-business
TAVILY_API_KEY=nor-this-one
OTHER_KEY=ignored
no-equals-sign
`
	got := parseRepoDotEnv(strings.NewReader(file))
	want := map[string]string{
		"EVAL_GITHUB_REPO_URL":   "https://github.com/owner/repo",
		"EVAL_GITHUB_REPO_TOKEN": "github_pat_single_quoted",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	// Named rather than left to the count above: this parser sits in the same
	// process as the one holding the model key, and the reason it filters at all
	// is so it never becomes a second reader of a credential that is not its own.
	for _, foreign := range []string{"MODEL_API_KEY", "TAVILY_API_KEY", "OTHER_KEY"} {
		if v, ok := got[foreign]; ok {
			t.Errorf("%s was read as %q; this parser must read only its own two names", foreign, v)
		}
	}
}

// The value-side rules, on the table modeltest asserts its own parser against
// (internal/modeltest/modeltest_test.go, TestParseValue) — copied rather than
// shared, because the parsers are unexported in different packages and a copy in
// each is what turns a drift in either into a red test. parseRepoValue's comment
// claims it reads a line "exactly as modeltest and webtooltest read the same
// file"; this is that claim, asserted.
//
// Every case is one a hand-written .env produces and an earlier version of the
// modeltest parser got wrong — silently, by handing an endpoint a value with a
// comment welded onto it.
func TestParseRepoValueMatchesModeltestsRules(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", `some-model`, "some-model"},
		{"space-delimited comment", `some-model # pinned`, "some-model"},
		{"tab-delimited comment", "some-model\t# pinned", "some-model"},
		{"hash inside the value", `sk-abc#def`, "sk-abc#def"},
		{"double-quoted", `"https://gateway.example/api"`, "https://gateway.example/api"},
		{"single-quoted", `'sk-secret'`, "sk-secret"},
		{"quoted, then a comment", `"abc" # note`, "abc"},
		{"hash inside quotes", `"model # pinned"`, "model # pinned"},
		{"hash inside quotes, then a comment", `"model # pinned" # note`, "model # pinned"},
		{"unbalanced quote", `"abc`, `"abc`},
		{"empty", ``, ""},
		{"surrounding space", `  spaced  `, "spaced"},
	} {
		if got := parseRepoValue(tc.in); got != tc.want {
			t.Errorf("%s: parseRepoValue(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The environment wins over the file, and an explicitly empty one is an answer
// rather than an invitation for the file to supply a value.
//
// That rule is what evals.yml rests on: an unset repository secret renders as
// the empty string in the step's env: block, and it must read as "unset" — a
// nightly that fell back to a file for a name CI deliberately left blank would
// be testing whatever was last on the runner's disk.
func TestRepoResolvePrefersTheEnvironmentIncludingAnEmptyValue(t *testing.T) {
	t.Setenv(RepoURLEnv, "https://github.com/owner/from-the-environment")
	if got := repoResolve(RepoURLEnv); got != "https://github.com/owner/from-the-environment" {
		t.Errorf("repoResolve(%s) = %q, want the environment's value", RepoURLEnv, got)
	}
	t.Setenv(RepoURLEnv, "")
	if got := repoResolve(RepoURLEnv); got != "" {
		t.Errorf("repoResolve(%s) = %q after the environment set it empty, want \"\" — "+
			"an explicit empty is the answer, not a fall-through to .env", RepoURLEnv, got)
	}
}

// A name this file does not own never reaches the .env fallback, whatever the
// file holds. The consent variable is the case that matters: RUN_EVALS deciding
// whether to spend money must come from the environment and never from disk.
func TestRepoResolveNeverReadsTheFileForAForeignName(t *testing.T) {
	if got := repoResolve("RUN_EVALS_NOT_A_REPO_NAME"); got != "" {
		t.Errorf("repoResolve of a foreign name = %q, want \"\"", got)
	}
}
