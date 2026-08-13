package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

// The identity lane over real HTTP, against a real fake OpenID Provider. Two
// rules hold throughout, both inherited from slice 1: time is the frozen
// identitytest.Clock handed to the verifier as Config.Now, so nothing sleeps;
// and the provider is reached through idp.Client(), because the production
// client's dial guard refuses loopback by design.

const laneAudience = "console"

// laneRoles is the deployment's claim value → role map. The three keys are
// deliberately not the role names: a real deployment maps its own group names,
// and a test that used "admin" as the claim value would still pass with the
// mapping dropped entirely.
func laneRoles() map[string]identity.Role {
	return map[string]identity.Role{
		"platform-admins": identity.RoleAdmin,
		"platform-devs":   identity.RoleDeveloper,
		"platform-read":   identity.RoleViewer,
	}
}

// laneServer is a control plane with human authentication enabled, plus the
// provider whose tokens it accepts.
type laneServer struct {
	*tserver
	idp   *identitytest.IdP
	clock *identitytest.Clock
}

// newLaneServer starts a control plane in oidc mode. The management key still
// works throughout — that is half of what these tests are for.
func newLaneServer(t *testing.T) *laneServer {
	t.Helper()
	return newLaneServerWith(t, func(*identity.Config) {})
}

// newLaneServerWith is newLaneServer with the verifier's config adjusted — the
// seam the trusted_proxy tests use, rather than a second fixture.
func newLaneServerWith(t *testing.T, adjust func(*identity.Config)) *laneServer {
	t.Helper()
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))

	cfg := identity.Config{
		Mode:       identity.ModeOIDC,
		Issuer:     idp.Issuer(),
		Audience:   laneAudience,
		JWKSURL:    idp.JWKSURL(),
		RoleMap:    laneRoles(),
		HTTPClient: idp.Client(),
		Now:        clock.Now,
	}
	adjust(&cfg)

	v, err := identity.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}

	pool := newPoolWithKey(t)
	blobs := blobtest.Mem()
	// A real cipher, matching newTestServer. With nil, the vault-credential write
	// routes answer errSecretsUnavailable — a 500 — and the role-matrix test's
	// admin rows would pass only for as long as body decoding keeps failing
	// first, turning a secrets-configuration detail into a load-bearing part of
	// an authorization test.
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	srv := httptest.NewServer(api.NewHandler(pool, blobs, cipher, v))
	t.Cleanup(srv.Close)
	return &laneServer{
		tserver: &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs},
		idp:     idp,
		clock:   clock,
	}
}

// claims is a live claim set for this deployment: the fixture's minimum, plus
// the descriptive claims a principals row records and the role values under
// test.
func (s *laneServer) claims(roles ...string) map[string]any {
	c := s.idp.Claims(laneAudience, s.clock.Now())
	c["email"] = "ada@corp.example"
	c["name"] = "Ada Lovelace"
	values := make([]any, len(roles))
	for i, r := range roles {
		values[i] = r
	}
	c["roles"] = values
	return c
}

// token mints a live token carrying the named role claim values.
func (s *laneServer) token(roles ...string) string {
	s.t.Helper()
	return s.idp.Mint(s.t, s.claims(roles...))
}

// bearer issues a request carrying a human token and nothing else.
func (s *laneServer) bearer(method, path, token string, body any) *http.Response {
	s.t.Helper()
	return s.doRaw(method, path, body, map[string]string{"Authorization": "Bearer " + token})
}

// env creates an environment through the management lane and returns its id.
// Creating it with the api key keeps the fixture independent of whatever the
// identity lane is allowed to do.
//
// self_hosted because that is the only kind an environment key is issued for
// (consoleapi.go's createEnvironmentKey): a cloud environment's work runs in
// this platform's own executor, which holds no key.
func (s *laneServer) env() string {
	s.t.Helper()
	return selfHostedEnv(s.t, s.tserver, "lane-test")
}

// laneRead reads a response's status, its error envelope's inner type, and the
// raw body, closing it. The type is "" for a success.
func laneRead(t *testing.T, res *http.Response) (int, string, string) {
	t.Helper()
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	return res.StatusCode, env.Error.Type, string(raw)
}

