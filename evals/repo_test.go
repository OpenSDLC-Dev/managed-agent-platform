package evals

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
)

// The e-repo-answer tier (docs/plan/25_git-repo-mounting.md, "Unit E"): the
// github_repository chain against a real GitHub repository. Consent is the
// suite's own RUN_EVALS, because this task costs what every other task costs —
// a model turn — and adds only a clone. Configuration rides the repo-root .env
// beside the model endpoint, under the same contract the other live tiers keep:
// the file supplies configuration, the environment supplies consent, and once
// opted in a missing name FAILS rather than skips.
//
// Neither name begins with GITHUB_, and that is not a style choice: GitHub
// refuses to store a secret or a variable whose name starts with that prefix, so
// the spelling these constants had until #358 could never have been configured
// in CI under the name the code reads — it would have needed a second name and a
// mapping in the workflow's env: block, a permanent indirection whose one job is
// to be typed correctly forever. Renaming was free while nothing anywhere was
// configured; it stopped being free the moment the evals environment took both.
const (
	// RepoURLEnv is the fixture repository's canonical clone URL.
	RepoURLEnv = "EVAL_GITHUB_REPO_URL"
	// RepoTokenEnv is a fine-grained token scoped to that one repository,
	// Contents: Read-only. Nothing here ever needs write.
	RepoTokenEnv = "EVAL_GITHUB_REPO_TOKEN"
)

// passphraseFile is the fixture repository's one required file. The trial asks
// for what it holds without naming it or the mount, so answering means the agent
// followed the injected "Mounted repositories" block to a real checkout.
const passphraseFile = "PASSPHRASE.txt"

// repoEvalMount is where the fixture repository is mounted. Stated rather than
// defaulted so a grader can assert the agent reached exactly it.
const repoEvalMount = "/workspace/fixture"

// cloneOracleTimeout bounds the oracle's own clone (see clonePassphrase). Two
// minutes is far more than a few-kilobyte fixture needs and far less than the
// suite's 120m budget, which is the number that matters: this exists so a stall
// fails the trial rather than wedging the run.
const cloneOracleTimeout = 2 * time.Minute

// RepoFixture attaches a github_repository resource to the session at create.
// The url and token are not fields: they are one deployment's credentials, and
// putting them in a task literal would be the one way this repository could
// come to hold a token.
type RepoFixture struct {
	MountPath string
}

// repoConfig resolves the fixture repository, failing (never skipping) when the
// suite is opted in and the configuration is absent — the rot-detection rule the
// other live tiers keep. It is called from inside the trial rather than from
// tasks(), so a missing name fails exactly this task instead of the whole suite.
func repoConfig(t *testing.T) (url, token string) {
	t.Helper()
	url, token, err := repoConfigErr()
	if err != nil {
		t.Fatal(err)
	}
	return url, token
}

// repoConfigErr is the same rule as an error, for the callers that must not
// Goexit — see repoPassphrase.
func repoConfigErr() (url, token string, err error) {
	url, token = repoResolve(RepoURLEnv), repoResolve(RepoTokenEnv)
	for _, c := range []struct{ name, v string }{{RepoURLEnv, url}, {RepoTokenEnv, token}} {
		if c.v == "" {
			return "", "", fmt.Errorf("%s opted into the eval suite but %s is unset: set it in "+
				"the environment or the repo-root .env (a fine-grained, single-repository, "+
				"Contents: Read-only token; the repository must contain %s)",
				modeltest.EvalsEnv, c.name, passphraseFile)
		}
	}
	return url, token, nil
}

// repoAnswer is the github_repository chain end to end (plan E2E-2): a
// passphrase lives only in a file inside a real GitHub repository, so a correct
// answer proves create → seal → claim → clone → tar → extract → the brain's
// block → the model → a read inside the checkout, as one chain over the real
// network.
//
// The turn names neither the mount nor the file's path. The injected "Mounted
// repositories" block is the only thing that says where the checkout is, so
// injection is the discovery mechanism under test — exactly the reasoning
// skillAnswer and fileAnswer are built on.
func repoAnswer() Task {
	return Task{
		ID:   "repo-answer",
		Repo: &RepoFixture{MountPath: repoEvalMount},
		Turns: []Turn{{Message: "A git repository has been mounted into your sandbox. " +
			"It contains a file named " + passphraseFile + " holding a secret passphrase. " +
			"What is the passphrase? Reply with exactly the passphrase and nothing else."}},
		Graders: []Grader{
			// Either, for the reason its two twins are: the passphrase is
			// reachable only through the materialized checkout, so a right answer
			// is unambiguous platform evidence, while a missing one may be the
			// model declining to look — and the read grader's transcript evidence
			// is the arbiter.
			ReadsFile(repoEvalMount+"/"+passphraseFile, Either),
			RepoPassphraseAnswered(Either),
			// Platform, unconditionally: no model behavior can put the token in
			// the transcript. Only we can.
			TranscriptCarriesNoRepoToken(),
		},
	}
}

