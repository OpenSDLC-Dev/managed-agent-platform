package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// revokedKeyMessage is the reference's sentence for an environment key known
// to be revoked: recorded on the work poll (2026-09-19 custom-mixed-tools,
// custom-order-followup and self-hosted-docker `worker-network.json` idx 169,
// 51 and 63), on a skill version's content (2026-09-05 batch1 idx 84
// `rec82.envkey.dead-key-verify.t4-builtin`, batch2 idx 64
// `rec83.control.revoked-key-A.content`) and on GET /v1/models (2026-09-19
// custom-mixed-tools, custom-order-followup and self-hosted-latest-cli
// `worker-network.json` idx 170, 52 and 61).
const revokedKeyMessage = "OAuth access token has been revoked."

// deadKeyMessage is the one 401 an expired key and an unknown key share on
// the environment-key lanes. No recording holds a sentence the reference
// gives either of them alone, so it is this platform's.
const deadKeyMessage = "invalid environment key"

// skillsScopeMessage is the reference's 403 for a live environment key on the
// skill collection and a skill's own read (2026-09-03 batch2 idx 4
// `envkey.skills.collection-list` and idx 2 `envkey.skills.get-anthropic`,
// 2026-09-04 batch2 idx 146 `rec81.envkey.skills.get.custom`, 2026-09-05
// batch1 idx 51 `rec82.skills.list.envkey`).
const skillsScopeMessage = "OAuth token does not meet scope requirement any_of(org:skills, user:developer, user:managed_agents, user:skills, workspace:developer, workspace:skills)"

// missingKeyMessage is the management lane's 401 for a request offering no
// credential it recognises (requireAPIKey; 2026-09-19 self-hosted-docker
// `worker-network.json` idx 0).
const missingKeyMessage = "x-api-key header is required"

// revokeViaConsole revokes the key issued under name, through the console
// route an operator uses.
func revokeViaConsole(t *testing.T, s *tserver, envID, name string) {
	t.Helper()
	keys, _, err := api.ListEnvironmentKeys(context.Background(), s.pool, envID, 100, 0)
	if err != nil {
		t.Fatalf("list %s's keys: %v", envID, err)
	}
	for _, k := range keys {
		if k.Name != name {
			continue
		}
		res := s.doRaw(http.MethodPost, consoleRevoke(envID, k.ID), nil, map[string]string{"x-api-key": testKey})
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("revoke %q: status %d", name, res.StatusCode)
		}
		return
	}
	t.Fatalf("no live key named %q on %s", name, envID)
}

// sendBounded sends one request with a deadline and returns its status and
// decoded body: the event stream holds a served response open, and a refused
// one closes at once.
func sendBounded(t *testing.T, s *tserver, method, path string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.roundTrip(ctx, method, path, nil, headers)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return readJSON(t, res)
}

// TestARevokedEnvironmentKeyIsAnsweredAsRevokedOnEveryPublicRoute: a revoked
// key this platform minted gets the reference's sentence wherever it is
// presented. Every route server.go registers is requested with a revoked key
// from each kind of environment — the work API, the session lane, the file
// download and the skill reads, ahead of their kind gates, and the management
// lane and the console namespace too — plus GET /v1/models, the route the
// reference was recorded answering it on outside every key lane, which this
// platform does not serve. The one exception is the internal gate-config
// endpoint: it is off the public wire, no reference route, and takes a gate
// token and nothing else, so any other credential there is an invalid gate
// token and is answered as one.
func TestARevokedEnvironmentKeyIsAnsweredAsRevokedOnEveryPublicRoute(t *testing.T) {
	s := newTestServer(t)
	envID, sessionID, _ := selfHostedWorker(t, s, "beside-the-revoked")
	cloudEnv := createEnvironment(t, s, map[string]any{"name": "revoked-cloud"})["id"].(string)
	skill := s.createSkill(t)
	keys := map[string]string{
		"self_hosted": issueViaConsole(t, s, envID, "revoked-self-hosted"),
		"cloud":       issueViaConsole(t, s, cloudEnv, "revoked-cloud"),
	}
	if res, raw := s.poll(t, envID, asBearer(keys["self_hosted"])); res.StatusCode != http.StatusOK {
		t.Fatalf("the self_hosted key does not poll before its revocation: %d %s", res.StatusCode, raw)
	}
	revokeViaConsole(t, s, envID, "revoked-self-hosted")
	revokeViaConsole(t, s, cloudEnv, "revoked-cloud")

	requests := []string{"GET /v1/models"}
	for _, reg := range parseRoutes(t, "server.go") {
		if reg.isFunc {
			continue // a 404 or 405 closure, not a route
		}
		pattern := resolveRoutePattern(t, reg.pattern)
		method, template, _ := strings.Cut(pattern, " ")
		if template == gateconfig.Path {
			for _, key := range keys {
				status, body := sendBounded(t, s, method, template, asBearer(key))
				wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", "invalid gate token")
			}
			continue
		}
		requests = append(requests, method+" "+fillRoute(template, envID, sessionID,
			"file_placeholder", "work_placeholder", skill["id"].(string), skill["latest_version_id"].(string)))
	}
	for kind, key := range keys {
		for _, request := range requests {
			method, path, _ := strings.Cut(request, " ")
			t.Run(kind+" "+request, func(t *testing.T) {
				status, body := sendBounded(t, s, method, path, asBearer(key))
				wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", revokedKeyMessage)
			})
		}
	}

	// A management key beside a revoked environment key is still the
	// management lane's caller: machine key first.
	res := s.doRaw(http.MethodGet, "/v1/agents", nil,
		map[string]string{"x-api-key": testKey, "Authorization": "Bearer " + keys["self_hosted"]})
	if status, body := readJSON(t, res); status != http.StatusOK {
		t.Errorf("x-api-key beside a revoked environment key: status %d, body %v; want 200", status, body)
	}
}

