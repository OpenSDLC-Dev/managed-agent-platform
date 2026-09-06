package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The four key lanes of plan 42 §6.1, driven through their middlewares
// directly rather than through the server: nothing on the /v1 wire echoes a
// scope, and adding a route so a test could read one would ship a production
// surface for a test's benefit. The middleware is the unit under test anyway —
// what it puts on the context and what it stamps on the response.
//
// The environment key appears twice because two middlewares share one
// resolver, and the plan's reason for stamping in the resolver is precisely
// that the second one exists (§6.2).

// laneCredential is one credential resolver set up over a database whose rows
// bind the credential to a workspace: the middleware to wrap, a request the
// credential authenticates, and the same route offered a credential no row
// matches.
type laneCredential struct {
	mw  func(http.Handler) http.Handler
	req func() *http.Request
	bad func() *http.Request
}

type scopeLane struct {
	name  string
	build func(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential
}

func scopeLanes() []scopeLane {
	return []scopeLane{
		{"management x-api-key", buildAPIKeyLane},
		{"environment key", buildEnvironmentKeyLane},
		{"environment key on a session route", buildEnvironmentKeyForSessionLane},
		{"work token", buildWorkTokenLane},
		{"gate token", buildGateTokenLane},
	}
}

// buildAPIKeyLane binds a management key to ws — the one lane whose scope is
// its own row's.
func buildAPIKeyLane(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential {
	t.Helper()
	const key = "map-scope-lane-key"
	if err := EnsureAPIKey(context.Background(), pool, "scopelane", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE api_keys SET workspace_id = $2 WHERE key_hash = $1`, hashKey(key), ws); err != nil {
		t.Fatalf("move api key: %v", err)
	}
	return laneCredential{
		mw:  func(next http.Handler) http.Handler { return requireAPIKey(pool, next) },
		req: laneRequest(http.MethodGet, "/v1/agents", "x-api-key", key),
		bad: laneRequest(http.MethodGet, "/v1/agents", "x-api-key", "map-no-such-key"),
	}
}

// buildEnvironmentKeyLane binds a worker key to ws through its ENVIRONMENT's
// row, and sets the key's own reserved columns to a value that must never
// appear in a resolved scope — the fixture half of §6.1's "a key can never
// disagree with the environment it serves".
func buildEnvironmentKeyLane(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential {
	t.Helper()
	env, key := environmentKeyInWorkspace(t, pool, ws)
	return laneCredential{
		mw:  func(next http.Handler) http.Handler { return requireEnvironmentKey(pool, next) },
		req: laneRequest(http.MethodGet, "/v1/environments/"+env+"/work", "Authorization", "Bearer "+key),
		bad: laneRequest(http.MethodGet, "/v1/environments/"+env+"/work", "Authorization", "Bearer sk-map-env01-nope"),
	}
}

// buildEnvironmentKeyForSessionLane is the same credential on the worker half
// of the dual-auth session routes.
func buildEnvironmentKeyForSessionLane(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential {
	t.Helper()
	env, key := environmentKeyInWorkspace(t, pool, ws)
	sess := pgtest.NewSessionInEnv(t, pool, domain.ID(env))
	path := "/v1/sessions/" + sess.String() + "/events"
	return laneCredential{
		mw:  func(next http.Handler) http.Handler { return requireEnvironmentKeyForSession(pool, next) },
		req: laneRequest(http.MethodGet, path, "Authorization", "Bearer "+key),
		bad: laneRequest(http.MethodGet, path, "Authorization", "Bearer sk-map-env01-nope"),
	}
}

// buildWorkTokenLane mints a sessions token for a live item. The token names
// no tenant; the workspace it resolves to is its SESSION's.
func buildWorkTokenLane(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential {
	t.Helper()
	ctx := context.Background()
	sess, env := pgtest.NewSession(t, pool, "self_hosted")
	moveSessionToWorkspace(t, pool, sess, ws)
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, env, sess, queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	item, err := q.Poll(ctx, env, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("poll: %v, %v", item, err)
	}
	token, err := worktoken.Mint(ctx, pool, item.ID.String(), sess.String())
	if err != nil {
		t.Fatalf("worktoken.Mint: %v", err)
	}
	path := "/v1/environments/" + env.String() + "/work/" + item.ID.String() + "/heartbeat"
	return laneCredential{
		mw:  func(next http.Handler) http.Handler { return requireWorkToken(pool, next) },
		req: laneRequest(http.MethodPost, path, "Authorization", "Bearer "+token),
		bad: laneRequest(http.MethodPost, path, "Authorization", "Bearer "+worktoken.TokenPrefix+"nope"),
	}
}

// buildGateTokenLane mints a session's gate token, which likewise inherits.
func buildGateTokenLane(t *testing.T, pool *pgxpool.Pool, ws string) laneCredential {
	t.Helper()
	ctx := context.Background()
	sess, _ := pgtest.NewSession(t, pool, "cloud")
	moveSessionToWorkspace(t, pool, sess, ws)
	token := gatetoken.Mint()
	if err := gatetoken.Ensure(ctx, pool, sess.String(), token); err != nil {
		t.Fatalf("gatetoken.Ensure: %v", err)
	}
	return laneCredential{
		mw:  func(next http.Handler) http.Handler { return requireGateToken(pool, next) },
		req: laneRequest(http.MethodGet, gateconfig.Path, "Authorization", "Bearer "+token),
		bad: laneRequest(http.MethodGet, gateconfig.Path, "Authorization", "Bearer "+gatetoken.Mint()),
	}
}

// TestCredentialLanesResolveAScope runs the one header rule (§6.2) over every
// key lane, message for message. One rule beats five, so one table drives all
// of them and a lane that grew an exception fails here.
func TestCredentialLanesResolveAScope(t *testing.T) {
	for _, lane := range scopeLanes() {
		t.Run(lane.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			ws := newWorkspaceRow(t, pool, "B")
			foreign := newWorkspaceRow(t, pool, "C")
			c := lane.build(t, pool, ws)

			t.Run("resolves the scope its own rows name", func(t *testing.T) {
				res := serveLane(c.mw, scopeEcho(t), c.req())
				wantScopeBody(t, res, domain.Scope{OrgID: "default", WorkspaceID: ws, ProjectID: "default"})
			})

			t.Run("both tenancy headers are stamped on a 200", func(t *testing.T) {
				res := serveLane(c.mw, scopeEcho(t), c.req())
				wantTenancyHeaders(t, res, "default", ws)
			})

			t.Run("both survive an authenticated 4xx", func(t *testing.T) {
				res := serveLane(c.mw, notFoundHandler(), c.req())
				if res.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404 (body %s)", res.Code, res.Body)
				}
				wantTenancyHeaders(t, res, "default", ws)
			})

			t.Run("a pre-auth 401 stamps neither", func(t *testing.T) {
				res := serveLane(c.mw, scopeEcho(t), c.bad())
				if res.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %s)", res.Code, res.Body)
				}
				wantNoTenancyHeaders(t, res)
			})

			t.Run("a header naming its own workspace narrows to it", func(t *testing.T) {
				r := c.req()
				r.Header.Set(workspaceHeader, ws)
				res := serveLane(c.mw, scopeEcho(t), r)
				wantScopeBody(t, res, domain.Scope{OrgID: "default", WorkspaceID: ws, ProjectID: "default"})
			})

			t.Run("a malformed header is refused", func(t *testing.T) {
				r := c.req()
				r.Header.Set(workspaceHeader, "marketing")
				res := serveLane(c.mw, scopeEcho(t), r)
				wantLaneError(t, res, http.StatusBadRequest, errTypeInvalidRequest,
					"anthropic-workspace-id header must be a valid workspace ID.")
				wantNoTenancyHeaders(t, res)
			})

			t.Run("a live workspace it does not cover is not found", func(t *testing.T) {
				r := c.req()
				r.Header.Set(workspaceHeader, foreign)
				res := serveLane(c.mw, scopeEcho(t), r)
				wantLaneError(t, res, http.StatusNotFound, errTypeNotFound,
					"Workspace `"+foreign+"` not found.")
				wantNoTenancyHeaders(t, res)
			})

			t.Run("a workspace that does not exist answers the same bytes", func(t *testing.T) {
				uncovered := serveLane(c.mw, scopeEcho(t), headered(c.req(), foreign))
				unknown := serveLane(c.mw, scopeEcho(t), headered(c.req(), wsUnknown))
				if uncovered.Code != unknown.Code {
					t.Fatalf("status %d for a live workspace, %d for one that does not exist",
						uncovered.Code, unknown.Code)
				}
				a := replaceOnce(t, uncovered.Body.String(), foreign)
				b := replaceOnce(t, unknown.Body.String(), wsUnknown)
				if a != b {
					t.Errorf("uncovered body %s differs from unknown body %s", a, b)
				}
			})

			// Last: it archives the workspace every subtest above resolves.
			t.Run("an archived workspace refuses it like a bad credential", func(t *testing.T) {
				if _, err := pool.Exec(context.Background(),
					`UPDATE workspaces SET archived_at = now() WHERE id = $1`, ws); err != nil {
					t.Fatalf("archive workspace: %v", err)
				}
				archived := serveLane(c.mw, scopeEcho(t), c.req())
				unknown := serveLane(c.mw, scopeEcho(t), c.bad())
				if archived.Code != unknown.Code || archived.Body.String() != unknown.Body.String() {
					t.Errorf("archived answers %d %s; an unknown credential answers %d %s — they must be identical",
						archived.Code, archived.Body, unknown.Code, unknown.Body)
				}
			})
		})
	}
}

// The environment key's scope is its ENVIRONMENT's, never environment_keys'
// own reserved columns — the fixture sets those to a value that cannot be
// confused with anything, so a resolver reading them fails loudly (§6.1).
func TestEnvironmentKeyScopeFollowsTheEnvironmentNotTheKey(t *testing.T) {
	pool := pgtest.NewPool(t)
	ws := newWorkspaceRow(t, pool, "B")
	env, key := environmentKeyInWorkspace(t, pool, ws)
	var keyWorkspace string
	if err := pool.QueryRow(context.Background(),
		`SELECT workspace_id FROM environment_keys WHERE environment_id = $1`, env).Scan(&keyWorkspace); err != nil {
		t.Fatalf("read key row: %v", err)
	}
	if keyWorkspace != laneWrongWorkspace {
		t.Fatalf("fixture did not garble the key's own column: %q", keyWorkspace)
	}
	res := serveLane(func(next http.Handler) http.Handler { return requireEnvironmentKey(pool, next) },
		scopeEcho(t), laneRequest(http.MethodGet, "/v1/environments/"+env+"/work", "Authorization", "Bearer "+key)())
	wantScopeBody(t, res, domain.Scope{OrgID: "default", WorkspaceID: ws, ProjectID: "default"})
}

// The shared resolver stamps, so the worker half of the dual-auth session
// routes carries both headers even on the 404 its own ownership check writes
// (§6.2: stamping the middleware instead would leave exactly these responses
// bare).
func TestSessionWorkerLaneStampsBeforeItsOwn404(t *testing.T) {
	pool := pgtest.NewPool(t)
	ws := newWorkspaceRow(t, pool, "B")
	_, key := environmentKeyInWorkspace(t, pool, ws)
	// NewSession brings its own environment, so this is a session the key does
	// not cover — the ownership check's 404.
	other, _ := pgtest.NewSession(t, pool, "self_hosted")
	res := serveLane(func(next http.Handler) http.Handler { return requireEnvironmentKeyForSession(pool, next) },
		scopeEcho(t),
		laneRequest(http.MethodGet, "/v1/sessions/"+other.String()+"/events", "Authorization", "Bearer "+key)())
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", res.Code, res.Body)
	}
	wantTenancyHeaders(t, res, "default", ws)
}

// The literal `default` is a valid workspace id here — this deployment's own
// frozen one, which the reference has no equivalent of.
func TestTheLiteralDefaultIsAValidWorkspaceHeader(t *testing.T) {
	pool := pgtest.NewPool(t)
	const key = "map-default-workspace-key"
	if err := EnsureAPIKey(context.Background(), pool, "scopelane", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	r := laneRequest(http.MethodGet, "/v1/agents", "x-api-key", key)()
	r.Header.Set(workspaceHeader, "default")
	res := serveLane(func(next http.Handler) http.Handler { return requireAPIKey(pool, next) }, scopeEcho(t), r)
	wantScopeBody(t, res, domain.Scope{OrgID: "default", WorkspaceID: "default", ProjectID: "default"})
}

// Org has one authority — the registry — so a key row whose org_id has drifted
// from its workspace's does not resolve at all. The join is composite for that
// reason, and the refusal must be the byte-identical 401 an unknown key gets:
// a drifted row is a row that names no tenant this deployment runs, and saying
// so differently would tell a caller the key exists.
func TestAnAPIKeyWhoseOrgDriftedDoesNotResolve(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	const key = "map-org-drift-key"
	if err := EnsureAPIKey(ctx, pool, "drift", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE api_keys SET org_id = 'other' WHERE key_hash = $1`, hashKey(key)); err != nil {
		t.Fatalf("drift the key's org: %v", err)
	}
	mw := func(next http.Handler) http.Handler { return requireAPIKey(pool, next) }
	drifted := serveLane(mw, scopeEcho(t), laneRequest(http.MethodGet, "/v1/agents", "x-api-key", key)())
	unknown := serveLane(mw, scopeEcho(t), laneRequest(http.MethodGet, "/v1/agents", "x-api-key", "map-no-such-key")())
	if drifted.Code != http.StatusUnauthorized {
		t.Fatalf("a drifted key answers %d (body %s), want 401", drifted.Code, drifted.Body)
	}
	if drifted.Body.String() != unknown.Body.String() {
		t.Errorf("a drifted key answers %s; an unknown one answers %s — they must be identical",
			drifted.Body, unknown.Body)
	}
	wantNoTenancyHeaders(t, drifted)
}

// The bootstrap marker reaches the request, and marks only the env-var-managed
// key — the row with created_by IS NULL (§6.8).
func TestBootstrapKeyMarksOnlyTheEnvVarManagedKey(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	const bootKey, consoleKey = "map-bootstrap-key", "map-console-key"
	if err := EnsureAPIKey(ctx, pool, "bootstrap", bootKey); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, created_by) VALUES ($1, 'console', $2, 'apikey_issuer')`,
		domain.NewID(domain.PrefixAPIKey).String(), hashKey(consoleKey)); err != nil {
		t.Fatalf("insert console key: %v", err)
	}
	mw := func(next http.Handler) http.Handler { return requireAPIKey(pool, next) }
	for _, tc := range []struct {
		name string
		key  string
		want bool
	}{
		{"the configured key is the bootstrap key", bootKey, true},
		{"a console-issued key is not", consoleKey, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := serveLane(mw, scopeEcho(t), laneRequest(http.MethodGet, "/v1/agents", "x-api-key", tc.key)())
			if got := laneEcho(t, res)["bootstrap"]; got != tc.want {
				t.Errorf("bootstrap = %v, want %v", got, tc.want)
			}
		})
	}
}

// laneWrongWorkspace is what the environment key's own reserved columns are
// set to: unreadable as a workspace id, so a resolver that read them could
// only fail visibly.
const laneWrongWorkspace = "wrkspc_this_column_is_never_read"

// environmentKeyInWorkspace places an environment in ws and issues a worker key
// for it, garbling the key row's own reserved columns.
func environmentKeyInWorkspace(t *testing.T, pool *pgxpool.Pool, ws string) (envID, key string) {
	t.Helper()
	ctx := context.Background()
	_, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := pool.Exec(ctx,
		`UPDATE environments SET workspace_id = $2 WHERE id = $1`, env, ws); err != nil {
		t.Fatalf("move environment: %v", err)
	}
	key, err := IssueEnvironmentKey(ctx, pool, env.String(), "worker")
	if err != nil {
		t.Fatalf("IssueEnvironmentKey: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE environment_keys SET org_id = $2, workspace_id = $2, project_id = $2 WHERE environment_id = $1`,
		env, laneWrongWorkspace); err != nil {
		t.Fatalf("garble key columns: %v", err)
	}
	return env.String(), key
}

