package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

func TestDeploymentInitiatorCookieAdminAndFutureSchedules(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	headers := nativeHeaders(cookie, session.CSRFToken)
	s.key, _ = issueOwnedHTTP(t, s, headers, "fixture resources", nil)
	agentID, envID := fixture(t, s.tserver)
	body, _ := json.Marshal(scheduledBody(agentID, envID, "* * * * *", "UTC"))
	var deployment map[string]any
	json.Unmarshal(expectManagement(t, s, "POST", "/api/organizations/default/proxy/v1/deployments", string(body), headers, 200), &deployment)
	id := deployment["id"].(string)
	path := "/api/console/organizations/default/deployments/" + id + "/initiator"
	var initiator map[string]any
	json.Unmarshal(expectManagement(t, s, "GET", path, "", headers, 200), &initiator)
	if len(initiator) != 2 || initiator["principal_id"] != principal.PublicID(admin, "user") || initiator["api_key_id"] != nil {
		t.Fatalf("initiator=%v", initiator)
	}
	var account map[string]any
	json.Unmarshal(expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"scheduler","default_workspace_role":"developer"}`, headers, 200), &account)
	accountID := account["id"].(string)
	change := fmt.Sprintf(`{"principal_id":%q}`, accountID)
	for _, malformed := range []string{"%00", "depl_%00", "not-an-id"} {
		for _, method := range []string{"GET", "POST"} {
			expectManagement(t, s, method, "/api/console/organizations/default/deployments/"+malformed+"/initiator", change, headers, 404)
		}
	}
	for _, role := range []string{"user", "developer"} {
		member := seedMember(t, s, role, role, "console")
		c, se := s.issue(member)
		for _, method := range []string{"GET", "POST"} {
			expectManagement(t, s, method, path, change, nativeHeaders(c, se.CSRFToken), 403)
		}
	}
	for _, method := range []string{"GET", "POST", "PUT"} {
		expectManagement(t, s, method, path, change, http.Header{"X-Api-Key": {testKey}, "Content-Type": {"application/json"}}, 401)
	}
	expectManagement(t, s, "POST", path, change, nativeHeaders(cookie, ""), 403)
	for _, bad := range []string{`{}`, `null`, `{"principal_id":null}`, `{"principal_id":"user_no"}`, `{"principal_id":"principal_no"}`, `{"Principal_id":"svac_no"}`, `{"principal_id":"svac_no","extra":true}`} {
		expectManagement(t, s, "POST", path, bad, headers, 400)
	}
	expectManagement(t, s, "POST", path, `{"principal_id":"svac_unknown"}`, headers, 404)
	// A scheduled session freezes its initiator before the setting changes.
	now := time.Date(2026, 11, 15, 10, 1, 30, 0, time.UTC)
	setResumedAt(t, s.tserver, id, now.Add(-time.Minute))
	if err := api.SchedulerTick(t.Context(), s.pool, now); err != nil {
		t.Fatal(err)
	}
	runs := scheduledRuns(t, s.tserver, id)
	if len(runs) != 1 || runs[0].sessionID == nil {
		t.Fatalf("initial run: %+v", runs)
	}
	expectManagement(t, s, "POST", path, change, headers, 200)
	if err := api.SchedulerTick(t.Context(), s.pool, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runs = scheduledRuns(t, s.tserver, id)
	if len(runs) != 2 || runs[1].sessionID == nil {
		t.Fatalf("future run: %+v", runs)
	}
	var old, next string
	if err := s.pool.QueryRow(t.Context(), `SELECT initiator_principal_id FROM sessions WHERE id=$1`, *runs[0].sessionID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT initiator_principal_id FROM sessions WHERE id=$1`, *runs[1].sessionID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if old != admin || principal.PublicID(next, "service_account") != accountID {
		t.Fatalf("initiators old=%q next=%q", old, next)
	}
	// Manual runs retain the caller even after a deployment is repointed.
	var run map[string]any
	json.Unmarshal(expectManagement(t, s, "POST", "/api/organizations/default/proxy/v1/deployments/"+id+"/run", `{}`, headers, 200), &run)
	var manual string
	if err := s.pool.QueryRow(t.Context(), `SELECT initiator_principal_id FROM sessions WHERE id=$1`, run["session_id"]).Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if manual != admin {
		t.Fatalf("manual initiator=%s", manual)
	}
	accountPath := serviceAccountsPath + "/" + accountID
	expectManagement(t, s, "POST", accountPath, `{"default_workspace_role":null}`, headers, 200)
	expectManagement(t, s, "POST", accountPath, `{"default_workspace_role":"developer"}`, headers, 200)
	expectManagement(t, s, "POST", path, change, headers, 200)
	var canResume bool
	if err := s.pool.QueryRow(t.Context(), `SELECT workspace_can_continue(initiator_principal_id,workspace_id,workspace_grant_generation)
		FROM deployments WHERE id=$1`, id).Scan(&canResume); err != nil || canResume {
		t.Fatalf("repeating the same initiator assignment revived a revoked deployment: allowed=%t err=%v", canResume, err)
	}
	expectManagement(t, s, "POST", serviceAccountsPath+"/"+accountID+"/archive", `{}`, headers, 200)
	expectManagement(t, s, "POST", path, change, headers, 400)
}