// TestTheManagementLaneAnswersALiveEnvironmentKeyAsRecorded: a live
// environment key presented as a Bearer on a management route gets the
// reference's recorded refusal there, in every identity mode, since it is a
// machine credential's answer and says nothing about whether humans can sign
// in. A percent-encoded spelling the router decodes takes the refusal too
// (TestALiveKeysRefusalReadsTheRequestAsTheRouterDoes has the rest of the
// routing). A route no recording reaches keeps the lane's own missing-key 401,
// a revoked key gets its own sentence in every mode, and a management key
// beside the environment key is served.
func TestTheManagementLaneAnswersALiveEnvironmentKeyAsRecorded(t *testing.T) {
	servers := map[string]func(*testing.T) *tserver{
		"identity disabled": newTestServer,
		"oidc":              func(t *testing.T) *tserver { return newLaneServer(t).tserver },
		"trusted_proxy": func(t *testing.T) *tserver {
			return newLaneServerWith(t, func(c *identity.Config) {
				c.Mode = identity.ModeTrustedProxy
				c.AssertionHeader = "x-goog-iap-jwt-assertion"
			}).tserver
		},
	}
	for mode, start := range servers {
		t.Run(mode, func(t *testing.T) {
			s := start(t)
			skill := s.createSkill(t)["id"].(string)
			selfHosted := issueKey(t, s.pool, selfHostedEnv(t, s, "mgmt-lane"), "host")
			cloud := issueViaConsole(t, s, createEnvironment(t, s, map[string]any{"name": "mgmt-lane-cloud"})["id"].(string), "cloud-host")

			recorded := []struct {
				path, errType, message string
				status                 int
			}{
				// 2026-09-03 batch2 idx 5 `envkey.agents.list-should-refuse`.
				{"/v1/agents?beta=true&limit=1", "authentication_error", "Authentication failed", http.StatusUnauthorized},
				{"/v1/agents", "authentication_error", "Authentication failed", http.StatusUnauthorized},
				// 2026-09-05 batch1 idx 51 `rec82.skills.list.envkey`.
				{"/v1/skills?limit=2", "permission_error", skillsScopeMessage, http.StatusForbidden},
				// 2026-09-03 batch2 idx 2 `envkey.skills.get-anthropic`.
				{"/v1/skills/xlsx?beta=true", "permission_error", skillsScopeMessage, http.StatusForbidden},
				// 2026-09-04 batch2 idx 146 `rec81.envkey.skills.get.custom`.
				{"/v1/skills/" + skill + "?beta=true", "permission_error", skillsScopeMessage, http.StatusForbidden},
				// Spellings the router decodes to the same routes.
				{"/v1/%61gents", "authentication_error", "Authentication failed", http.StatusUnauthorized},
				{"/v1/sk%69lls/" + skill, "permission_error", skillsScopeMessage, http.StatusForbidden},
			}
			unrecorded := []struct{ method, path string }{
				{http.MethodGet, "/v1/sessions"},
				{http.MethodGet, "/v1/environments"},
				{http.MethodGet, "/v1/agents/agent_011CZkYpogX7uDKUyvBTophP"},
				{http.MethodPost, "/v1/agents"},
				{http.MethodPost, "/v1/skills"},
				{http.MethodDelete, "/v1/skills/" + skill},
				{http.MethodGet, consoleTokens("env_011CZkYpogX7uDKUyvBTophP")},
			}
			for kind, key := range map[string]string{"self_hosted": selfHosted, "cloud": cloud} {
				for _, r := range recorded {
					status, body := readJSON(t, s.doRaw(http.MethodGet, r.path, nil, asBearer(key)))
					t.Run(kind+" GET "+r.path, func(t *testing.T) {
						wantErrMsg(t, status, body, r.status, r.errType, r.message)
					})
				}
				for _, u := range unrecorded {
					status, body := readJSON(t, s.doRaw(u.method, u.path, nil, asBearer(key)))
					t.Run(kind+" "+u.method+" "+u.path, func(t *testing.T) {
						wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", missingKeyMessage)
					})
				}
			}

			envID := selfHostedEnv(t, s, "mgmt-lane-revoked")
			revoked := issueViaConsole(t, s, envID, "revoked-host")
			revokeViaConsole(t, s, envID, "revoked-host")
			status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil, asBearer(revoked)))
			wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", revokedKeyMessage)

			for _, path := range []string{"/v1/agents", "/v1/skills", "/v1/skills/" + skill} {
				res := s.doRaw(http.MethodGet, path, nil,
					map[string]string{"x-api-key": testKey, "Authorization": "Bearer " + selfHosted})
				if status, body := readJSON(t, res); status != http.StatusOK {
					t.Errorf("x-api-key beside a live environment key on %s: status %d, body %v; want 200", path, status, body)
				}
			}
		})
	}
}