func moveSessionToWorkspace(t *testing.T, pool *pgxpool.Pool, sessionID domain.ID, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET workspace_id = $2 WHERE id = $1`, sessionID, ws); err != nil {
		t.Fatalf("move session: %v", err)
	}
}

func newWorkspaceRow(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixWorkspace).String()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, org_id, name) VALUES ($1, 'default', $2)`, id, name); err != nil {
		t.Fatalf("insert workspace %s: %v", name, err)
	}
	return id
}

// laneRequest returns a builder rather than a request, because every subtest
// needs its own to set a header on.
func laneRequest(method, path, header, value string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set(header, value)
		return r
	}
}

func headered(r *http.Request, workspace string) *http.Request {
	r.Header.Set(workspaceHeader, workspace)
	return r
}

func serveLane(mw func(http.Handler) http.Handler, next http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mw(next).ServeHTTP(w, r)
	return w
}

// scopeEcho renders what the middleware attached. scopeFrom's !ok is an
// internal error at every call site, and here it is a test failure.
func scopeEcho(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := scopeFrom(r.Context())
		if !ok {
			t.Errorf("%s %s reached the handler with no scope on the context", r.Method, r.URL.Path)
			writeError(w, r, errAuth("no scope"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"org": s.OrgID, "workspace": s.WorkspaceID, "project": s.ProjectID,
			"bootstrap": bootstrapKeyFrom(r.Context()),
		})
	})
}