// laneStatus is laneRead without the body, for the many assertions that only
// care about the status and the error type.
func laneStatus(t *testing.T, res *http.Response) (int, string) {
	t.Helper()
	status, errType, _ := laneRead(t, res)
	return status, errType
}

// laneMessage pulls error.message out of a response body.
func laneMessage(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	return env.Error.Message
}

// countPrincipals is the JIT-provisioning observation point: the lane's only
// side effect on the database.
func countPrincipals(t *testing.T, s *laneServer) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM principals`).Scan(&n); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	return n
}

// TestIdentityLaneAuthenticatesAHuman is the lane end to end: a real token from
// a real provider reaches a handler, and a principals row afterwards records who
// it was.
func TestIdentityLaneAuthenticatesAHuman(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()

	status, errType := laneStatus(t, s.bearer(http.MethodGet, consoleTokens(envID), s.token("platform-admins"), nil))
	if status != http.StatusOK {
		t.Fatalf("admin listing environment keys: status %d, error type %q, want 200", status, errType)
	}

	if n := countPrincipals(t, s); n != 1 {
		t.Fatalf("principals rows = %d, want exactly 1", n)
	}
	var id, issuer, subject, email, name string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id, issuer, subject, email, display_name FROM principals`).
		Scan(&id, &issuer, &subject, &email, &name); err != nil {
		t.Fatalf("read principal: %v", err)
	}
	if !strings.HasPrefix(id, "principal_") {
		t.Errorf("principal id = %q, want the principal_ prefix", id)
	}
	if issuer != s.idp.Issuer() {
		t.Errorf("issuer = %q, want %q", issuer, s.idp.Issuer())
	}
	if subject != "user-1" || email != "ada@corp.example" || name != "Ada Lovelace" {
		t.Errorf("principal = (%q, %q, %q); the claims the token carried must be recorded",
			subject, email, name)
	}

	// There is no role column to read, which is the point — assert its absence, so
	// a later migration that adds one fails here and has to argue with 0022's
	// comment first.
	var hasRole bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'principals' AND column_name = 'role')`).Scan(&hasRole); err != nil {
		t.Fatalf("check for a role column: %v", err)
	}
	if hasRole {
		t.Error("principals has a role column; the role is the provider's answer per request, not a copy that can go stale here")
	}
}

// TestIdentityLaneProvisionsOnceAndRefreshes pins JIT provisioning's two halves:
// a returning human keeps the id earlier requests were stamped with, and the
// descriptive claims follow the provider rather than freezing at first sight.
func TestIdentityLaneProvisionsOnceAndRefreshes(t *testing.T) {
	s := newLaneServer(t)
	path := consoleTokens(s.env())

	if status, errType := laneStatus(t, s.bearer(http.MethodGet, path, s.token("platform-admins"), nil)); status != http.StatusOK {
		t.Fatalf("first request: status %d, error %q, want 200", status, errType)
	}
	var first string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM principals`).Scan(&first); err != nil {
		t.Fatalf("read principal: %v", err)
	}

	// The same human — same issuer, same sub — with a changed name at the provider.
	claims := s.claims("platform-admins")
	claims["name"] = "Ada L. Byron"
	if status, errType := laneStatus(t, s.bearer(http.MethodGet, path, s.idp.Mint(t, claims), nil)); status != http.StatusOK {
		t.Fatalf("second request: status %d, error %q, want 200", status, errType)
	}

	if n := countPrincipals(t, s); n != 1 {
		t.Errorf("principals rows = %d after two requests by one human, want 1", n)
	}
	var second, name string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id, display_name FROM principals`).Scan(&second, &name); err != nil {
		t.Fatalf("read principal: %v", err)
	}
	if second != first {
		t.Errorf("principal id changed from %q to %q; anything that referenced the first would stop resolving", first, second)
	}
	if name != "Ada L. Byron" {
		t.Errorf("display_name = %q, want the refreshed value", name)
	}
}

// TestIdentityLaneDefaultDenies is the half of default-deny that survives slice
// 3. Slice 2 could show it with any route, since every route sat at the floor;
// now that the matrix has relaxed them, the property lives at the other end of
// Role.AtLeast: a human the deployment's role map does not recognise satisfies
// nothing, so they are refused on the most permissive route there is, while the
// same route still serves the management key.
//
// This is what an unmapped group looks like in practice — a real person, a valid
// token, an IdP group nobody mapped — and it must never read as permission.
func TestIdentityLaneDefaultDenies(t *testing.T) {
	s := newLaneServer(t)

	for _, claim := range []string{"unmapped-group", "Platform-Read", "platform-read-only"} {
		status, errType := laneStatus(t, s.bearer(http.MethodGet, "/v1/agents", s.token(claim), nil))
		if status != http.StatusForbidden {
			t.Errorf("GET /v1/agents as %q: status %d, want 403 — an unmapped claim value is not a role", claim, status)
		}
		if errType != "permission_error" {
			t.Errorf("GET /v1/agents as %q: error type %q, want permission_error", claim, errType)
		}
	}

	// A token carrying no role claim at all is the same case, reached differently.
	if status, _ := laneStatus(t, s.bearer(http.MethodGet, "/v1/agents", s.token(), nil)); status != http.StatusForbidden {
		t.Errorf("GET /v1/agents with no role claim: status %d, want 403", status)
	}

	// The control: the same route, the same server, the machine key.
	if status, _ := s.do(http.MethodGet, "/v1/agents", nil); status != http.StatusOK {
		t.Errorf("GET /v1/agents with x-api-key: status %d, want 200 — the key lane has no role to check", status)
	}
}

// TestIdentityLaneEnvironmentKeyRoutesRequireAdmin is the role mechanism's first
// real use. The whole surface is gated, the list included: which hosts hold keys,
// and what they are named, is itself the inventory an attacker would want.
func TestIdentityLaneEnvironmentKeyRoutesRequireAdmin(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()
	path := consoleTokens(envID)

	for _, tc := range []struct {
		role string
		want int
	}{
		{role: "platform-read", want: http.StatusForbidden},
		{role: "platform-devs", want: http.StatusForbidden},
		{role: "platform-admins", want: http.StatusOK},
	} {
		status, errType := laneStatus(t, s.bearer(http.MethodGet, path, s.token(tc.role), nil))
		if status != tc.want {
			t.Errorf("GET tokens as %s: status %d, want %d (error type %q)", tc.role, status, tc.want, errType)
		}
		if tc.want == http.StatusForbidden && errType != "permission_error" {
			t.Errorf("GET tokens as %s: error type %q, want permission_error", tc.role, errType)
		}

		body := map[string]any{"name": "host-" + tc.role}
		status, errType = laneStatus(t, s.bearer(http.MethodPost, path, s.token(tc.role), body))
		if status != tc.want {
			t.Errorf("POST tokens as %s: status %d, want %d (error type %q)", tc.role, status, tc.want, errType)
		}
	}

	// The revoke route is gated too, and denies before it decides whether the key
	// exists — a 404 here would be a probe for key ids.
	status, errType := laneStatus(t,
		s.bearer(http.MethodPost, consoleRevoke(envID, "envkey_missing"), s.token("platform-devs"), nil))
	if status != http.StatusForbidden || errType != "permission_error" {
		t.Errorf("revoke as a developer: status %d, error type %q, want 403 permission_error", status, errType)
	}

	// The denial names the requirement and never the caller — a 403 that told a
	// viewer they are a viewer would map their own authority for them.
	_, _, body := laneRead(t, s.bearer(http.MethodGet, path, s.token("platform-read"), nil))
	if !strings.Contains(body, "admin") {
		t.Errorf("denial %q does not name the required role", body)
	}
	for _, leak := range []string{"viewer", "platform-read"} {
		if strings.Contains(body, leak) {
			t.Errorf("denial %q names the caller's own role or claim value", body)
		}
	}
}

// TestIdentityLaneAPIKeyRoutesRequireAdmin is the environment-key section's twin
// for management keys (plan 32 slice 2, #378), and gated the same way and for a
// sharper reason: this surface mints the credential that reaches every /v1 route,
// so a developer who could issue one would hold admin by another name. The
// listing is gated with the mutations because which credentials exist, what they
// are named and who issued them is itself the inventory an attacker would want.
func TestIdentityLaneAPIKeyRoutesRequireAdmin(t *testing.T) {
	s := newLaneServer(t)

	for _, tc := range []struct {
		role string
		want int
	}{
		{role: "platform-read", want: http.StatusForbidden},
		{role: "platform-devs", want: http.StatusForbidden},
		{role: "platform-admins", want: http.StatusOK},
	} {
		status, errType := laneStatus(t, s.bearer(http.MethodGet, consoleAPIKeysPath, s.token(tc.role), nil))
		if status != tc.want {
			t.Errorf("GET api_keys as %s: status %d, want %d (error type %q)", tc.role, status, tc.want, errType)
		}
		if tc.want == http.StatusForbidden && errType != "permission_error" {
			t.Errorf("GET api_keys as %s: error type %q, want permission_error", tc.role, errType)
		}

		body := map[string]any{"name": "key-" + tc.role}
		status, errType = laneStatus(t, s.bearer(http.MethodPost, consoleAPIKeysPath, s.token(tc.role), body))
		if status != tc.want {
			t.Errorf("POST api_keys as %s: status %d, want %d (error type %q)", tc.role, status, tc.want, errType)
		}
	}

	// The update route denies before it decides whether the key exists — a 404
	// here would be a probe for key ids, from a caller who may not read the list.
	status, errType := laneStatus(t, s.bearer(http.MethodPost,
		consoleAPIKey("apikey_"+strings.Repeat("a", 24)), s.token("platform-devs"),
		map[string]any{"status": "archived"}))
	if status != http.StatusForbidden || errType != "permission_error" {
		t.Errorf("update as a developer: status %d, error type %q, want 403 permission_error", status, errType)
	}

	// An admin who issues a key over the identity lane is recorded as its creator
	// by their stable principal_ id — the audit answer plan 31 shipped principals
	// for, and the reason created_by is not merely decorative.
	_, _, raw := laneRead(t, s.bearer(http.MethodPost, consoleAPIKeysPath, s.token("platform-admins"),
		map[string]any{"name": "issued-by-a-human"}))
	var issued struct {
		CreatedBy struct{ ID, Type string } `json:"created_by"`
	}
	if err := json.Unmarshal([]byte(raw), &issued); err != nil {
		t.Fatalf("decode issuance: %v (body %s)", err, raw)
	}
	if issued.CreatedBy.Type != "principal" || !strings.HasPrefix(issued.CreatedBy.ID, "principal_") {
		t.Errorf("created_by = %+v, want the issuing human's principal_ id", issued.CreatedBy)
	}
}

// TestIdentityLaneRejectionsAreUniform401 pins that every bad credential answers
// alike. The lane must not become an oracle telling an attacker whether the
// issuer was wrong, the audience was wrong, or the token had merely expired.
func TestIdentityLaneRejectionsAreUniform401(t *testing.T) {
	s := newLaneServer(t)
	path := consoleTokens(s.env())

	// A second provider, ROTATED before use. identitytest pools its keys per test
	// binary and NewIdP always takes index 0, so two fresh IdPs sign with the same
	// RSA key under the same kid — a token from `other` would verify here on its
	// signature alone, and only the issuer check would refuse it. Rotating gives
	// this provider key material and a kid our verifier has never published, which
	// is what the unknown-key row below actually needs.
	other := identitytest.NewIdP(t)
	other.Rotate(t)

	wrongIssuer := other.Claims(laneAudience, s.clock.Now())
	wrongIssuer["roles"] = []any{"platform-admins"}

	expired := s.claims("platform-admins")
	expired["exp"] = s.clock.Now().Add(-2 * time.Hour).Unix()

	wrongAudience := s.claims("platform-admins")
	wrongAudience["aud"] = "some-other-app"

	var messages []string
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "wrong issuer", token: other.Mint(t, wrongIssuer)},
		{name: "wrong audience", token: s.idp.Mint(t, wrongAudience)},
		{name: "expired", token: s.idp.Mint(t, expired)},
		{name: "signed by a key this deployment never published", token: other.Mint(t, s.claims("platform-admins"))},
		{name: "not a JWS", token: "a.b.c"},
		// alg:none carrying a non-empty signature segment, so it has the JWT
		// silhouette and reaches the verifier. The classic forgery — the same
		// header with an EMPTY signature — is covered separately below, because it
		// never reaches this lane at all.
		{name: "alg none", token: "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.AAAA"},
	} {
		status, errType, body := laneRead(t, s.bearer(http.MethodGet, path, tc.token, nil))
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, status)
		}
		if errType != "authentication_error" {
			t.Errorf("%s: error type %q, want authentication_error", tc.name, errType)
		}
		messages = append(messages, laneMessage(t, body))
	}
	for i, m := range messages {
		if m != messages[0] {
			t.Errorf("rejection %d says %q but rejection 0 says %q; the difference tells an attacker which check failed",
				i, m, messages[0])
		}
	}

	// An Authorization value without the JWT silhouette — the alg:none forgery
	// with its empty signature segment is the canonical one — is not a credential
	// this lane recognizes, so it never reaches the verifier and the management
	// arm answers with the missing-key 401 it always has. Still 401, and the
	// difference leaks nothing: it reports the shape of the string the caller
	// themselves constructed, not the outcome of any check against a real token.
	status, errType, body := laneRead(t, s.bearer(http.MethodGet, path, "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.", nil))
	if status != http.StatusUnauthorized || errType != "authentication_error" {
		t.Errorf("alg none with an empty signature: status %d, error type %q, want 401 authentication_error", status, errType)
	}
	if msg := laneMessage(t, body); !strings.Contains(msg, "x-api-key") {
		t.Errorf("message = %q, want the management arm's missing-key 401", msg)
	}

	// A token that verifies but maps to no role is authenticated with no
	// authority: 403, not 401. The distinction is the operator's whole diagnostic
	// — "your IdP is not sending the group I map" reads nothing like "your token
	// is bad" — and it is safe because the caller already proved who they are.
	status, errType = laneStatus(t, s.bearer(http.MethodGet, path, s.token("some-unrelated-group"), nil))
	if status != http.StatusForbidden || errType != "permission_error" {
		t.Errorf("unmapped role: status %d, error type %q, want 403 permission_error", status, errType)
	}
}

// TestIdentityDisabledIsUnchanged is the contract IDENTITY_MODE=disabled
// carries: a Bearer on a management path 401s exactly as it did before this
// slice existed, and nothing in the response says a human lane was considered.
func TestIdentityDisabledIsUnchanged(t *testing.T) {
	s := newTestServer(t) // built with a nil verifier

	status, body := s.do(http.MethodGet, "/v1/agents", nil)
	if status != http.StatusOK {
		t.Errorf("x-api-key with identity disabled: status %d, want 200 (%v)", status, body)
	}

	res := s.doRaw(http.MethodGet, "/v1/agents", nil,
		map[string]string{"Authorization": "Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"})
	status, errType, raw := laneRead(t, res)
	if status != http.StatusUnauthorized {
		t.Errorf("Bearer with identity disabled: status %d, want 401", status)
	}
	if errType != "authentication_error" {
		t.Errorf("error type %q, want authentication_error", errType)
	}
	if msg := laneMessage(t, raw); !strings.Contains(msg, "x-api-key") {
		t.Errorf("message = %q; the missing-key 401 must stay the one this platform always sent", msg)
	}

	// The one request shape that is NOT what it was, and the only behaviour this
	// slice changes in a default deployment: a repeated x-api-key field. The
	// refusal in requireAPIKey is unconditional, so it fires with no verifier at
	// all. Gating it on one would leave the same malformed request answered
	// differently in two deployments, and would let lane selection and
	// authentication disagree about which value is the key — so this is pinned
	// here rather than left to the enabled-mode test.
	req, err := http.NewRequest(http.MethodGet, s.url+"/v1/agents", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Add("x-api-key", "")
	req.Header.Add("x-api-key", testKey)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/agents: %v", err)
	}
	status, errType, raw = laneRead(t, res)
	if status != http.StatusUnauthorized {
		t.Errorf("repeated x-api-key with identity disabled: status %d, want 401", status)
	}
	if errType != "authentication_error" {
		t.Errorf("error type %q, want authentication_error", errType)
	}
	if msg := laneMessage(t, raw); !strings.Contains(msg, "multiple") {
		t.Errorf("message = %q, want the ambiguous-credential refusal", msg)
	}
}

// TestManagementKeyWinsOverAHumanCredential pins the dispatch order. A machine
// key present means the machine lane, full stop — so a Bearer riding alongside
// one can never vouch for it, and a caller cannot attach a second credential to
// change which lane, and so which role check, applies.
func TestManagementKeyWinsOverAHumanCredential(t *testing.T) {
	s := newLaneServer(t)

	// A viewer token, which is forbidden on this route, alongside the management
	// key: the key lane serves it, with no role check at all.
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{
		"x-api-key":     testKey,
		"Authorization": "Bearer " + s.token("platform-read"),
	})
	if status, errType := laneStatus(t, res); status != http.StatusOK {
		t.Errorf("x-api-key beside a Bearer: status %d, error %q, want 200", status, errType)
	}

	// And no principal was provisioned: the human credential was never read.
	if n := countPrincipals(t, s); n != 0 {
		t.Errorf("principals rows = %d; a credential that never authenticated must not provision one", n)
	}

	// The converse is not a fallback: a BAD management key stays on the machine
	// lane and 401s rather than being retried as the human beside it. Falling
	// back would make every role check optional for anyone holding a token.
	res = s.doRaw(http.MethodGet, consoleTokens(s.env()), nil, map[string]string{
		"x-api-key":     "map-wrong-key",
		"Authorization": "Bearer " + s.token("platform-admins"),
	})
	if status, errType := laneStatus(t, res); status != http.StatusUnauthorized || errType != "authentication_error" {
		t.Errorf("bad x-api-key beside a valid admin token: status %d, error %q, want 401 authentication_error", status, errType)
	}
}

// TestARepeatedAPIKeyHeaderCannotChangeLanes pins the machine-first rule against
// a field HTTP allows to repeat. Header.Get reads only the first value, so an
// empty x-api-key placed ahead of a real one would read as "no key offered" and
// move the request onto the human lane — the exact lane change the rule exists
// to prevent. Every ordering must stay on the machine lane and 401 there.
func TestARepeatedAPIKeyHeaderCannotChangeLanes(t *testing.T) {
	s := newLaneServer(t)
	path := consoleTokens(s.env())
	admin := s.token("platform-admins")

	// doRaw's map cannot express a repeated field, so the request is built here.
	send := func(t *testing.T, values []string, authorization string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, s.url+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		for _, v := range values {
			req.Header.Add("x-api-key", v)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return laneStatus(t, res)
	}

	for _, tc := range []struct {
		name   string
		values []string
	}{
		{name: "empty then wrong", values: []string{"", "map-wrong-key"}},
		{name: "empty then valid", values: []string{"", testKey}},
		{name: "wrong then empty", values: []string{"map-wrong-key", ""}},
		{name: "valid twice", values: []string{testKey, testKey}},
	} {
		status, errType := send(t, tc.values, "Bearer "+admin)
		if status != http.StatusUnauthorized {
			t.Errorf("%s beside a valid admin token: status %d, want 401 — a repeated key must not reach the human lane",
				tc.name, status)
		}
		if errType != "authentication_error" {
			t.Errorf("%s: error type %q, want authentication_error", tc.name, errType)
		}
	}

	// The single-field cases are unchanged: one valid key still serves, and no
	// x-api-key at all still leaves the human lane reachable.
	if status, _ := send(t, []string{testKey}, ""); status != http.StatusOK {
		t.Errorf("one valid x-api-key: status %d, want 200", status)
	}
	if status, _ := send(t, nil, "Bearer "+admin); status != http.StatusOK {
		t.Errorf("admin token with no x-api-key: status %d, want 200", status)
	}
}

// TestEnvironmentKeyCannotReachAConsoleRoute pins that widening the management
// arm did not widen what a worker credential can do. An environment key is not a
// human and not a management key, and the console namespace is neither its lane
// nor its business.
func TestEnvironmentKeyCannotReachAConsoleRoute(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()
	key := issueKey(t, s.pool, envID, "worker-1")

	res := s.doRaw(http.MethodGet, consoleTokens(envID), nil,
		map[string]string{"Authorization": "Bearer " + key})
	status, errType := laneStatus(t, res)
	if status != http.StatusUnauthorized {
		t.Errorf("environment key on a console route: status %d, want 401", status)
	}
	if errType != "authentication_error" {
		t.Errorf("error type %q, want authentication_error", errType)
	}
}

// TestDualAuthDiscriminatesByCredentialShape covers the branch that now carries
// two different Bearer credentials. An environment key is sk-map-env01- plus
// base64url and holds no dots, so it can never be read as a JWT; a JWT goes to
// the human lane and is role-checked there.
//
// The human token deliberately carries an UNMAPPED claim value, so the 403 that
// proves which lane served the request comes from the caller having no role
// rather than from the route's requirement. That keeps this test about credential
// shape, which is its subject, and leaves the matrix to TestRoleMatrixIsEnforced-
// OnEveryRoute — otherwise annotating these two routes would silently turn the
// lane assertion below into a tautology.
func TestDualAuthDiscriminatesByCredentialShape(t *testing.T) {
	s := newLaneServer(t)
	agent := createAgent(t, s.tserver, map[string]any{"name": "a", "model": "m"})
	envID := s.env()
	key := issueKey(t, s.pool, envID, "worker-1")

	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agent["id"], "environment_id": envID,
	})
	if status != http.StatusOK {
		t.Fatalf("create session: status %d, body %v", status, body)
	}
	path := "/v1/sessions/" + body["id"].(string)

	for _, route := range []string{path, path + "/events"} {
		// The worker's key still reaches its own session.
		res := s.doRaw(http.MethodGet, route, nil, map[string]string{"Authorization": "Bearer " + key})
		if status, errType := laneStatus(t, res); status != http.StatusOK {
			t.Errorf("environment key on %s: status %d, error %q, want 200", route, status, errType)
		}

		// A human token on the same route takes the identity lane, and is refused
		// there for having no role rather than being mistaken for a worker
		// credential — which would have served it.
		status, errType := laneStatus(t, s.bearer(http.MethodGet, route, s.token("unmapped-group"), nil))
		if status != http.StatusForbidden {
			t.Errorf("human token on %s: status %d, want 403", route, status)
		}
		if errType != "permission_error" {
			t.Errorf("human token on %s: error type %q, want permission_error", route, errType)
		}
	}
}

// TestTrustedProxyModeReadsOnlyItsHeader pins mode B's discipline: the assertion
// header is the credential, a Bearer is never one, and the machine lanes still
// resolve first.
func TestTrustedProxyModeReadsOnlyItsHeader(t *testing.T) {
	const header = "x-goog-iap-jwt-assertion"
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = header
	})
	envID := s.env()
	path := consoleTokens(envID)
	admin := s.token("platform-admins")

	// The assertion header authenticates.
	res := s.doRaw(http.MethodGet, path, nil, map[string]string{header: admin})
	if status, errType := laneStatus(t, res); status != http.StatusOK {
		t.Errorf("assertion header: status %d, error %q, want 200", status, errType)
	}

	// The same token as a Bearer does not. In this mode Bearer is a machine
	// credential's header, and reading it as a human's would accept a credential
	// the proxy never vouched for.
	if status, errType := laneStatus(t, s.bearer(http.MethodGet, path, admin, nil)); status != http.StatusUnauthorized {
		t.Errorf("Bearer in trusted_proxy mode: status %d, error %q, want 401", status, errType)
	}

	// A management key beside the assertion still wins.
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{
		"x-api-key": testKey,
		header:      s.token("platform-read"),
	})
	if status, _ := laneStatus(t, res); status != http.StatusOK {
		t.Errorf("x-api-key beside an assertion: status %d, want 200", status)
	}

	// And an environment key on the dual-auth session read keeps its lane: the
	// assertion is a different header, so nothing about mode B disturbs a worker.
	key := issueKey(t, s.pool, envID, "worker-1")
	agent := createAgent(t, s.tserver, map[string]any{"name": "a", "model": "m"})
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agent["id"], "environment_id": envID,
	})
	if status != http.StatusOK {
		t.Fatalf("create session: status %d, body %v", status, body)
	}
	res = s.doRaw(http.MethodGet, "/v1/sessions/"+body["id"].(string), nil,
		map[string]string{"Authorization": "Bearer " + key})
	if status, errType := laneStatus(t, res); status != http.StatusOK {
		t.Errorf("environment key in trusted_proxy mode: status %d, error %q, want 200", status, errType)
	}
}
