package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

const setupCodeURL = "/api/console/organizations/default/setup_code/redeem"

func mintSetupCode(t *testing.T, pool *pgxpool.Pool, now time.Time) string {
	t.Helper()
	code, err := api.MintSetupCode(context.Background(), pool, now, false)
	if err != nil || code == "" {
		t.Fatalf("MintSetupCode = %q, %v; want a code", code, err)
	}
	return code
}

func setupCounts(t *testing.T, pool *pgxpool.Pool, codes, flags, admins int) {
	t.Helper()
	var c, f, a int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM setup_codes), (SELECT count(*) FROM install_bootstrap),
		(SELECT count(*) FROM principals WHERE role = 'admin')`).Scan(&c, &f, &a); err != nil {
		t.Fatal(err)
	}
	if c != codes || f != flags || a != admins {
		t.Fatalf("codes/flags/admins = %d/%d/%d, want %d/%d/%d", c, f, a, codes, flags, admins)
	}
}

func TestMintSetupCodeOnlyBeforeTheFirstAdmin(t *testing.T) {
	pool := pgtest.NewPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := mintSetupCode(t, pool, now)
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(code)
	if err != nil || len(decoded) != 20 {
		t.Fatalf("code is not 20 bytes of base32 without padding: %q, %v", code, err)
	}
	var hash string
	var expires time.Time
	if err := pool.QueryRow(context.Background(), `SELECT code_hash, expires_at FROM setup_codes`).Scan(&hash, &expires); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(code))
	if hash != hex.EncodeToString(sum[:]) || !expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("stored hash/expiry = %q/%v; want SHA256/now+1h", hash, expires)
	}
	other := mintSetupCode(t, pool, now)
	if other == code {
		t.Fatal("replicas minted the same code")
	}
	setupCounts(t, pool, 2, 0, 0)
}

func TestNoCodeOnceAnAdminHasExisted(t *testing.T) {
	for _, grant := range []string{"insert", "promotion"} {
		t.Run(grant, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			role := "user"
			if grant == "insert" {
				role = "admin"
			}
			if _, err := pool.Exec(context.Background(), `INSERT INTO principals (id, issuer, subject, role)
				VALUES ('principal_first', 'https://idp.example', 'first', $1)`, role); err != nil {
				t.Fatal(err)
			}
			if grant == "promotion" {
				if _, err := pool.Exec(context.Background(), `UPDATE principals SET role = 'admin'`); err != nil {
					t.Fatal(err)
				}
			}
			for _, state := range []string{"active admin", "demoted and deactivated"} {
				if state == "demoted and deactivated" {
					if _, err := pool.Exec(context.Background(), `UPDATE principals SET role = 'user', status = 'deactivated'`); err != nil {
						t.Fatal(err)
					}
				}
				code, err := api.MintSetupCode(context.Background(), pool, time.Now(), false)
				if err != nil || code != "" {
					t.Fatalf("%s: MintSetupCode = %q, %v; want no code", state, code, err)
				}
			}
			setupCounts(t, pool, 0, 1, 0)
		})
	}
}

func TestNoCodeWhenTheMapCanGrantAdmin(t *testing.T) {
	pool := pgtest.NewPool(t)
	code, err := api.MintSetupCode(context.Background(), pool, time.Now(), true)
	if err != nil || code != "" {
		t.Fatalf("MintSetupCode(mapGrantsAdmin) = %q, %v; want no code", code, err)
	}
	setupCounts(t, pool, 0, 0, 0)
}

func setupResponse(t *testing.T, res *http.Response, want int, message string) string {
	t.Helper()
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	status, typ, body := laneRead(t, res)
	if status != want {
		t.Fatalf("status = %d, want %d: %s", status, want, body)
	}
	if want >= 400 {
		wantType := "invalid_request_error"
		if want == 403 {
			wantType = "permission_error"
		} else if want == 401 {
			wantType = "authentication_error"
		}
		if typ != wantType || (message != "" && laneMessage(t, body) != message) {
			t.Fatalf("error = %s, want %s / %q", body, wantType, message)
		}
	}
	return body
}

func TestARedeemedCodeMakesTheCallerAdmin(t *testing.T) {
	s := newLaneServer(t)
	token := s.token()
	code := mintSetupCode(t, s.pool, time.Now())
	mintSetupCode(t, s.pool, time.Now())
	if status, typ := laneStatus(t, s.bearer("GET", "/v1/agents", token, nil)); status != 403 || typ != "permission_error" {
		t.Fatalf("before bootstrap read = %d/%s, want 403", status, typ)
	}
	body := setupResponse(t, s.bearer("POST", setupCodeURL, token, map[string]any{"code": code}), 200, "")
	var got struct{ ID, Role string }
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.ID == "" || got.Role != "admin" {
		t.Fatalf("redemption response = %s, %v", body, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode redemption response fields: %v", err)
	}
	if len(fields) != 2 || fields["id"] == nil || fields["role"] == nil {
		t.Error("redemption response must contain exactly id and role")
	}
	if strings.Contains(body, code) {
		t.Error("redemption response leaked the submitted setup code")
	}
	var role, source string
	if err := s.pool.QueryRow(context.Background(), `SELECT role, role_source FROM principals WHERE id = $1`, got.ID).Scan(&role, &source); err != nil || role != "admin" || source != "console" {
		t.Fatalf("promoted row = %q/%q, %v", role, source, err)
	}
	setupCounts(t, s.pool, 0, 1, 1)
	for _, replay := range []any{map[string]any{"code": code}, map[string]any{"code": "wrong"}, map[string]any{}, `{"code":`} {
		setupResponse(t, s.bearer("POST", setupCodeURL, token, replay), 409, "an admin already exists")
	}
	if status, typ := laneStatus(t, s.bearer("GET", "/v1/agents", token, nil)); status != 200 {
		t.Fatalf("after bootstrap read = %d/%s, want 200", status, typ)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE principals SET role = 'user', status = 'deactivated' WHERE id = $1`, got.ID); err != nil {
		t.Fatal(err)
	}
	claims := s.claims()
	claims["sub"] = "another-person"
	setupResponse(t, s.bearer("POST", setupCodeURL, s.idp.Mint(t, claims), map[string]any{"code": code}), 409, "an admin already exists")
	setupCounts(t, s.pool, 0, 1, 0)
}