// RepoPassphraseAnswered asserts the final message carries the fixture
// repository's passphrase.
//
// The expected value is read from the remote itself rather than configured,
// because a third .env name would be a second copy of the fixture's state and
// the copies would drift silently — the trial would then be graded against what
// the repository used to hold. The oracle shares go-git with the code under
// test, so a go-git content bug would cancel out here; that is what unit M's
// rows are for, which read content back from a fixture repository this process
// wrote itself. What this row proves is the platform chain around the clone.
func RepoPassphraseAnswered(class Class) Grader {
	return Grader{
		Name:  "repo-passphrase-answered",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			want := repoPassphrase(t)
			if got := finalMessage(tr); !strings.Contains(got, want) {
				return fmt.Errorf("final agent.message = %q, want it to carry the passphrase "+
					"held in %s", got, passphraseFile)
			}
			return nil
		},
	}
}

// TranscriptCarriesNoRepoToken is the live twin of unit M's token sweep and unit
// W's: the whole transcript, as the API hands it back, is searched for the
// token's literal bytes. The sandbox-side sweep proves the token never reached
// the container; this one proves it never reached the record of what happened.
func TranscriptCarriesNoRepoToken() Grader {
	return Grader{
		Name:  "transcript-carries-no-repo-token",
		Class: Platform,
		Check: func(t *testing.T, tr *Trial) error {
			_, token := repoConfig(t)
			raw, err := json.Marshal(tr.Events)
			if err != nil {
				return fmt.Errorf("marshal the transcript for the sweep: %w", err)
			}
			if strings.Contains(string(raw), token) {
				// The token is not echoed into the failure: a failing eval's
				// message is the thing most likely to be pasted somewhere.
				return fmt.Errorf("the transcript carries the fixture repository's " +
					"authorization token")
			}
			return nil
		},
	}
}

// repoSecrets is every fixture value the artifacts must never publish: the
// authorization token, and the passphrase itself. secretsOf supplies the model
// endpoint's half of the same set.
//
// The token is the obvious one. TranscriptCarriesNoRepoToken asserts it never
// reaches the transcript — but the run where that grader fires is precisely the
// run whose artifacts would carry it, since a failed trial's whole transcript is
// dumped to evals/artifacts/ and evals.yml uploads that directory
// unconditionally from a public repository, whose workflow artifacts anyone can
// download.
//
// The passphrase is the one that matters more, and it is not hypothetical: on a
// trial that fails for any *other* reason — a grader the model tripped, a flaky
// turn — the passphrase is sitting in the agent's own final message, and that
// transcript is what gets uploaded. Every other answer-style trial plants a
// fresh {{RECALL}} per trial, so publishing one costs nothing; this fixture is
// fixed, so a single publication burns it permanently and every later run of
// this trial silently proves nothing.
//
// Both are resolved here, before the first trial, rather than when the grader
// first wants the passphrase: grading is not the only path to an artifact write
// — a trial that aborts in its drive records through runAndGrade's defer without
// a grader ever running — and the cost is one shallow clone of a few kilobytes,
// shared with the grader through the same sync.Once.
func repoSecrets() []string {
	var out []string
	if token := repoResolve(RepoTokenEnv); token != "" {
		out = append(out, token)
	}
	// Best effort: an unconfigured or unreachable fixture is not this function's
	// failure to report. repoConfig fails the trial that needs it, naming the
	// name that was missing, and the grader raises this same cached error.
	if pass, err := repoPassphraseValue(); err == nil {
		out = append(out, pass)
	}
	return out
}

// repoPassphraseValue clones the fixture repository in-process and returns the
// passphrase, once per run: every caller wants the same bytes, and re-cloning
// per grader would spend network for nothing.
var repoPassphraseValue = func() func() (string, error) {
	var (
		once sync.Once
		val  string
		err  error
	)
	return func() (string, error) {
		once.Do(func() { val, err = clonePassphrase() })
		return val, err
	}
}()

