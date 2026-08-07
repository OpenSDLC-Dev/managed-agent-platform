package api_test

// Unit W of plan 25 (docs/plan/25_git-repo-mounting.md, "Verification"): the
// wire surface of github_repository session resources, driven through a real
// handler over a real Postgres. Each test names the matrix rows it implements;
// rows marked 🔍 in the plan are the adversarial probes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

// mustUnmarshal decodes raw JSON or fails the test.
func mustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

// errMessage pulls the message out of an Anthropic error envelope.
func errMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

const repoTestURL = "https://github.com/example-org/example-repo"

// repoBody builds a github_repository create-resource object.
func repoBody(token string, extra map[string]any) map[string]any {
	obj := map[string]any{"type": "github_repository", "url": repoTestURL, "authorization_token": token}
	for k, v := range extra {
		obj[k] = v
	}
	return obj
}

// createRepoSession creates a session mounting the given resources.
func createRepoSession(t *testing.T, s *tserver, resources ...any) map[string]any {
	t.Helper()
	agentID, envID := fixture(t, s)
	return createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID, "resources": resources})
}

// wantRepoResourceFields asserts the rendered github_repository wire shape —
// {id, created_at, mount_path, type, updated_at, url, checkout}, checkout
// nullable, and NEVER an authorization_token (betasessionresource.go:211-221).
func wantRepoResourceFields(t *testing.T, res map[string]any) {
	t.Helper()
	wantFields(t, res, "id", "created_at", "mount_path", "type", "updated_at", "url", "checkout")
	if id, _ := res["id"].(string); !strings.HasPrefix(id, "sesrsc_") {
		t.Errorf("resource id = %q, want a sesrsc_ id", res["id"])
	}
	if res["type"] != "github_repository" {
		t.Errorf("resource type = %v, want github_repository", res["type"])
	}
}

// TestSessionRepoCreateRendersWireShape is w-create-minimal, w-create-full,
// w-create-commit, and w-multi: the rendered union shape, the defaulted mount
// path, checkout echoed as given (null when omitted).
func TestSessionRepoCreateRendersWireShape(t *testing.T) {
	s := newTestServer(t)

	// w-create-minimal: defaults resolved and rendered at create.
	sess := createRepoSession(t, s, repoBody("ghp_minimal", nil))
	res := resourcesOf(t, sess)[0]
	wantRepoResourceFields(t, res)
	if res["mount_path"] != "/workspace/example-repo" {
		t.Errorf("default mount_path = %v, want /workspace/example-repo", res["mount_path"])
	}
	if co, present := res["checkout"]; !present || co != nil {
		t.Errorf("omitted checkout renders %v (present=%v), want explicit null", co, present)
	}
	if res["created_at"] != res["updated_at"] {
		t.Errorf("created_at %v != updated_at %v on create", res["created_at"], res["updated_at"])
	}

	// w-create-full: everything as given, .git stripped only for the default.
	sess = createRepoSession(t, s, repoBody("ghp_full", map[string]any{
		"url":        repoTestURL + ".git",
		"mount_path": "/srv/checkout",
		"checkout":   map[string]any{"type": "branch", "name": "feature/x"},
	}))
	res = resourcesOf(t, sess)[0]
	wantRepoResourceFields(t, res)
	if res["url"] != repoTestURL+".git" {
		t.Errorf("url = %v, want the .git form echoed as given", res["url"])
	}
	if res["mount_path"] != "/srv/checkout" {
		t.Errorf("mount_path = %v, want /srv/checkout", res["mount_path"])
	}
	co, _ := res["checkout"].(map[string]any)
	if co["type"] != "branch" || co["name"] != "feature/x" {
		t.Errorf("checkout = %v, want the branch object echoed", res["checkout"])
	}

	// w-create-commit: the sha preserved verbatim.
	sha := strings.Repeat("0123456789abcdef", 2) + "01234567"
	sess = createRepoSession(t, s, repoBody("ghp_commit", map[string]any{
		"checkout": map[string]any{"type": "commit", "sha": sha},
	}))
	res = resourcesOf(t, sess)[0]
	co, _ = res["checkout"].(map[string]any)
	if co["type"] != "commit" || co["sha"] != sha {
		t.Errorf("checkout = %v, want the commit object echoed", res["checkout"])
	}

	// w-create-minimal continued: ".git" strips for the derived name only —
	// the stored url keeps it.
	sess = createRepoSession(t, s, repoBody("ghp_git", map[string]any{
		"url": "https://github.com/example-org/tool.git"}))
	res = resourcesOf(t, sess)[0]
	if res["mount_path"] != "/workspace/tool" {
		t.Errorf("mount_path = %v, want /workspace/tool (.git stripped)", res["mount_path"])
	}
	if res["url"] != "https://github.com/example-org/tool.git" {
		t.Errorf("url = %v, want the .git suffix kept", res["url"])
	}

	// w-multi: two repos + one file, all rendered, paths distinct. A file
	// inside a repo's tree is the supported overlay direction — and since a
	// file's mount_path resolves under the uploads root (#323), the overlaid
	// repo mounts there and the file's already-rooted path passes through.
	fileID := uploadOneFile(t, s, "overlay.txt")
	sess = createRepoSession(t, s,
		repoBody("ghp_a", nil),
		repoBody("ghp_b", map[string]any{"url": "https://github.com/example-org/other", "mount_path": "/mnt/session/uploads/proj"}),
		map[string]any{"type": "file", "file_id": fileID, "mount_path": "/mnt/session/uploads/proj/config.json"},
	)
	all := resourcesOf(t, sess)
	if len(all) != 3 {
		t.Fatalf("rendered %d resources, want 3", len(all))
	}
	types := map[string]int{}
	mounts := map[string]bool{}
	for _, r := range all {
		types[r["type"].(string)]++
		mounts[r["mount_path"].(string)] = true
	}
	if types["github_repository"] != 2 || types["file"] != 1 {
		t.Errorf("resource types = %v, want 2 repos + 1 file", types)
	}
	if !mounts["/mnt/session/uploads/proj/config.json"] {
		t.Errorf("mounts = %v, want the overlay file kept inside the repo tree", mounts)
	}
}