func TestAnExpiredOrMalformedSetupCodeIsRefused(t *testing.T) {
	s := newLaneServer(t)
	token := s.token()
	valid := mintSetupCode(t, s.pool, time.Now())
	expired := mintSetupCode(t, s.pool, time.Now().Add(-2*time.Hour))
	for _, body := range []any{
		map[string]any{"code": expired}, map[string]any{"code": strings.Repeat("A", 32)},
		map[string]any{"code": "malformed"}, map[string]any{"code": ""},
		map[string]any{"code": 7}, map[string]any{"code": nil}, map[string]any{}, `{"code":`,
	} {
		setupResponse(t, s.bearer("POST", setupCodeURL, token, body), 400, "")
		setupCounts(t, s.pool, 2, 0, 0)
	}
	setupResponse(t, s.bearer("POST", setupCodeURL, token, map[string]any{"code": valid}), 200, "")
}

func TestTheManagementKeyCannotRedeemASetupCode(t *testing.T) {
	s := newLaneServer(t)
	code := mintSetupCode(t, s.pool, time.Now())
	// A real person already exists, so a mistaken machine promotion has a target.
	laneStatus(t, s.bearer("GET", "/v1/agents", s.token(), nil))
	setupResponse(t, s.doRaw("POST", setupCodeURL, map[string]any{"code": code}, map[string]string{"x-api-key": testKey}), 403,
		"only a signed-in person can redeem a setup code")
	setupCounts(t, s.pool, 1, 0, 0)
}

func TestAnIdPMappedPersonCannotRedeem(t *testing.T) {
	for _, claim := range []string{"platform-devs", "platform-read"} {
		t.Run(claim, func(t *testing.T) {
			s := newLaneServer(t)
			code := mintSetupCode(t, s.pool, time.Now())
			setupResponse(t, s.bearer("POST", setupCodeURL, s.token(claim), map[string]any{"code": code}), 409,
				"your role comes from the identity provider's mapping")
			setupCounts(t, s.pool, 1, 0, 0)
		})
	}
}

func TestSetupCodeOrganizationAndMethod(t *testing.T) {
	s := newLaneServerWith(t, func(c *identity.Config) { c.RoleMap = nil })
	token := s.token()
	code := mintSetupCode(t, s.pool, time.Now())
	for _, org := range []struct {
		value  string
		status int
	}{{"not-an-org", 400}, {"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", 401}} {
		setupResponse(t, s.bearer("POST", strings.Replace(setupCodeURL, "default", org.value, 1), token, map[string]any{"code": code}), org.status, "")
	}
	setupResponse(t, s.bearer("GET", setupCodeURL, token, nil), 405, "Method Not Allowed")
	setupCounts(t, s.pool, 1, 0, 0)
}

func TestConcurrentRedemptionsMakeOneAdmin(t *testing.T) {
	s := newLaneServer(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	type response struct {
		res *http.Response
		err error
	}
	responses := make(chan response, 2)
	for _, subject := range []string{"first", "second"} {
		claims := s.claims()
		claims["sub"] = subject
		token := s.idp.Mint(t, claims)
		code := mintSetupCode(t, s.pool, time.Now())
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		t.Cleanup(cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.roundTrip(ctx, "POST", setupCodeURL, map[string]any{"code": code}, map[string]string{"Authorization": "Bearer " + token})
			responses <- response{res, err}
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	counts := map[int]int{}
	for result := range responses {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if got := result.res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		status, _, body := laneRead(t, result.res)
		counts[status]++
		if status == 409 && laneMessage(t, body) != "an admin already exists" {
			t.Errorf("losing redemption = %s", body)
		}
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("redemption statuses = %v, want one 200 and one 409", counts)
	}
	setupCounts(t, s.pool, 0, 1, 1)
}
