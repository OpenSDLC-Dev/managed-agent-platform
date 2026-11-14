package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// The legacy IdP mapping is applied on admission, not replayed by the fence.
// Withdrawal tests change the stored role after this real admission.
func ownedMutationCaller(t *testing.T, lane, role string) (*nativeServer, http.Handler, string, http.Header) {
	t.Helper()
	if lane == "cookie" {
		s, h := newCredentialHandler(t)
		id := seedMember(t, s, "mutation-caller", role, "console")
		return s, h, id, ownedCookie(t, s, id)
	}
	s := newNativeServer(t, login.ModeOIDC)
	cfg := identity.Config{Mode: identity.ModeOIDC, Issuer: s.idp.Issuer(), Audience: laneAudience, JWKSURL: s.idp.JWKSURL(), HTTPClient: s.idp.Client(), RoleMap: laneRoles()}
	v, err := identity.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(s.pool, nil, nil, v, api.WithLogin(s.manager, nil))
	claims := s.idp.Claims(laneAudience, time.Now())
	claim := "platform-admins"
	if role == "developer" {
		claim = "platform-devs"
	}
	claims["roles"] = []any{claim}
	headers := nativeHeaders("", "")
	headers.Set("Authorization", "Bearer "+s.idp.Mint(t, claims))
	rr := credentialCall(t.Context(), h, "GET", consoleAPIKeysPath, nil, headers)
	if rr.Code != 200 {
		t.Fatalf("legacy human admission: %d %s", rr.Code, rr.Body.String())
	}
	var id string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals WHERE subject='user-1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return s, h, id, headers
}