// TestSessionRepoTokenSweep is w-token-sweep 🔍: every surface a token could
// reach — the create body itself, rotation, session GET, resource GET, LIST,
// events, and the server's captured slog output — swept for both token values
// and the authorization_token key. Zero hits; any hit anywhere is a failure.
// (The SSE stream is a live tail from connect time with no history replay, so
// the events list endpoint is the stored-event surface.)
func TestSessionRepoTokenSweep(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	const tok1 = "SWEEP-TOKEN-ONE-8f3b2c"
	const tok2 = "SWEEP-TOKEN-TWO-a91d4e"

	var logBuf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	sweep := func(surface string, raw []byte) {
		t.Helper()
		for _, needle := range []string{tok1, tok2, "authorization_token"} {
			if bytes.Contains(raw, []byte(needle)) {
				t.Errorf("%s leaks %q", surface, needle)
			}
		}
	}
	get := func(path string) []byte {
		t.Helper()
		res := s.doRaw(http.MethodGet, path, nil, map[string]string{"x-api-key": testKey})
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return raw
	}

	// The create 201 response body itself (the mutating response a rendering
	// defect leaks through).
	res := s.doRaw(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{repoBody(tok1, nil)}}, map[string]string{"x-api-key": testKey})
	createRaw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("create: status %d, err %v", res.StatusCode, err)
	}
	sweep("create response", createRaw)

	var sess map[string]any
	mustUnmarshal(t, createRaw, &sess)
	sid := sess["id"].(string)
	rid := resourcesOf(t, sess)[0]["id"].(string)

	// The rotation 200 response body, carrying the new token's resource.
	res = s.doRaw(http.MethodPost, "/v1/sessions/"+sid+"/resources/"+rid,
		map[string]any{"authorization_token": tok2}, map[string]string{"x-api-key": testKey})
	rotRaw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("rotate: status %d, err %v", res.StatusCode, err)
	}
	sweep("rotation response", rotRaw)

	sweep("session GET", get("/v1/sessions/"+sid))
	sweep("resource GET", get("/v1/sessions/"+sid+"/resources/"+rid))
	sweep("resources LIST", get("/v1/sessions/"+sid+"/resources"))
	sweep("events GET", get("/v1/sessions/"+sid+"/events"))
	sweep("captured slog output", logBuf.Bytes())
}

// syncBuf is a mutex-guarded buffer for capturing slog output written from
// the handler goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (sb *syncBuf) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.Write(p)
}

func (sb *syncBuf) Bytes() []byte {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]byte(nil), sb.b.Bytes()...)
}