// TestTheManagementLaneLeavesEveryOtherBearerAlone: only a live or a revoked
// environment key changes the management lane's answer. An expired key and an
// unknown one keep the missing-key 401, as does a key the lane does not
// recognise as an environment key at all: a grandfathered pre-0021 key, whose
// value the operator chose and which carries no sk-map-env01- prefix, still
// polls its queue but is not looked up here.
func TestTheManagementLaneLeavesEveryOtherBearerAlone(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "other-bearers")
	expired := issueKey(t, s.pool, envID, "expired")
	if _, err := s.pool.Exec(ctx,
		`UPDATE environment_keys SET expires_at = now() - interval '1 second' WHERE environment_id = $1 AND name = 'expired'`,
		envID); err != nil {
		t.Fatalf("age the key past its expiry: %v", err)
	}
	const legacy = "operator-chosen-legacy-worker-key"
	sum := sha256.Sum256([]byte(legacy))
	if _, err := s.pool.Exec(ctx, `INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)`,
		domain.NewID(domain.PrefixEnvironmentKey).String(), envID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert a pre-0021 key: %v", err)
	}
	if res, raw := s.poll(t, envID, asBearer(legacy)); res.StatusCode != http.StatusOK {
		t.Fatalf("the grandfathered key does not poll: %d %s", res.StatusCode, raw)
	}

	for name, key := range map[string]string{
		"an expired key":      expired,
		"an unknown key":      "sk-map-env01-not-a-real-key",
		"a grandfathered key": legacy,
	} {
		status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil, asBearer(key)))
		t.Run(name, func(t *testing.T) {
			wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", missingKeyMessage)
		})
	}
}