// notFoundHandler is an authenticated 4xx: the lane admitted the credential and
// the route answered a missing resource.
func notFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound("nothing here"))
	})
}

func laneEcho(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", res.Code, res.Body)
	}
	var echoed map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("decode echo %s: %v", res.Body, err)
	}
	return echoed
}

func wantScopeBody(t *testing.T, res *httptest.ResponseRecorder, want domain.Scope) {
	t.Helper()
	echoed := laneEcho(t, res)
	got := domain.Scope{}
	got.OrgID, _ = echoed["org"].(string)
	got.WorkspaceID, _ = echoed["workspace"].(string)
	got.ProjectID, _ = echoed["project"].(string)
	if got != want {
		t.Errorf("scope = %+v, want %+v", got, want)
	}
}

func wantTenancyHeaders(t *testing.T, res *httptest.ResponseRecorder, org, workspace string) {
	t.Helper()
	if got := res.Header().Get(orgHeader); got != org {
		t.Errorf("%s = %q, want %q", orgHeader, got, org)
	}
	if got := res.Header().Get(workspaceHeader); got != workspace {
		t.Errorf("%s = %q, want %q", workspaceHeader, got, workspace)
	}
}

// wantNoTenancyHeaders is the other half of the stamping schedule, and the half
// wantLaneError does not read: a response that resolved NO scope carries
// neither header. It covers the pre-auth 401 and both header refusals, which
// answer before there is a scope to stamp.
func wantNoTenancyHeaders(t *testing.T, res *httptest.ResponseRecorder) {
	t.Helper()
	if got := res.Header().Get(orgHeader); got != "" {
		t.Errorf("%s = %q on a response that resolved no scope, want absent", orgHeader, got)
	}
	if got := res.Header().Get(workspaceHeader); got != "" {
		t.Errorf("%s = %q on a response that resolved no scope, want absent", workspaceHeader, got)
	}
}

func wantLaneError(t *testing.T, res *httptest.ResponseRecorder, status int, errType, message string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", res.Code, status, res.Body)
	}
	var body struct {
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	if body.Error.Type != errType {
		t.Errorf("error.type = %q, want %q", body.Error.Type, errType)
	}
	if body.Error.Message != message {
		t.Errorf("error.message = %q, want %q", body.Error.Message, message)
	}
}