func TestOwnedKeyMutationFence(t *testing.T) {
	for _, lane := range []string{"cookie", "legacy-human"} {
		for _, withdrawal := range []string{"user", "deactivate", "logout", "expire", "developer-self", "developer-service", "developer-other-update"} {
			if lane != "cookie" && (withdrawal == "logout" || withdrawal == "expire" || withdrawal == "developer-service") {
				continue
			}
			t.Run(lane+"/body/"+withdrawal, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				initialRole := "developer"
				if strings.HasPrefix(withdrawal, "developer-") {
					initialRole = "admin"
				}
				s, h, caller, headers := ownedMutationCaller(t, lane, initialRole)
				path, patch := consoleAPIKeysPath, `{"name":"fenced-owned"}`
				if withdrawal == "developer-service" {
					sa := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
					patch = fmt.Sprintf(`{"name":"fenced-owned","principal_id":%q}`, principal.PublicID(sa, "service_account"))
				}
				if withdrawal == "developer-other-update" {
					owner := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
					_, id := ownedKeyFixture(t, s.pool, owner, caller)
					path, patch = consoleOrgAPIKey(id), `{"name":"stolen"}`
				}
				before := ownedSnapshot(t, s)
				body := newCredentialBody(ctx, patch)
				call := startCredentialRequest(t, ctx, h, "POST", path, body, headers)
				call.await(t, body.entered, "body after human admission")
				want, marker := 403, ""
				switch withdrawal {
				case "logout", "expire", "deactivate":
					want, marker = withdrawCredentialCaller(t, ctx, s, h, caller, withdrawal, headers)
					if lane != "cookie" {
						marker = ""
					}
				default:
					role := "user"
					if strings.HasPrefix(withdrawal, "developer-") {
						role = "developer"
					}
					if _, err := s.pool.Exec(ctx, `UPDATE principals SET role=$2 WHERE id=$1`, caller, role); err != nil {
						t.Fatal(err)
					}
					if withdrawal == "developer-self" {
						want = 200
					}
					if withdrawal == "developer-other-update" {
						want = 404
					}
				}
				body.unblock()
				rr := call.result(t)
				if rr.Code != want || rr.Header().Get("x-platform-signed-out") != marker {
					t.Errorf("current authority status=%d want=%d marker=%q want=%q body=%s", rr.Code, want, rr.Header().Get("x-platform-signed-out"), marker, rr.Body.String())
				}
				if want != 200 && before != ownedSnapshot(t, s) {
					t.Error("withdrawn authority left durable key mutation")
				}
				if want == 200 {
					var key struct {
						ID string `json:"id"`
					}
					if json.Unmarshal(rr.Body.Bytes(), &key) != nil || key.ID == "" {
						t.Fatal("self-owned issuance did not return key")
					}
					var valid bool
					if err := s.pool.QueryRow(ctx, `SELECT owner_principal_id=$2 AND created_by=$2 FROM api_keys WHERE id=$1`, key.ID, caller).Scan(&valid); err != nil || !valid {
						t.Errorf("demoted developer self ownership=%v err=%v", valid, err)
					}
				}
				if strings.HasPrefix(withdrawal, "developer-") || withdrawal == "user" {
					var role string
					if err := s.pool.QueryRow(ctx, `SELECT role FROM principals WHERE id=$1`, caller).Scan(&role); err != nil || role == "admin" {
						t.Errorf("fence replayed stale IdP mapping: role=%s err=%v", role, err)
					}
				}
			})
		}
		for _, order := range []string{"issuance-first", "removal-first", "demotion-first"} {
			t.Run(lane+"/"+order, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				s, h, caller, headers := ownedMutationCaller(t, lane, "developer")
				// Fresh pool connections inherit REPEATABLE READ. A fence with an implicit
				// Begin would retain a pre-wait snapshot and write after a committed removal.
				pgtest.DefaultRepeatableRead(t, s.pool)
				if _, err := s.pool.Exec(ctx, `CREATE FUNCTION require_owned_read_committed() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF current_setting('transaction_isolation') <> 'read committed' THEN
 RAISE EXCEPTION 'owned issuer must use READ COMMITTED'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER require_owned_read_committed BEFORE INSERT ON api_keys
 FOR EACH ROW EXECUTE FUNCTION require_owned_read_committed()`); err != nil {
					t.Fatal(err)
				}
				blockerConn, observer := credentialFenceConnection(t, ctx, s), credentialFenceConnection(t, ctx, s)
				blocker, err := blockerConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				body := newCredentialBody(ctx, `{"name":"ordered-owned"}`)
				call := startCredentialRequest(t, ctx, h, "POST", consoleAPIKeysPath, body, headers)
				call.await(t, body.entered, "admitted body")
				blockerPID := int(blockerConn.PgConn().PID())
				if order != "issuance-first" {
					query := `UPDATE principals SET status='deactivated' WHERE id=$1`
					if order == "demotion-first" {
						query = `UPDATE principals SET role='user' WHERE id=$1`
					}
					if _, err := blocker.Exec(ctx, query, caller); err != nil {
						t.Fatal(err)
					}
					body.unblock()
					waitCredentialWrite(t, ctx, observer, blockerPID, "%LOCK TABLE principals%", call)
					if err := blocker.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					rr := call.result(t)
					if rr.Code != 403 {
						t.Errorf("%s allowed issuance: %d %s", order, rr.Code, rr.Body.String())
					}
				} else {
					if _, err := blocker.Exec(ctx, `LOCK TABLE api_keys IN SHARE MODE`); err != nil {
						t.Fatal(err)
					}
					body.unblock()
					issuerPID := waitCredentialWrite(t, ctx, observer, blockerPID, "%INSERT INTO api_keys%", call)
					var principalFence bool
					if err := observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='principals'::regclass AND mode='ShareRowExclusiveLock' AND granted)`, issuerPID).Scan(&principalFence); err != nil || !principalFence {
						t.Fatalf("issuer lacks principal-first transaction lock: %v %v", principalFence, err)
					}
					withdrawal := credentialFenceConnection(t, ctx, s)
					done := make(chan struct{})
					var withdrawalErr error
					go func() {
						defer close(done)
						_, withdrawalErr = withdrawal.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE id=$1`, caller)
					}()
					defer func() { cancel(); <-done }()
					observeCredentialWithdrawalFence(t, ctx, observer, issuerPID, int(withdrawal.PgConn().PID()), "%UPDATE principals%", done)
					if err := blocker.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					rr := call.result(t)
					if rr.Code != 200 {
						t.Errorf("issuance-first failed: %d %s", rr.Code, rr.Body.String())
					}
					select {
					case <-done:
						if withdrawalErr != nil {
							t.Fatal(withdrawalErr)
						}
					case <-ctx.Done():
						t.Fatal("removal did not finish")
					}
				}
				var count, bad int
				if err := observer.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status<>'archived' OR owner_principal_id IS DISTINCT FROM $1) FROM api_keys WHERE name='ordered-owned'`, caller).Scan(&count, &bad); err != nil {
					t.Fatal(err)
				}
				want := 0
				if order == "issuance-first" {
					want = 1
				}
				if count != want || bad != 0 {
					t.Errorf("ordered durable rows=%d unarchived/wrong-owner=%d want=%d/0", count, bad, want)
				}
			})
		}
	}
}

// Body errors need current authority and visible rows, but never target locks.
// Holding a real target lock must not delay either parsing or the 400 response.
func TestOwnedKeyBadBodyDoesNotLockTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	s, h := newCredentialHandler(t)
	caller := seedMember(t, s, "body-dev", "developer", "console")
	_, key := ownedKeyFixture(t, s.pool, caller, caller)
	headers := ownedCookie(t, s, caller)
	for _, path := range []string{consoleOrgAPIKey(key), consoleAPIKey(key)} {
		t.Run(path, func(t *testing.T) {
			blockerConn := credentialFenceConnection(t, ctx, s)
			blocker, err := blockerConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(ctx)
			if _, err := blocker.Exec(ctx, `SELECT id FROM api_keys WHERE id=$1 FOR UPDATE`, key); err != nil {
				t.Fatal(err)
			}
			body := newCredentialBody(ctx, `{"unknown":1}`)
			call := startCredentialRequest(t, ctx, h, "POST", path, body, headers)
			call.await(t, body.entered, "unlocked body")
			// No principal lock while the upload is incomplete either.
			if _, err := blocker.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE NOWAIT`); err != nil {
				t.Fatalf("body upload held authority lock: %v", err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			blocker, err = blockerConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(ctx)
			if _, err := blocker.Exec(ctx, `SELECT id FROM api_keys WHERE id=$1 FOR UPDATE`, key); err != nil {
				t.Fatal(err)
			}
			body.unblock()
			rr := call.result(t)
			if rr.Code != 400 {
				t.Fatalf("visible bad body=%d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