// TestSessionRepoCreateValidation is w-no-token, w-bad-url, w-bad-checkout,
// w-unclean-mount, w-mount-collision, w-nesting, and w-repo-cap 🔍: the
// create-time rejections, all INFERRED strictness (plan 25 decision 3).
func TestSessionRepoCreateValidation(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	fileID := uploadOneFile(t, s, "anchor.txt")
	sha := strings.Repeat("ab", 20)

	nineRepos := make([]any, 9)
	for i := range nineRepos {
		nineRepos[i] = repoBody("ghp_cap", map[string]any{
			"mount_path": "/workspace/r" + strings.Repeat("x", i+1)})
	}

	for name, resources := range map[string][]any{
		// w-no-token
		"missing token":    {map[string]any{"type": "github_repository", "url": repoTestURL}},
		"empty token":      {repoBody("", nil)},
		"token above 8KiB": {repoBody(strings.Repeat("t", 8193), nil)},
		// w-bad-url
		"http scheme":     {repoBody("g", map[string]any{"url": "http://github.com/o/r"})},
		"non-github host": {repoBody("g", map[string]any{"url": "https://gitlab.com/o/r"})},
		"owner only":      {repoBody("g", map[string]any{"url": "https://github.com/onlyowner"})},
		"extra segments":  {repoBody("g", map[string]any{"url": repoTestURL + "/tree/main"})},
		"garbage url":     {repoBody("g", map[string]any{"url": "not a url"})},
		"userinfo url":    {repoBody("g", map[string]any{"url": "https://TOKEN@github.com/o/r"})},
		"query url":       {repoBody("g", map[string]any{"url": repoTestURL + "?token=x"})},
		"fragment url":    {repoBody("g", map[string]any{"url": repoTestURL + "#frag"})},
		"explicit port":   {repoBody("g", map[string]any{"url": "https://github.com:443/o/r"})},
		"trailing slash":  {repoBody("g", map[string]any{"url": repoTestURL + "/"})},
		// The derived default mount is /workspace/<repo-name>; a repo segment
		// that would make it unclean, unstorable, or oversized must be
		// refused at the URL grammar, not stored (verifier findings, 2026-08-07:
		// acme/.. stored mount_path /workspace/.. — the reserved "/" in
		// disguise — and %00 reached the jsonb bind as a 500).
		"bare query url":      {repoBody("g", map[string]any{"url": repoTestURL + "?"})},
		"bare fragment url":   {repoBody("g", map[string]any{"url": repoTestURL + "#"})},
		"dotdot repo url":     {repoBody("g", map[string]any{"url": "https://github.com/acme/.."})},
		"dot repo url":        {repoBody("g", map[string]any{"url": "https://github.com/acme/."})},
		"nul repo url":        {repoBody("g", map[string]any{"url": "https://github.com/acme/%00repo"})},
		"space repo url":      {repoBody("g", map[string]any{"url": "https://github.com/acme/re%20po"})},
		"oversized repo name": {repoBody("g", map[string]any{"url": "https://github.com/acme/" + strings.Repeat("r", 1200)})},
		// w-bad-checkout
		"tag checkout":      {repoBody("g", map[string]any{"checkout": map[string]any{"type": "tag", "name": "v1"}})},
		"branch no name":    {repoBody("g", map[string]any{"checkout": map[string]any{"type": "branch"}})},
		"short sha":         {repoBody("g", map[string]any{"checkout": map[string]any{"type": "commit", "sha": "abc123"}})},
		"non-hex sha":       {repoBody("g", map[string]any{"checkout": map[string]any{"type": "commit", "sha": strings.Repeat("g", 40)}})},
		"checkout bad keys": {repoBody("g", map[string]any{"checkout": map[string]any{"type": "branch", "name": "m", "sha": sha}})},
		"unknown repo key":  {repoBody("g", map[string]any{"depth": 1})},
		// w-unclean-mount
		"relative mount":       {repoBody("g", map[string]any{"mount_path": "workspace/r"})},
		"dot segment":          {repoBody("g", map[string]any{"mount_path": "/workspace/./r"})},
		"doubled slash":        {repoBody("g", map[string]any{"mount_path": "/workspace//r"})},
		"trailing slash mount": {repoBody("g", map[string]any{"mount_path": "/workspace/r/"})},
		// w-mount-collision (aliases of one directory; the file arm compares
		// by its uploads-resolved path since #323, so a relative file
		// spelling collides with a repo's literal path at the same place)
		"repo/repo collision": {
			repoBody("g", map[string]any{"mount_path": "/workspace/same"}),
			repoBody("g", map[string]any{"url": "https://github.com/example-org/other", "mount_path": "/workspace/same"})},
		"alias collision": {
			map[string]any{"type": "file", "file_id": fileID, "mount_path": "same"},
			repoBody("g", map[string]any{"mount_path": "/mnt/session/uploads/same"})},
		// w-nesting
		"file ancestor of repo": {
			map[string]any{"type": "file", "file_id": fileID, "mount_path": "repo"},
			repoBody("g", map[string]any{"mount_path": "/mnt/session/uploads/repo/src"})},
		"nested repos": {
			repoBody("g", map[string]any{"mount_path": "/workspace/outer"}),
			repoBody("g", map[string]any{"url": "https://github.com/example-org/other", "mount_path": "/workspace/outer/inner"})},
		"root mount":    {repoBody("g", map[string]any{"mount_path": "/"})},
		"staging mount": {repoBody("g", map[string]any{"mount_path": "/tmp"})},
		// w-repo-cap
		"nine repos": nineRepos,
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": envID, "resources": resources})
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}

	// The caps are inclusive at the limit: a token of exactly 8 KiB and
	// exactly eight repos are accepted.
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{repoBody(strings.Repeat("t", 8192), nil)}})
	if status != http.StatusOK {
		t.Errorf("token at 8 KiB: status %d, want 200 (%v)", status, body)
	}
	status, body = s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "resources": nineRepos[:8]})
	if status != http.StatusOK {
		t.Errorf("eight repos: status %d, want 200 (%v)", status, body)
	}
}

