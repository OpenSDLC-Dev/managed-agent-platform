package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

type managementReply struct {
	status int
	header http.Header
	raw    []byte
	err    error
}

func managementAsync(ctx context.Context, s *nativeServer, method, path, body string, h http.Header) <-chan managementReply {
	done := make(chan managementReply, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, method, s.url+path, strings.NewReader(body))
		if err != nil {
			done <- managementReply{err: err}
			return
		}
		req.Header = h.Clone()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- managementReply{err: err}
			return
		}
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		done <- managementReply{res.StatusCode, res.Header, raw, err}
	}()
	return done
}

func lockPrincipalMutations(t *testing.T, s *nativeServer, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Observe the actual database barrier. No sleep or scheduling premise establishes ordering.
func observeManagementWait(t *testing.T, s *nativeServer, ctx context.Context, count int, done ...<-chan managementReply) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND relation='principals'::regclass AND mode='ShareRowExclusiveLock' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		for _, ch := range done {
			select {
			case early := <-ch:
				t.Fatalf("mutation finished before observed lock wait: %+v body=%s", early, early.raw)
			default:
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("did not observe principal mutation lock wait")
		case <-tick.C:
		}
	}
}

// Missing serialization, counting an admin SA, or authorizing from the pre-lock cookie fails this.
func TestMembersLastHumanAdminRace(t *testing.T) {
	for _, actions := range [][2]string{{"demote", "demote"}, {"remove", "remove"}, {"demote", "remove"}} {
		t.Run(strings.Join(actions[:], "-"), func(t *testing.T) {
			s := newNativeServer(t, login.ModeOIDC)
			pgtest.DefaultRepeatableRead(t, s.pool)
			a := seedMember(t, s, "admin-a", "admin", "console")
			b := seedMember(t, s, "admin-b", "admin", "console")
			sa := domain.NewID(domain.PrefixPrincipal).String()
			if _, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,kind,role) VALUES ($1,'urn:managed-agent-platform:service-account','sa-admin','service_account','admin')`, sa); err != nil {
				t.Fatal(err)
			}
			ca, saSession := s.issue(a)
			cb, sbSession := s.issue(b)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			blocker := lockPrincipalMutations(t, s, ctx)
			request := func(action, target string, h http.Header) <-chan managementReply {
				method, body := "POST", `{"role":"developer"}`
				if action == "remove" {
					method, body = "DELETE", ""
				}
				return managementAsync(ctx, s, method, membersPath+"/"+principal.PublicID(target, "user"), body, h)
			}
			doneA := request(actions[0], b, nativeHeaders(ca, saSession.CSRFToken))
			doneB := request(actions[1], a, nativeHeaders(cb, sbSession.CSRFToken))
			observeManagementWait(t, s, ctx, 2, doneA, doneB)
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			ra, rb := <-doneA, <-doneB
			if ra.err != nil || rb.err != nil {
				t.Fatalf("request errors: %v %v", ra.err, rb.err)
			}
			if !((ra.status == 200 && rb.status == 403) || (ra.status == 403 && rb.status == 200)) {
				t.Fatalf("mutual changes=%d %s; %d %s", ra.status, ra.raw, rb.status, rb.raw)
			}
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE kind='user' AND status='active' AND role='admin'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("active human admins=%d err=%v", count, err)
			}
			// The surviving person cannot demote or remove themselves, even with an admin SA.
			currentID, currentCookie, currentSession := a, ca, saSession
			if ra.status != 200 {
				currentID, currentCookie, currentSession = b, cb, sbSession
			}
			h := nativeHeaders(currentCookie, currentSession.CSRFToken)
			path := membersPath + "/" + principal.PublicID(currentID, "user")
			expectManagement(t, s, "POST", path, `{"role":"user"}`, h, 409)
			expectManagement(t, s, "DELETE", path, "", h, 409)
		})
	}
}

// Every mutating route must recheck role, status, revocation and expiry AFTER waiting.
func TestMembersMutationCallerRecheck(t *testing.T) {
	for _, operation := range []string{"member-update", "member-remove", "member-create", "sa-create", "sa-update", "sa-archive"} {
		for _, withdrawal := range []string{"demote", "deactivate", "revoke", "expire"} {
			t.Run(operation+"/"+withdrawal, func(t *testing.T) {
				s := newNativeServer(t, login.ModePassword)
				caller := seedMember(t, s, "caller", "admin", "console")
				target := seedMember(t, s, "target", "developer", "console")
				cookie, session := s.issue(caller)
				h := nativeHeaders(cookie, session.CSRFToken)
				method, path, body := "POST", membersPath+"/"+principal.PublicID(target, "user"), `{"role":"user"}`
				var saID string
				if strings.HasPrefix(operation, "sa-") && operation != "sa-create" {
					saID = domain.NewID(domain.PrefixPrincipal).String()
					if _, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,kind,role) VALUES ($1,'urn:managed-agent-platform:service-account','existing','service_account','developer')`, saID); err != nil {
						t.Fatal(err)
					}
				}
				switch operation {
				case "member-remove":
					method, body = "DELETE", ""
				case "member-create":
					path, body = membersPath, fmt.Sprintf(`{"login_name":"new-person","password":%q,"role":"user"}`, localPassword)
				case "sa-create":
					path, body = serviceAccountsPath, `{"name":"new-service"}`
				case "sa-update":
					path, body = serviceAccountsPath+"/"+principal.PublicID(saID, "service_account"), `{"organization_role":"admin"}`
				case "sa-archive":
					path, body = serviceAccountsPath+"/"+principal.PublicID(saID, "service_account")+"/archive", ""
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				blocker := lockPrincipalMutations(t, s, ctx)
				done := managementAsync(ctx, s, method, path, body, h)
				observeManagementWait(t, s, ctx, 1, done)
				query := map[string]string{
					"demote":     `UPDATE principals SET role='developer' WHERE id=$1`,
					"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
					"revoke":     `DELETE FROM browser_sessions WHERE principal_id=$1`,
					"expire":     `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1`,
				}[withdrawal]
				if _, err := blocker.Exec(ctx, query, caller); err != nil {
					t.Fatal(err)
				}
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				reply := <-done
				want, marker := 403, ""
				if withdrawal == "deactivate" {
					marker = "1"
				}
				if withdrawal == "revoke" || withdrawal == "expire" {
					want, marker = 401, "1"
				}
				if reply.err != nil || reply.status != want || reply.header.Get("x-platform-signed-out") != marker {
					t.Fatalf("waiting caller=%+v body=%s want=%d marker=%q", reply, reply.raw, want, marker)
				}
				var changed int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE (id=$1 AND (role<>'developer' OR status<>'active')) OR subject IN ('new-person','new-service') OR (id=$2 AND (role<>'developer' OR status<>'active'))`, target, saID).Scan(&changed); err != nil || changed != 0 {
					t.Fatalf("withdrawn caller changed %d targets: %v", changed, err)
				}
			})
		}
	}
}

// The guard belongs to manual mutations, never to the IdP mapping resolver.
func TestMembersMappingMayDemoteLastAdmin(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	id := seedMember(t, s, "mapped", "admin", "idp")
	p, err := principal.Resolve(context.Background(), s.pool, identity.Identity{Issuer: "https://idp.example", Subject: "mapped"})
	if err != nil || p.ID != id || p.Role != identity.RoleUser {
		t.Fatalf("mapping exception=%+v %v", p, err)
	}
}

// A tuple lock alone does not make PostgreSQL re-evaluate an expiry predicate
// checked before FOR SHARE waited. Observe that actual wait and database expiry;
// no tuple UPDATE or principal-table barrier establishes the ordering here.
func TestMembersSessionRowWaitRechecksExpiry(t *testing.T) {
	for _, expire := range []bool{true, false} {
		name := "expired"
		if !expire {
			name = "valid-control"
		}
		t.Run(name, func(t *testing.T) {
			s := newNativeServer(t, login.ModePassword)
			admin := seedMember(t, s, "row-wait-admin", "admin", "console")
			cookie, session := s.issue(admin)
			h := nativeHeaders(cookie, session.CSRFToken)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if expire {
				// Shorten the ordinary issued session before the blocking transaction.
				if _, err := s.pool.Exec(ctx, `UPDATE browser_sessions SET expires_at=clock_timestamp()+interval '4 seconds' WHERE principal_id=$1`, admin); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var blockerPID int
			// The blocker must not change the tuple: an UPDATE could trigger a
			// fresh predicate check and hide the stale-clock authorization defect.
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM browser_sessions WHERE principal_id=$1 FOR UPDATE`, admin).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			done := managementAsync(ctx, s, "POST", serviceAccountsPath, `{"name":"row-wait-created"}`, h)
			waitFor := func(observation, query string, args ...any) {
				t.Helper()
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for {
					var observed bool
					if err := s.pool.QueryRow(ctx, query, args...).Scan(&observed); err != nil {
						t.Fatal(err)
					}
					if observed {
						t.Log(observation)
						return
					}
					select {
					case early := <-done:
						t.Fatalf("request finished before %s: status=%d err=%v body=%s", observation, early.status, early.err, early.raw)
					case <-ctx.Done():
						t.Fatalf("did not observe %s", observation)
					case <-tick.C:
					}
				}
			}
			waitFor("FOR SHARE OF s blocked by the actual session-row holder", `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
				AND wait_event_type='Lock' AND position('FOR SHARE OF s' in query)>0
				AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID)
			if expire {
				waitFor("database-clock expiry while the session row remains locked", `SELECT expires_at<clock_timestamp() FROM browser_sessions WHERE principal_id=$1`, admin)
			} else {
				var valid bool
				if err := s.pool.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM browser_sessions WHERE principal_id=$1`, admin).Scan(&valid); err != nil || !valid {
					t.Fatalf("valid-session control expired before release: valid=%v err=%v", valid, err)
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			reply := <-done
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			var targetRows, principalRows, keyRows, credentialRows int
			var role, status string
			if err := s.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM principals WHERE subject='row-wait-created'),
				(SELECT count(*) FROM principals), (SELECT count(*) FROM api_keys),
				(SELECT count(*) FROM local_accounts), role, status FROM principals WHERE id=$1`, admin).
				Scan(&targetRows, &principalRows, &keyRows, &credentialRows, &role, &status); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantRows, wantMarker := 200, 1, ""
			if expire {
				wantStatus, wantRows, wantMarker = 401, 0, "1"
			}
			if reply.status != wantStatus || reply.header.Get("x-platform-signed-out") != wantMarker || targetRows != wantRows || principalRows != 1+wantRows || keyRows != 1 || credentialRows != 0 || role != "admin" || status != "active" {
				t.Fatalf("post-row-wait status=%d marker=%q target_rows=%d principal_rows=%d key_rows=%d credential_rows=%d caller=%s/%s body=%s; want status=%d marker=%q target_rows=%d",
					reply.status, reply.header.Get("x-platform-signed-out"), targetRows, principalRows, keyRows, credentialRows, role, status, reply.raw, wantStatus, wantMarker, wantRows)
			}
			expectManagement(t, s, "GET", "/api/auth/session", "", h, wantStatus)
		})
	}
}