// TestARevokedGrandfatheredKeyIsAnsweredAsDead: the revoked sentence is only
// for a key carrying the mint prefix (docs/DIVERGENCES.md's *Environment key
// revocation and expiry* says why). A revoked pre-0021 key gets the dead-key
// answer of whichever lane it reaches — the key lanes' `invalid environment
// key`, the management lane's missing-key 401.
func TestARevokedGrandfatheredKeyIsAnsweredAsDead(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "grandfathered-revoked")
	const legacy = "operator-chosen-revoked-worker-key"
	sum := sha256.Sum256([]byte(legacy))
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO environment_keys (id, environment_id, key_hash, revoked_at) VALUES ($1, $2, $3, now())`,
		domain.NewID(domain.PrefixEnvironmentKey).String(), envID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert a revoked pre-0021 key: %v", err)
	}

	status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work/poll", nil, asBearer(legacy)))
	wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", deadKeyMessage)
	status, body = readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil, asBearer(legacy)))
	wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", missingKeyMessage)
}

// TestAHumanBesideAnEnvironmentKeyIsServedAsTheHuman: in trusted_proxy mode a
// proxy's assertion and a worker's Bearer can ride one request, and the human
// the proxy vouched for is served, whatever state the stale Bearer is in: on
// the management lane an environment key is only ever refused, so it is
// answered only when no human credential is offered. An assertion that fails
// verification is the human lane's own 401, not the environment key's answer.
// (In oidc mode the two cannot meet: the human's credential is the Bearer.)
func TestAHumanBesideAnEnvironmentKeyIsServedAsTheHuman(t *testing.T) {
	const header = "x-goog-iap-jwt-assertion"
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = header
	})
	envID := s.env()
	live := issueViaConsole(t, s.tserver, envID, "stale-but-live")
	revoked := issueViaConsole(t, s.tserver, envID, "stale-and-revoked")
	revokeViaConsole(t, s.tserver, envID, "stale-and-revoked")
	viewer := s.token("platform-read")

	for name, key := range map[string]string{"a live key": live, "a revoked key": revoked} {
		for _, path := range []string{"/v1/agents", "/v1/skills"} {
			res := s.doRaw(http.MethodGet, path, nil, map[string]string{header: viewer, "Authorization": "Bearer " + key})
			if status, body := readJSON(t, res); status != http.StatusOK {
				t.Errorf("an assertion beside %s on %s: status %d, body %v; want the human served", name, path, status, body)
			}
		}
	}
	status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil,
		map[string]string{header: "not-a-token", "Authorization": "Bearer " + live}))
	if status != http.StatusUnauthorized || envelopeMessage(body) == "Authentication failed" {
		t.Errorf("a bad assertion beside a live key: status %d, body %v; want the human lane's 401", status, body)
	}
}

// TestEveryEnvironmentKeyRefusalNamesARegisteredRoute: the recorded refusals
// are keyed by the patterns server.go registers and looked up by the pattern
// the router resolves, on the management lane alone. So each key must be a
// registration, and a live key sent to it must reach the management lane and
// draw that refusal: an entry naming a route dispatchAuth gives to another
// lane — an environment-key lane serving the key — would never fire.
func TestEveryEnvironmentKeyRefusalNamesARegisteredRoute(t *testing.T) {
	registered := map[string]bool{}
	for _, reg := range parseRoutes(t, "server.go") {
		if !reg.isFunc {
			registered[reg.pattern] = true
		}
	}
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "refusal-table")
	key := issueKey(t, s.pool, envID, "host")
	skill := s.createSkill(t)
	for pattern, refusal := range api.EnvironmentKeyRefusalsForTest() {
		if !registered[pattern] {
			t.Errorf("environmentKeyRefusals names %q, which server.go does not register", pattern)
			continue
		}
		method, template, _ := strings.Cut(pattern, " ")
		path := fillRoute(template, envID, "", "", "", skill["id"].(string), skill["latest_version_id"].(string))
		status, body := readJSON(t, s.doRaw(method, path, nil, asBearer(key)))
		t.Run(pattern, func(t *testing.T) {
			wantErrMsg(t, status, body, refusal.Status, refusal.Type, refusal.Message)
		})
	}
}

// TestALiveKeysRefusalReadsTheRequestAsTheRouterDoes: the recorded refusal
// goes with the route the router would serve. A HEAD, which it serves through
// the GET registration, takes it — on the skills a 403 nothing else gives, on
// the agents list a 401 the log line tells from the missing-key one. A path
// the router would redirect instead of serving (a doubled slash, a dot
// segment) does not: the route was recorded refusing a request it served.
func TestALiveKeysRefusalReadsTheRequestAsTheRouterDoes(t *testing.T) {
	s := newTestServer(t)
	key := issueKey(t, s.pool, selfHostedEnv(t, s, "router-reading"), "host")
	skill := s.createSkill(t)["id"].(string)
	logs := captureLogs(t, slog.LevelInfo)
	refused := func() int { return strings.Count(logs(), "management route refused: environment key") }

	for path, want := range map[string]int{"/v1/skills": http.StatusForbidden, "/v1/skills/" + skill: http.StatusForbidden} {
		res := s.doRaw(http.MethodHead, path, nil, asBearer(key))
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("HEAD %s: status %d, want the recorded %d", path, res.StatusCode, want)
		}
	}
	before := refused()
	res := s.doRaw(http.MethodHead, "/v1/agents", nil, asBearer(key))
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || refused() != before+1 {
		t.Errorf("HEAD /v1/agents: status %d, refusals logged %d; want the recorded 401, logged as the refusal",
			res.StatusCode, refused()-before)
	}

	for _, path := range []string{"/v1//agents", "/v1/./agents", "/v1/agents/x/..", "/v1//skills"} {
		before := refused()
		status, body := readJSON(t, s.doRaw(http.MethodGet, path, nil, asBearer(key)))
		wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", missingKeyMessage)
		if refused() != before {
			t.Errorf("GET %s: a path the router redirects drew the recorded refusal", path)
		}
	}
}

// TestTheManagementLaneFailsClosedWhenTheKeyLookupFails: the management lane's
// environment-key lookup runs for a request nothing has authenticated, so a
// failed lookup must not turn that request into a 500 and an ERROR line. It
// answers the missing-key 401 the lane gives without the lookup, and the
// failure goes to the operator's log at Warn.
func TestTheManagementLaneFailsClosedWhenTheKeyLookupFails(t *testing.T) {
	// Nothing listens on port 1, so every query fails at connect.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=2")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	logs := captureLogs(t, slog.LevelInfo)
	srv := httptest.NewServer(api.NewHandler(pool, blobtest.Mem(), nil, nil))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}

	status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil, asBearer("sk-map-env01-looked-up-against-nothing")))
	wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", missingKeyMessage)
	if out := logs(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "environment key lookup failed") ||
		strings.Contains(out, "level=ERROR") {
		t.Errorf("want the failed lookup logged at Warn and nothing at Error:\n%s", out)
	}
}

// TestTheManagementLaneLooksUpOnlyAnEnvironmentKeyShapedBearer: recognising
// an environment key costs the management lane one lookup, and only for a
// Bearer carrying the prefix every key this platform mints carries. A
// management key's request — with an environment key beside it or not — and
// any other Bearer run no statement against environment_keys.
func TestTheManagementLaneLooksUpOnlyAnEnvironmentKeyShapedBearer(t *testing.T) {
	var log statementLog
	s := newTracedTestServer(t, &log)
	key := issueKey(t, s.pool, selfHostedEnv(t, s, "traced"), "host")

	for name, headers := range map[string]map[string]string{
		"a management key":                           {"x-api-key": testKey},
		"a management key beside an environment key": {"x-api-key": testKey, "Authorization": "Bearer " + key},
		"another vendor's token":                     asBearer("sk-ant-oat01-not-ours"),
		"a JWT-shaped Bearer":                        asBearer("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"),
	} {
		log.reset()
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, headers)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if n := log.count("environment_keys"); n != 0 {
			t.Errorf("%s: the management lane ran %d environment_keys lookups, want none", name, n)
		}
	}

	log.reset()
	status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/agents", nil, asBearer(key)))
	wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", "Authentication failed")
	if n := log.count("environment_keys"); n != 1 {
		t.Errorf("an environment key: %d environment_keys lookups, want exactly one", n)
	}
}

// TestADeletedEnvironmentTakesItsKeysWithIt pins this platform's side of a
// recorded divergence. The reference keeps a key after its environment is
// deleted and answers its work stats 404 not_found_error "Environment env_…
// not found." (2026-09-05 batch2 idx 55 `rec83.orphan.stats.after-delete`,
// on idx 52's cloud environment). Here a key is a row that cascades with its
// environment (environment_keys.environment_id ON DELETE CASCADE), so the key
// is unknown afterwards and answers the dead-key 401; answering the 404 needs
// the key to outlive the environment, a schema change docs/DIVERGENCES.md
// argues against building (*A deleted environment's key*).
func TestADeletedEnvironmentTakesItsKeysWithIt(t *testing.T) {
	s := newTestServer(t)
	envID := createEnvironment(t, s, map[string]any{"name": "orphan"})["id"].(string)
	key := issueViaConsole(t, s, envID, "orphan")
	stats := "/v1/environments/" + envID + "/work/stats"

	if status, body := readJSON(t, s.doRaw(http.MethodGet, stats, nil, asBearer(key))); status != http.StatusOK {
		t.Fatalf("stats before the delete: status %d, body %v", status, body)
	}
	if status, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil); status != http.StatusOK {
		t.Fatalf("delete the environment: status %d, body %v", status, body)
	}
	status, body := readJSON(t, s.doRaw(http.MethodGet, stats, nil, asBearer(key)))
	wantErrMsg(t, status, body, http.StatusUnauthorized, "authentication_error", deadKeyMessage)
	var left int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM environment_keys WHERE environment_id = $1`, envID).Scan(&left); err != nil {
		t.Fatalf("count the environment's keys: %v", err)
	}
	if left != 0 {
		t.Errorf("%d keys outlived their environment, want none", left)
	}
}