// TestSessionRepoRotation is w-rotate, w-rotate-bad 🔍, and w-archived-gate 🔍:
// the happy rotation (rendered resource back, updated_at bumped, ciphertext
// replaced and decryptable) and every rejection around it.
func TestSessionRepoRotation(t *testing.T) {
	s := newTestServer(t)
	fileID := uploadOneFile(t, s, "f.txt")
	sess := createRepoSession(t, s,
		repoBody("ghp_original", nil),
		map[string]any{"type": "file", "file_id": fileID})
	sid := sess["id"].(string)
	var repoRID, fileRID string
	for _, r := range resourcesOf(t, sess) {
		if r["type"] == "github_repository" {
			repoRID = r["id"].(string)
		} else {
			fileRID = r["id"].(string)
		}
	}

	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources/"+repoRID,
		map[string]any{"authorization_token": "ghp_rotated"})
	if status != http.StatusOK {
		t.Fatalf("rotate: status %d (%v)", status, res)
	}
	wantRepoResourceFields(t, res)
	if res["updated_at"] == res["created_at"] {
		t.Errorf("rotation did not bump updated_at (%v)", res["updated_at"])
	}

	// Storage: the ciphertext decrypts to the NEW token under the test cipher.
	var ct []byte
	var keyID string
	err := s.pool.QueryRow(context.Background(),
		`SELECT token_ciphertext, token_key_id FROM session_resource_credentials
		  WHERE resource_id = $1 AND session_id = $2`, repoRID, sid).Scan(&ct, &keyID)
	if err != nil {
		t.Fatalf("credential row: %v", err)
	}
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	plain, err := cipher.Decrypt(context.Background(), ct, keyID)
	if err != nil {
		t.Fatalf("decrypt rotated token: %v", err)
	}
	if string(plain) != "ghp_rotated" {
		t.Errorf("rotated token round-trips to %q, want ghp_rotated", plain)
	}

	// w-rotate-bad: unknown body key / empty token / a file resource's rid
	// (the established message) / unknown rid.
	for name, tc := range map[string]struct {
		rid        string
		body       any
		wantStatus int
	}{
		"unknown body key": {repoRID, map[string]any{"authorization_token": "t", "bogus": 1}, 400},
		"empty token":      {repoRID, map[string]any{"authorization_token": ""}, 400},
		"oversized token":  {repoRID, map[string]any{"authorization_token": strings.Repeat("t", 8193)}, 400},
		"file resource":    {fileRID, map[string]any{"authorization_token": "t"}, 400},
		"unknown rid":      {"sesrsc_0000000000000000000000gk", map[string]any{"authorization_token": "t"}, 404},
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources/"+tc.rid, tc.body)
		wantType := "invalid_request_error"
		if tc.wantStatus == 404 {
			wantType = "not_found_error"
		}
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		wantErr(t, status, body, tc.wantStatus, wantType)
	}

	// w-archived-gate: rotation and delete on an archived session are the
	// established archived-session error.
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}
	status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources/"+repoRID,
		map[string]any{"authorization_token": "t"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodDelete, "/v1/sessions/"+sid+"/resources/"+fileRID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

// TestSessionRepoDeleteRejected is w-delete-rejected 🔍: a repository cannot be
// removed post-create (attached-for-lifetime, INFERRED) while a file in the
// same session keeps its landed delete; the credential row survives the
// rejection and dies with the session (the cascade).
func TestSessionRepoDeleteRejected(t *testing.T) {
	s := newTestServer(t)
	fileID := uploadOneFile(t, s, "f.txt")
	sess := createRepoSession(t, s,
		repoBody("ghp_del", nil),
		map[string]any{"type": "file", "file_id": fileID})
	sid := sess["id"].(string)
	var repoRID, fileRID string
	for _, r := range resourcesOf(t, sess) {
		if r["type"] == "github_repository" {
			repoRID = r["id"].(string)
		} else {
			fileRID = r["id"].(string)
		}
	}

	status, body := s.do(http.MethodDelete, "/v1/sessions/"+sid+"/resources/"+repoRID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	status, body = s.do(http.MethodDelete, "/v1/sessions/"+sid+"/resources/"+fileRID, nil)
	if status != http.StatusOK || body["type"] != "session_resource_deleted" {
		t.Fatalf("file delete: status %d (%v), want the deletion envelope", status, body)
	}

	credRows := func() int {
		var n int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM session_resource_credentials WHERE session_id = $1`, sid).Scan(&n); err != nil {
			t.Fatalf("count credentials: %v", err)
		}
		return n
	}
	if n := credRows(); n != 1 {
		t.Fatalf("credential rows after rejected delete = %d, want 1", n)
	}
	if status, body := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("session delete: status %d (%v)", status, body)
	}
	if n := credRows(); n != 0 {
		t.Errorf("credential rows after session delete = %d, want 0 (ON DELETE CASCADE)", n)
	}
}

// TestSessionRepoAddPostCreateRejected is w-add-post-create 🔍: the add
// endpoint stays file-only (the SDK types Add's body and response as the file
// variant), even for a fully valid repository object — and the file it does
// accept obeys the same ancestor rule create enforces: a post-create add must
// not land a resource above a repository's mount.
func TestSessionRepoAddPostCreateRejected(t *testing.T) {
	s := newTestServer(t)
	sess := createRepoSession(t, s,
		repoBody("ghp_seed", nil),
		repoBody("ghp_up", map[string]any{
			"url": "https://github.com/example-org/other", "mount_path": "/mnt/session/uploads/repo/src"}))
	sid := sess["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources",
		repoBody("ghp_new", map[string]any{"mount_path": "/workspace/added"}))
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(body); !strings.Contains(msg, "only file resources") {
		t.Errorf("add rejection message = %q, want the file-only wording", msg)
	}

	// A file added at /mnt/session/uploads/repo (the resolved form of "repo")
	// would sit above the repo mounted at .../repo/src — the direction create
	// rejects, so add rejects it too. A file *inside* the repo's tree stays
	// the legal overlay.
	fileID := uploadOneFile(t, s, "above.txt")
	status, body = s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": fileID, "mount_path": "repo"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources",
		map[string]any{"type": "file", "file_id": fileID, "mount_path": "repo/src/inside.txt"})
	if status != http.StatusOK {
		t.Errorf("overlay add: status %d, want 200 (%v)", status, body)
	}
}

// TestSessionRepoCreateWithoutCipher is w-no-cipher 🔍: a deployment with no
// secrets cipher refuses a repo-bearing create — nothing is stored, encrypted
// or otherwise. Files keep working on the same server.
func TestSessionRepoCreateWithoutCipher(t *testing.T) {
	s := newTestServerWithCipher(t, nil)
	agentID, envID := fixture(t, s)

	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{repoBody("ghp_x", nil)}})
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	var sessions int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions after refused create = %d, want 0", sessions)
	}

	fileID := uploadOneFile(t, s, "still-works.txt")
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileID}}})
	if sess["id"] == nil {
		t.Fatalf("file-only create failed on the cipher-less server: %v", sess)
	}
}