// repoPassphrase is repoPassphraseValue for a grader, raising the failure.
//
// It is raised here rather than inside the once because t.Fatal is
// runtime.Goexit, and Once marks itself done on the way out regardless — so the
// next caller would sail past with an empty passphrase, which strings.Contains
// treats as present in everything. A grader that passes because its expectation
// is missing is worse than no grader.
func repoPassphrase(t *testing.T) string {
	t.Helper()
	val, err := repoPassphraseValue()
	if err != nil {
		t.Fatalf("read %s from the fixture repository: %v", passphraseFile, err)
	}
	return val
}

// repoTokenUsername is the basic-auth username GitHub wants beside a token.
const repoTokenUsername = "x-access-token"

func clonePassphrase() (string, error) {
	url, token, err := repoConfigErr()
	if err != nil {
		return "", err
	}
	fs := memfs.New()
	// Bounded, because the alternative is not a slow run but a lost one: a
	// blackholed route leaves this clone hanging until `go test -timeout` panics
	// the process from testing's alarm goroutine, and that is the one exit that
	// never runs runAndGrade's deferred recordTrial — so the wedged run would
	// spend its whole budget and still write no evidence for the trial that
	// wedged. Generous for a fixture of a few kilobytes; short against 120m.
	ctx, cancel := context.WithTimeout(context.Background(), cloneOracleTimeout)
	defer cancel()
	// Depth 1: the oracle wants the tip's content, not the history. The
	// materialization under test is deliberately full — see decision 1.
	if _, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:   url,
		Auth:  &githttp.BasicAuth{Username: repoTokenUsername, Password: token},
		Depth: 1,
	}); err != nil {
		// go-git copies up to a kilobyte of a refusing host's response body into
		// the error it returns, and a host that names the credential it rejected
		// has already decoded it — the exposure internal/executor scrubs at its
		// own clone boundary (scrubTokenErr), measured there against a fixture
		// that echoes the header back. This error ends up in a t.Fatalf, so in
		// CI it lands in a public step log; GitHub masks a secret's literal
		// bytes and not its base64 basic-auth form, so both go. Nothing here
		// classifies the error, so a plain string loses nothing.
		return "", errors.New(scrubRepoToken(err.Error(), token))
	}
	f, err := fs.Open(passphraseFile)
	if err != nil {
		return "", fmt.Errorf("the fixture repository must contain %s: %w", passphraseFile, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	got := strings.TrimSpace(string(b))
	if got == "" {
		return "", fmt.Errorf("%s is empty; it must hold the passphrase the trial asks for", passphraseFile)
	}
	return got, nil
}

// scrubRepoToken removes the fixture token from a message in both forms it can
// appear in — raw, and the base64 blob go-git puts after "Basic ". The pair is
// what internal/executor/repoclone.go removes at the production clone's own
// boundary; this is the same rule for the oracle's, kept as a separate copy
// because that one is unexported and a test package must not reach for it.
func scrubRepoToken(msg, token string) string {
	if token == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, token, "[redacted]")
	blob := base64.StdEncoding.EncodeToString([]byte(repoTokenUsername + ":" + token))
	return strings.ReplaceAll(msg, blob, "[redacted]")
}

// repoResolve reads one name from the environment, falling back to the repo-root
// .env for exactly the two repository names. The environment always wins,
// including when it sets one to the empty string — that is an answer, not an
// invitation for the file to supply one. Consent (RUN_EVALS) is outside this
// set, so it can never come from disk.
func repoResolve(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	if !repoEnvName(key) {
		return ""
	}
	return repoDotEnv()[key]
}

func repoEnvName(key string) bool { return key == RepoURLEnv || key == RepoTokenEnv }

// repoDotEnv parses the repo-root .env once. The values stay here rather than
// being pushed into the process environment — an os.Setenv would outlive the
// test that triggered it (modeltest's rationale, mirrored). A missing file is
// not an error: the environment may carry everything.
var repoDotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(evalsRepoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseRepoDotEnv(f)
})

// evalsRepoRoot derives the checkout root from this file's compile-time path, so
// a worktree reads its own .env rather than the main checkout's.
func evalsRepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..")
}

func parseRepoDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(key)
		if !repoEnvName(key) {
			continue
		}
		out[key] = parseRepoValue(value)
	}
	return out
}

// parseRepoValue takes the value side of one .env line, exactly as modeltest and
// webtooltest read the same file: a quoted value is whatever the quotes enclose,
// so a '#' inside them is content; an unquoted one runs to a '#' that follows
// whitespace, which is a trailing comment, and keeps one that does not.
func parseRepoValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if i := repoCommentStart(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func repoCommentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
