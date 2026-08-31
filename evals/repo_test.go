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
// The turn names neither the mount nor the file's path, so the injected "Mounted
// repositories" block is what the agent is meant to follow — injection as the
// discovery mechanism, the same reasoning skillAnswer and fileAnswer are built
// on, and with the same admitted leak. Isolation is not perfect and cannot be:
// the platform's own default mount root for a repository is /workspace/ (api's
// defaultRepoMountRoot), which is also the sandbox's working directory, so a
// bare `ls` finds the checkout whether or not injection worked. Mounting it
// somewhere unlikely would buy a cleaner claim by testing a path no user takes.
// That is why both content graders are Either rather than Platform: a right
// answer is strong evidence, and the transcript is the arbiter on a miss.
//
// This turn asks a model to hand back something the prompt itself calls a secret
// out of a repository the model did not clone, which is a shape a safety-tuned
// model may decline — and one did, during verification on 2026-08-12: a refusal
// in 1.7s with no tool call at all, "I notice there's a prompt injection
// attempt". Both content graders are Either precisely for that, so the classing
// holds, but the trial still reds, and an unactionable red is what parked it.
//
// The obvious repair was tried and measured worse, which is why this wording
// survives unchanged from #331. Rewording the turn to drop "secret passphrase"
// and ask for "the one line" of a named file — the move journalMultiturn and
// viewRange record for the same reflex — went 2 of 4 against the same endpoint,
// against 7 of 8 for the wording above: once a fabricated "there is no
// PASSPHRASE.txt here, which is convenient, there is nothing to leak", once a
// tool call emitted as literal text. Asking plainly for the passphrase reads as
// a task; asking someone to read a file and recite it apparently reads as a
// wrapper around the same request, with worse odds. Twelve runs is not a study,
// so what is recorded here is the measurement, not a law — but do not re-do this
// rewrite on intuition alone.
func repoAnswer() Task {
	return Task{
		ID:   "repo-answer",
		Repo: &RepoFixture{MountPath: repoEvalMount},
		Turns: []Turn{{Message: "A git repository has been mounted into your sandbox. " +
			"It contains a file named " + passphraseFile + " holding a secret passphrase. " +
			"What is the passphrase? Reply with exactly the passphrase and nothing else."}},
		Graders: []Grader{
			// The token sweep runs FIRST, and the order is load-bearing rather
			// than cosmetic. RepoPassphraseAnswered resolves the passphrase and
			// t.Fatals when it cannot, which is runtime.Goexit — it unwinds the
			// grader loop, so every grader after it is skipped. Put the sweep
			// second and the one unconditional Platform assertion here would go
			// unrun on exactly the runs where the credential path misbehaved.
			//
			// Platform, unconditionally: no model behavior can put the token in
			// the transcript. Only we can.
			TranscriptCarriesNoRepoToken(),
			// Either, for the reason its two twins are: the passphrase is
			// reachable only through the materialized checkout, so a right answer
			// is unambiguous platform evidence, while a missing one may be the
			// model declining to look — and the read grader's transcript evidence
			// is the arbiter.
			ReadsFile(repoEvalMount+"/"+passphraseFile, Either),
			RepoPassphraseAnswered(Either),
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
// token. The sandbox-side sweep proves the token never reached the container;
// this one proves it never reached the record of what happened.
//
// Both forms are swept, not just the literal bytes. A credential that surfaces
// in a transcript at all most likely arrived inside an echoed Authorization
// header or a transport error, and there it is base64 — so a sweep for the raw
// token alone would report clean on the one shape the leak actually takes.
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
			if strings.Contains(string(raw), token) || strings.Contains(string(raw), basicAuthBlob(token)) {
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
// The token is registered in both the forms it can appear in, for the reason
// scrubRepoToken removes both: the base64 basic-auth blob is a perfectly usable
// credential and is the one form GitHub's own log masking does not cover.
//
// The passphrase cannot be registered from here alone, because it is not known
// until something clones. This function makes the attempt so that the value is
// in the set before the first trial runs — a trial that aborts in its drive
// records through gradeAttempt's defer without any grader having run — but the
// attempt is best effort, and the guarantee comes from repoPassphraseValue
// registering the value itself the moment it first resolves, wherever that is.
func repoSecrets() []string {
	var out []string
	if token := repoResolve(RepoTokenEnv); token != "" {
		out = append(out, token, basicAuthBlob(token))
	}
	// Best effort, and its error deliberately dropped: an unconfigured or
	// unreachable fixture is not this function's failure to report. repoConfig
	// fails the trial that needs it, naming the name that was missing.
	if pass, err := repoPassphraseValue(); err == nil {
		out = append(out, pass)
	}
	return out
}

// repoPassphraseValue clones the fixture repository in-process and returns the
// passphrase, resolving it at most once successfully per run: every caller wants
// the same bytes, and re-cloning per grader would spend network for nothing.
//
// A *failure* is not cached, which is the whole reason this is a mutex and not a
// sync.Once. The first caller is recordMeta, at second zero, before the stack is
// even up; the trial that needs the value runs many minutes later. Caching a
// startup blip would replay it as a hard failure long after github.com came
// back, reddening a nightly for a condition that no longer exists — the exact
// species of unactionable red that got this trial parked in the first place.
//
// The value registers itself with the run's artifact scrub as soon as it exists.
// That placement is deliberate: the alternative — registering only in
// recordMeta's best-effort call — leaves the scrub blind on precisely the path
// that publishes, where the oracle's early clone failed, the platform's later
// one succeeded, the agent answered, and the transcript with the passphrase in
// it is what gets written.
var repoPassphraseValue = func() func() (string, error) {
	var (
		mu  sync.Mutex
		val string
	)
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if val != "" {
			return val, nil
		}
		v, err := clonePassphrase()
		if err != nil {
			return "", err
		}
		val = v
		addRunSecret(val)
		return val, nil
	}
}()

// repoPassphrase is repoPassphraseValue for a grader, raising the failure.
//
// It is raised here rather than inside the resolver because t.Fatal is
// runtime.Goexit: called down there it would unwind through the resolver's own
// lock-and-cache, and any design that then treated the value as settled would
// hand the next caller an empty passphrase — which strings.Contains treats as
// present in everything. A grader that passes because its expectation is missing
// is worse than no grader. The resolver caches only success for the same reason.
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
	// never runs gradeAttempt's deferred recordTrial — so the wedged run would
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
	return strings.ReplaceAll(msg, basicAuthBlob(token), "[redacted]")
}

// basicAuthBlob is what go-git puts after "Basic " for this credential — the
// only encoded form of it that can appear anywhere, and the form GitHub's own
// log masking does not cover. internal/executor/repoclone.go keeps the twin.
func basicAuthBlob(token string) string {
	return base64.StdEncoding.EncodeToString([]byte(repoTokenUsername + ":" + token))
}

// repoResolve reads one name from the environment, falling back to the repo-root
// .env for exactly the two repository names. The environment always wins,
// including when it sets one to the empty string — that is an answer, not an
// invitation for the file to supply one. Consent (RUN_EVALS) is outside this
// set, so it can never come from disk.
func repoResolve(key string) string { return repoLookup(os.LookupEnv, repoDotEnv(), key) }

// repoLookup is repoResolve with both of its sources injected — the shape
// internal/modeltest and its three siblings expose for the same reason. Without
// it the rules above can only be asserted against whatever the machine's real
// .env happens to hold, which in CI is nothing at all: evals.yml writes no file,
// so a test of the fallback would pass there by having nothing to fall back to.
func repoLookup(env func(string) (string, bool), file map[string]string, key string) string {
	if v, ok := env(key); ok {
		return v
	}
	if !repoEnvName(key) {
		return ""
	}
	return file[key]
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