func TestScheduledInitiatorMembership(t *testing.T) {
	for _, mode := range []string{"demoted", "removed", "missing", "legacy_archived"} {
		t.Run(mode, func(t *testing.T) {
			s := newNativeServer(t, login.ModeOIDC)
			member := seedMember(t, s, "creator", "developer", "console")
			s.key, _ = issueOwnedHTTP(t, s, ownedCookie(t, s, member), "fixture resources", nil)
			agentID, envID := fixture(t, s.tserver)
			id := createDeployment(t, s.tserver, scheduledBody(agentID, envID, "* * * * *", "UTC"))["id"].(string)
			if _, err := s.pool.Exec(t.Context(), `UPDATE deployments SET initiator_principal_id=$1, initiator_api_key_id=NULL WHERE id=$2`, member, id); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "demoted":
				_, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='user' WHERE id=$1`, member)
				if err != nil {
					t.Fatal(err)
				}
			case "removed":
				_, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, member)
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				_, err := s.pool.Exec(t.Context(), `UPDATE deployments SET initiator_principal_id=NULL, created_by=NULL WHERE id=$1`, id)
				if err != nil {
					t.Fatal(err)
				}
			case "legacy_archived":
				_, err := s.pool.Exec(t.Context(), `UPDATE deployments SET initiator_principal_id=NULL, initiator_api_key_id='gone_key' WHERE id=$1`, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			now := time.Date(2026, 11, 15, 10, 1, 30, 0, time.UTC)
			setResumedAt(t, s.tserver, id, now.Add(-time.Minute))
			if err := api.SchedulerTick(t.Context(), s.pool, now); err != nil {
				t.Fatal(err)
			}
			runs := scheduledRuns(t, s.tserver, id)
			if len(runs) != 1 {
				t.Fatalf("runs=%+v", runs)
			}
			failed := mode != "demoted"
			if failed {
				if runs[0].sessionID != nil || runs[0].errType == nil || *runs[0].errType != "unknown_error" {
					t.Fatalf("failed run=%+v", runs[0])
				}
			} else if runs[0].sessionID == nil || runs[0].errType != nil {
				t.Fatalf("success run=%+v", runs[0])
			}
			var paused bool
			if err := s.pool.QueryRow(t.Context(), `SELECT paused_at IS NOT NULL FROM deployments WHERE id=$1`, id).Scan(&paused); err != nil {
				t.Fatal(err)
			}
			if paused != failed {
				t.Fatalf("paused=%v want=%v", paused, failed)
			}
		})
	}
}

func TestDeploymentInitiatorRechecksCookieAfterDeploymentLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	s, h := newCredentialHandler(t)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	headers := nativeHeaders(cookie, session.CSRFToken)
	agentID, envID := fixture(t, s.tserver)
	id := createDeployment(t, s.tserver, deploymentBody(agentID, envID))["id"].(string)
	var original string
	if err := s.pool.QueryRow(ctx, `SELECT initiator_principal_id FROM deployments WHERE id=$1`, id).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var account map[string]any
	json.Unmarshal(expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"schedule"}`, headers, 200), &account)
	blockerConn := credentialFenceConnection(t, ctx, s)
	observer := credentialFenceConnection(t, ctx, s)
	blocker, err := blockerConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM deployments WHERE id=$1 FOR UPDATE`, id).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var expiry time.Time
	if err := observer.QueryRow(ctx, `UPDATE browser_sessions SET expires_at=clock_timestamp()+interval '3 seconds' WHERE principal_id=$1 RETURNING expires_at`, admin).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	path := "/api/console/organizations/default/deployments/" + id + "/initiator"
	call := startCredentialRequest(t, ctx, h, "POST", path, io.NopCloser(strings.NewReader(fmt.Sprintf(`{"principal_id":%q}`, account["id"]))), headers)
	waitCredentialWrite(t, ctx, observer, pid, "%SELECT id FROM deployments%", call)
	for {
		var expired bool
		if err := observer.QueryRow(ctx, `SELECT clock_timestamp()>$1`, expiry).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rr := call.result(t)
	if rr.Code != 401 || rr.Header().Get("x-platform-signed-out") != "1" {
		t.Fatalf("expired write: %d %s", rr.Code, rr.Body.String())
	}
	var principalID *string
	if err := observer.QueryRow(ctx, `SELECT initiator_principal_id FROM deployments WHERE id=$1`, id).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if principalID == nil || *principalID != original {
		t.Fatal("expired cookie changed initiator")
	}
}

func TestDeploymentInitiatorBoundsItsLockWait(t *testing.T) {
	restore := api.SetDeploymentLockWaitForTest(300 * time.Millisecond)
	defer restore()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	s, h := newCredentialHandler(t)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	headers := nativeHeaders(cookie, session.CSRFToken)
	agentID, envID := fixture(t, s.tserver)
	id := createDeployment(t, s.tserver, deploymentBody(agentID, envID))["id"].(string)
	var account map[string]any
	json.Unmarshal(expectManagement(t, s, "POST", serviceAccountsPath, `{"name":"schedule"}`, headers, 200), &account)
	blockerConn := credentialFenceConnection(t, ctx, s)
	observer := credentialFenceConnection(t, ctx, s)
	blocker, err := blockerConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM deployments WHERE id=$1 FOR SHARE`, id).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	path := "/api/console/organizations/default/deployments/" + id + "/initiator"
	call := startCredentialRequest(t, ctx, h, "POST", path, io.NopCloser(strings.NewReader(fmt.Sprintf(`{"principal_id":%q}`, account["id"]))), headers)
	waitCredentialWrite(t, ctx, observer, pid, "%SELECT id FROM deployments%", call)
	select {
	case <-call.done:
	case <-time.After(2 * time.Second):
		t.Fatal("initiator write held the principal fence past the deployment lock timeout")
	}
	if rr := call.result(t); rr.Code != 500 {
		t.Fatalf("blocked write: %d %s", rr.Code, rr.Body.String())
	}
	// The deployment remains locked, but the failed setter must release its
	// organization-wide principal fence so member removal can proceed.
	bounded, release := context.WithTimeout(ctx, time.Second)
	defer release()
	if _, err := observer.Exec(bounded, `UPDATE principals SET status='deactivated' WHERE id=$1`, admin); err != nil {
		t.Fatalf("blocked deployment still prevents principal withdrawal: %v", err)
	}
}
