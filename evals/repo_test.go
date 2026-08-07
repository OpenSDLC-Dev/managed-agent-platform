package evals

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

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
const (
	// RepoURLEnv is the fixture repository's canonical clone URL.
	RepoURLEnv = "GITHUB_EVAL_REPO_URL"
	// RepoTokenEnv is a fine-grained token scoped to that one repository,
	// Contents: Read-only. Nothing here ever needs write.
	RepoTokenEnv = "GITHUB_EVAL_REPO_TOKEN"
)

// passphraseFile is the fixture repository's one required file. The trial asks
// for what it holds without naming it or the mount, so answering means the agent
// followed the injected "Mounted repositories" block to a real checkout.
const passphraseFile = "PASSPHRASE.txt"

// repoEvalMount is where the fixture repository is mounted. Stated rather than
// defaulted so a grader can assert the agent reached exactly it.
const repoEvalMount = "/workspace/fixture"

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

// repoPassphrase clones the fixture repository in-process and returns the
// passphrase, once per run: every trial that grades against it wants the same
// bytes, and re-cloning per grader would spend network for nothing.
//
// Nothing inside the once may call t.Fatal. That is runtime.Goexit, and Once
// marks itself done on the way out regardless — so the next caller would sail
// past with an empty passphrase, which strings.Contains treats as present in
// everything. A grader that passes because its expectation is missing is worse
// than no grader, so the failure travels back as an error and is raised here.
var repoPassphrase = func() func(*testing.T) string {
	var (
		once sync.Once
		val  string
		err  error
	)
	return func(t *testing.T) string {
		t.Helper()
		once.Do(func() { val, err = clonePassphrase() })
		if err != nil {
			t.Fatalf("read %s from the fixture repository: %v", passphraseFile, err)
		}
		return val
	}
}()

func clonePassphrase() (string, error) {
	url, token, err := repoConfigErr()
	if err != nil {
		return "", err
	}
	fs := memfs.New()
	// Depth 1: the oracle wants the tip's content, not the history. The
	// materialization under test is deliberately full — see decision 1.
	if _, err := git.CloneContext(context.Background(), memory.NewStorage(), fs, &git.CloneOptions{
		URL:   url,
		Auth:  &githttp.BasicAuth{Username: "x-access-token", Password: token},
		Depth: 1,
	}); err != nil {
		return "", err
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
