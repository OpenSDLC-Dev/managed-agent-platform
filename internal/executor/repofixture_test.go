package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// gitFixture is a real repository served over real smart-HTTP, so the clone
// path under test is the production one end to end: go-git's own client speaks
// to git's own `git http-backend`, the upload-pack GitHub runs, behind
// net/http/cgi. go-git's server-side transport would be the dependency-free
// choice, but it refuses a shallow request outright and cannot answer one
// (#582), and every clone asks for depth 1. `make test` already needs git
// (tools/sdkref asks it which files are tracked); this needs its http-backend
// helper too, which some distributions package apart from git, and without
// it these tests fail rather than skip.
//
// Failure modes are handler-level, which is what lets one fixture drive every
// adversarial row: status answers the auth and not-found reasons, a stall
// answers the deadline, and the repository's own content answers the rest.
type gitFixture struct {
	srv  *httptest.Server
	dir  string
	repo *git.Repository
	// backend serves the repository's gitdir, which sits at the URL's path
	// under the backend's project root, so the two need no mapping.
	backend *cgi.Handler

	// clones counts POSTs to the upload-pack endpoint — one per real clone,
	// which is how m-idempotence proves a second pass fetched nothing.
	clones atomic.Int64
	// requests counts every request, the ref advertisement's included — how a
	// row proves the remote was never asked.
	requests atomic.Int64
	// status, when non-zero, is answered instead of serving the repository.
	status atomic.Int64
	// echoAuth makes the refusal body quote the credentials it was given, both
	// as sent and decoded — what a hostile or over-chatty git host does, and the
	// only way a token can reach a go-git error message (go-git copies up to 1
	// KiB of an error response's body into the error it returns).
	echoAuth atomic.Bool
	// stall delays every request, for the deadline row.
	stall atomic.Int64
	// wantAuth, when set, is the Authorization header the fixture requires;
	// it is also what proves the token travels as a header and nothing else.
	wantAuth atomic.Value
	// sawAuth records the last Authorization header received.
	sawAuth atomic.Value
}

// newGitFixture builds a repository with the given files on its default branch
// and serves it. Every file lands in one commit; extra branches and commits are
// added by the caller through the returned fixture.
func newGitFixture(t *testing.T, files map[string]string) *gitFixture {
	t.Helper()
	// The object store and the working tree sit side by side rather than
	// nested, so handing the storer to git.Init does not collide with the
	// gitdir link it writes into the worktree.
	base := t.TempDir()
	storer := filesystem.NewStorage(osfs.New(filepath.Join(base, "o", "r.git")), cache.NewObjectLRUDefault())
	dir := filepath.Join(base, "tree")
	repo, err := git.Init(storer, osfs.New(dir))
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the fixture serves through git http-backend: %v", err)
	}
	f := &gitFixture{dir: dir, repo: repo, backend: &cgi.Handler{
		// GitHub answers a want for a commit no ref names; git's own
		// upload-pack does only when told to. `-c` reaches the upload-pack the
		// backend runs, on any git version.
		Path: gitBin, Args: []string{"-c", "uploadpack.allowReachableSHA1InWant=true", "http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + base, "GIT_HTTP_EXPORT_ALL=1",
			"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1",
		},
	}}
	f.commit(t, "initial", files)

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.serve)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// url is the fixture repository's clone URL.
func (f *gitFixture) url() string { return f.srv.URL + "/o/r.git" }

// commit writes files into the worktree and commits them, returning the hash.
func (f *gitFixture) commit(t *testing.T, msg string, files map[string]string) plumbing.Hash {
	t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for name, content := range files {
		fh, err := wt.Filesystem.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fh.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		fh.Close()
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return h
}

// branch creates a branch at HEAD, commits files on it, and switches back to
// the default branch — so the branch tip differs from the default's.
func (f *gitFixture) branch(t *testing.T, name string, files map[string]string) {
	t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	head, err := f.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name), Create: true,
	}); err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	f.commit(t, "on "+name, files)
	if err := wt.Checkout(&git.CheckoutOptions{Branch: head.Name()}); err != nil {
		t.Fatalf("switch back to %s: %v", head.Name(), err)
	}
}

// serve is git http-backend behind the handler-level failure modes the
// adversarial rows drive.
func (f *gitFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if auth := r.Header.Get("Authorization"); auth != "" {
		f.sawAuth.Store(auth)
	}
	if d := f.stall.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	if want, _ := f.wantAuth.Load().(string); want != "" && r.Header.Get("Authorization") != want {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	if code := f.status.Load(); code != 0 {
		body := "fixture refuses"
		if f.echoAuth.Load() {
			auth := r.Header.Get("Authorization")
			decoded := auth
			if raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic ")); err == nil {
				decoded = string(raw)
			}
			body = "fixture refuses credentials " + auth + " (" + decoded + ")"
		}
		http.Error(w, body, int(code))
		return
	}

	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
		f.clones.Add(1)
	}
	// The suffix-less spelling a github_repository resource carries names the
	// same repository, as it does on GitHub.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/o/r/"); ok {
		r.URL.Path = "/o/r.git/" + rest
	}
	f.backend.ServeHTTP(w, r)
}

// seedRepoResource stores a github_repository resource on the harness session
// and seals its token, exactly as the API's create transaction would — the two
// halves of the sealed-token contract meeting at one test cipher.
func (h *harness) seedRepoResource(t *testing.T, id, url, mount, token string, checkout map[string]any) {
	t.Helper()
	res := map[string]any{
		"id": id, "type": "github_repository", "url": url, "mount_path": mount,
		"created_at": time.Unix(1700000000, 0).UTC(), "updated_at": time.Unix(1700000000, 0).UTC(),
		"checkout": checkout,
	}
	h.appendResource(t, res)
	ct, keyID, err := h.cipher.Encrypt(context.Background(), []byte(token))
	if err != nil {
		t.Fatalf("seal the token: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO session_resource_credentials (resource_id, session_id, token_ciphertext, token_key_id)
		 VALUES ($1, $2, $3, $4)`, id, h.sid.String(), ct, keyID); err != nil {
		t.Fatalf("store the sealed token: %v", err)
	}
}

// appendResource pushes one rendered resource object onto the session's
// resources array, the way a create would have left it.
func (h *harness) appendResource(t *testing.T, res map[string]any) {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal the resource: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = resources || $1::jsonb WHERE id = $2`,
		string(raw), h.sid.String()); err != nil {
		t.Fatalf("store the resource: %v", err)
	}
}

// repoErrors returns the clone-error payloads recorded on the session, in
// order — the S3 surface for every tolerated failure row.
func (h *harness) repoErrors(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range h.types(t, string(domain.EventSessionError)) {
		var p struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			t.Fatalf("decode a session.error payload: %v", err)
		}
		if p.Error["type"] == repoCloneErrorType {
			out = append(out, p.Error)
		}
	}
	return out
}

// runPass drives one whole work item — provision, materialize (skills, then
// repositories, then files), run the tool — so every repository row observes
// the production sequence rather than a materializer called in isolation.
func (h *harness) runPass(t *testing.T) {
	t.Helper()
	h.suspend(t, writeUse("pass.txt", "ran"))
	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !worked {
		t.Fatal("step claimed no work")
	}
}
