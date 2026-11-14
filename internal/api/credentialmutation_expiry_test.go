package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
)

// The table barrier permits admission's reads but delays the real authorization
// LOCK. Only the target expires; the admin and browser session stay valid.
func TestNativeCredentialTargetExpiryAfterAuthorityWait(t *testing.T) {
	for _, operation := range []string{"management-issue", "reactivate-org", "reactivate-workspace", "rename-org", "rename-workspace"} {
		for _, expiry := range []string{"expires-during-wait", "no-expiry"} {
			t.Run(operation+"/"+expiry, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				s, h := newCredentialHandler(t)
				caller := seedMember(t, s, "target-expiry-admin", "admin", "console")
				cookie, session := s.issue(caller)
				headers := nativeHeaders(cookie, session.CSRFToken)
				assertTargetExpiryCookie(t, ctx, h, headers, session)
				var validCaller bool
				if err := s.pool.QueryRow(ctx, `SELECT kind='user' AND status='active' AND role='admin'
					AND EXISTS (SELECT 1 FROM browser_sessions WHERE principal_id=$1
					AND expires_at>clock_timestamp()+interval '2 minutes') FROM principals WHERE id=$1`, caller).Scan(&validCaller); err != nil || !validCaller {
					t.Fatalf("target-expiry premise: caller/session is not a durable active human admin: valid=%v err=%v", validCaller, err)
				}

				path, name, initialStatus := consoleAPIKeysPath, "target-before", "active"
				var keyID, secret string
				patch := map[string]any{"name": name}
				if operation != "management-issue" {
					secret, keyID = issueOwnedHTTP(t, s, headers, name, nil)
					path = consoleOrgAPIKey(keyID)
					if strings.HasSuffix(operation, "-workspace") {
						path = consoleAPIKey(keyID)
					}
					patch = map[string]any{"name": "target-after"}
					if strings.HasPrefix(operation, "reactivate-") {
						initialStatus, patch = "inactive", map[string]any{"status": "active"}
						if tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET status='inactive' WHERE id=$1`, keyID); err != nil || tag.RowsAffected() != 1 {
							t.Fatalf("target-expiry premise: disable target: rows=%d err=%v", tag.RowsAffected(), err)
						}
					}
					lane := 200
					if initialStatus == "inactive" {
						lane = 401
					}
					assertTargetExpiryCredential(t, ctx, h, secret, lane)
				}
				var beforeKeys, beforeWorkers int
				if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_keys),(SELECT count(*) FROM environment_keys)`).Scan(&beforeKeys, &beforeWorkers); err != nil {
					t.Fatal(err)
				}

				blockerConn := credentialFenceConnection(t, ctx, s)
				observer := credentialFenceConnection(t, ctx, s)
				blocker, err := blockerConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					// Release before the existing request cleanup joins its goroutine.
					cancel()
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cleanupCancel()
					if err := blocker.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
						t.Errorf("release target-expiry principal barrier: %v", err)
					}
				}()
				// This is an unrelated principal mutation's table lock, not a lock
				// on the credential, issuer row or browser session.
				if _, err := blocker.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
				blockerPID := int(blockerConn.PgConn().PID())
				var configuredAt, deadline time.Time
				if err := observer.QueryRow(ctx, `WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at)
					SELECT at,at+interval '10 seconds' FROM stamp`).Scan(&configuredAt, &deadline); err != nil {
					t.Fatal(err)
				}
				var expiresAt *time.Time
				if expiry == "expires-during-wait" {
					expiresAt = &deadline
					if keyID == "" {
						patch["expires_at"] = deadline.UTC().Format(time.RFC3339Nano)
					} else if tag, err := observer.Exec(ctx, `UPDATE api_keys SET expires_at=$2 WHERE id=$1`, keyID, deadline); err != nil || tag.RowsAffected() != 1 {
						t.Fatalf("target-expiry premise: configure future expiry: rows=%d err=%v", tag.RowsAffected(), err)
					}
				}
				encoded, err := json.Marshal(patch)
				if err != nil {
					t.Fatal(err)
				}
				body := io.NopCloser(strings.NewReader(string(encoded)))
				call := startCredentialRequest(t, ctx, h, "POST", path, body, headers)
				// Controls keep the same barrier and database-clock hold interval;
				// only the target's expires_at is absent.
				issuerPID, crossedAt := waitCredentialTargetExpiry(t, ctx, observer, blockerPID, call, configuredAt, deadline, keyID, expiresAt)
				if err := blocker.Commit(ctx); err != nil {
					t.Fatalf("release observed target-expiry authority wait: %v", err)
				}
				rr := call.result(t)
				if err := body.Close(); err != nil {
					t.Errorf("close target-expiry request body: %v", err)
				}
				var completedAt time.Time
				if err := observer.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&completedAt); err != nil || completedAt.Before(crossedAt) {
					t.Fatalf("target-expiry premise: completion did not follow the observed clock crossing: at=%s crossed=%s err=%v", completedAt.Format(time.RFC3339Nano), crossedAt.Format(time.RFC3339Nano), err)
				}
				t.Logf("TARGET_EXPIRY_COMPLETED issuer_pid=%d blocker_pid=%d crossed_at=%s completed_at=%s status=%d", issuerPID, blockerPID, crossedAt.Format(time.RFC3339Nano), completedAt.Format(time.RFC3339Nano), rr.Code)
				wantCode := 200
				if expiresAt != nil && operation != "management-issue" {
					wantCode = 400
				}
				if rr.Code != wantCode || rr.Header().Get("x-platform-signed-out") != "" {
					t.Errorf("target-expiry mutation: status=%d want=%d signed_out=%t", rr.Code, wantCode, rr.Header().Get("x-platform-signed-out") != "")
				}
				if wantCode == 400 {
					var obj map[string]any
					if json.Unmarshal(rr.Body.Bytes(), &obj) != nil || errMessage(obj) != "Expired API keys can only be deleted, not renamed or reactivated." {
						t.Error("expired target did not return the existing expired-key error")
					}
				}

				wantStatus, wantName := initialStatus, name
				if wantCode == 200 {
					if strings.HasPrefix(operation, "reactivate-") {
						wantStatus = "active"
					} else if strings.HasPrefix(operation, "rename-") {
						wantName = "target-after"
					}
					var resource struct {
						ID, Name, Status string
						CreatedBy        *struct{ ID, Type string } `json:"created_by"`
						ExpiresAt        *time.Time                 `json:"expires_at"`
						RawKey           string                     `json:"raw_key"`
					}
					if json.Unmarshal(rr.Body.Bytes(), &resource) != nil || resource.ID == "" {
						t.Fatal("successful target-expiry mutation returned no valid key resource")
					}
					if operation == "management-issue" {
						keyID, secret = resource.ID, resource.RawKey
						if !strings.HasPrefix(secret, api.IssuedKeyPrefix) {
							t.Fatal("target-expiry issuance returned no valid raw management credential")
						}
					} else if resource.ID != keyID || resource.RawKey != "" {
						t.Error("target-expiry update returned a different key or disclosed a raw credential")
					}
					wantRendered := wantStatus
					if expiresAt != nil {
						wantRendered = "expired"
					}
					if resource.Status != wantRendered {
						t.Errorf("post-wait returned status=%q want=%q", resource.Status, wantRendered)
					}
					if resource.Name != wantName || resource.CreatedBy == nil || resource.CreatedBy.ID != principal.PublicID(caller, "user") || resource.CreatedBy.Type != "user" {
						t.Error("target-expiry response has the wrong name or human issuer")
					}
					if (resource.ExpiresAt == nil) != (expiresAt == nil) || expiresAt != nil && !resource.ExpiresAt.Equal(*expiresAt) {
						t.Error("target-expiry response did not preserve the caller-chosen expiry")
					}
				}
				var status, storedName, issuer string
				var storedExpiry *time.Time
				var matchesHash bool
				if err := observer.QueryRow(ctx, `SELECT status,name,created_by,expires_at,owned_key_hash=$2 FROM api_keys WHERE id=$1`, keyID, apikey.Hash(secret)).Scan(&status, &storedName, &issuer, &storedExpiry, &matchesHash); err != nil {
					t.Fatalf("read durable target-expiry key metadata: %v", err)
				}
				if status != wantStatus || storedName != wantName || issuer != caller || !matchesHash {
					t.Errorf("durable target-expiry key: status=%q want=%q name_matches=%t issuer_matches=%t credential_matches=%t", status, wantStatus, storedName == wantName, issuer == caller, matchesHash)
				}
				if (storedExpiry == nil) != (expiresAt == nil) || expiresAt != nil && !storedExpiry.Equal(*expiresAt) {
					t.Error("durable target-expiry key did not preserve the configured expiry")
				}
				var afterKeys, afterWorkers int
				if err := observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_keys),(SELECT count(*) FROM environment_keys)`).Scan(&afterKeys, &afterWorkers); err != nil {
					t.Fatal(err)
				}
				wantKeys := beforeKeys
				if operation == "management-issue" {
					wantKeys++
				}
				if afterKeys != wantKeys || afterWorkers != beforeWorkers {
					t.Errorf("target-expiry durable row counts: management=%d want=%d worker=%d want=%d", afterKeys, wantKeys, afterWorkers, beforeWorkers)
				}
				// A fresh projection independently proves expiry, even when the
				// refused reactivation must leave the stored status inactive.
				keys, err := api.ListManagementKeys(ctx, s.pool)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, key := range keys {
					if key.ID == keyID {
						found = true
						want := wantStatus
						if expiresAt != nil {
							want = "expired"
						}
						if key.Status != want {
							t.Errorf("fresh target-expiry projection status=%q want=%q", key.Status, want)
						}
					}
				}
				if !found {
					t.Error("target-expiry key disappeared from the fresh projection")
				}
				lane := 200
				if expiresAt != nil {
					lane = 401
				}
				assertTargetExpiryCredential(t, ctx, h, secret, lane)
				assertTargetExpiryCookie(t, ctx, h, headers, session)
				if expiry == "expires-during-wait" && !t.Failed() {
					fmt.Printf("TARGET_EXPIRY_ASSERTIONS_PASSED %s\n", t.Name())
				}
				if expiry == "no-expiry" && !t.Failed() {
					fmt.Printf("TARGET_EXPIRY_CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
				}
			})
		}
	}
}

// Observe both sides of the deadline while the exact authorization lock remains
// blocked by our separate connection. A late first sample is a failed premise,
// not evidence that the mutation made a stale-clock decision.
func waitCredentialTargetExpiry(t *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID int, call *credentialRequest, configuredAt, deadline time.Time, keyID string, expiresAt *time.Time) (int, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var issuerPID int
	var startedAt time.Time
	for {
		var pid int
		var started, at time.Time
		var matchesExpiry bool
		err := observer.QueryRow(ctx, `SELECT a.pid,a.xact_start,clock_timestamp(),
			($3::text='' OR EXISTS (SELECT 1 FROM api_keys WHERE id=$3 AND expires_at IS NOT DISTINCT FROM $4::timestamptz))
			FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid
			WHERE a.datname=current_database() AND a.state='active' AND a.wait_event_type='Lock'
			AND a.query='LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE'
			AND l.locktype='relation' AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND l.relation='principals'::regclass AND l.mode='ShareRowExclusiveLock' AND NOT l.granted
			AND $1::integer=ANY(pg_blocking_pids(a.pid)) AND ($2::integer=0 OR a.pid=$2)`, blockerPID, issuerPID, keyID, expiresAt).Scan(&pid, &started, &at, &matchesExpiry)
		select {
		case <-call.done:
			t.Fatalf("target-expiry premise: request ended before releasing its observed principal barrier: status=%d", call.rr.Code)
		default:
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("target-expiry premise: observe actual principal lock: %v", err)
		}
		if err == nil {
			if !matchesExpiry {
				t.Fatal("target-expiry premise: stored target expiry differs from the configured deadline")
			}
			if issuerPID == 0 {
				if started.Before(configuredAt) || !started.Before(deadline) || at.Before(started) || !at.Before(deadline) {
					t.Fatalf("target-expiry premise: no unexpired pre-deadline transaction wait: issuer_pid=%d blocker_pid=%d configured_at=%s xact_start=%s observed_at=%s deadline=%s", pid, blockerPID, configuredAt.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
				}
				issuerPID, startedAt = pid, started
				t.Logf("TARGET_EXPIRY_WAIT issuer_pid=%d blocker_pid=%d xact_start=%s configured_at=%s deadline=%s observed_at=%s target_has_expiry=%t", issuerPID, blockerPID, startedAt.Format(time.RFC3339Nano), configuredAt.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), expiresAt != nil)
			} else if !started.Equal(startedAt) {
				t.Fatal("target-expiry premise: observed issuer transaction changed while blocked")
			}
			if !at.Before(deadline) {
				t.Logf("TARGET_EXPIRY_CLOCK_CROSSED issuer_pid=%d blocker_pid=%d xact_start=%s deadline=%s observed_at=%s", issuerPID, blockerPID, startedAt.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
				return issuerPID, at
			}
		} else if issuerPID != 0 {
			t.Fatal("target-expiry premise: issuer lost the exact principal-lock wait before release")
		}
		select {
		case <-call.done:
			t.Fatalf("target-expiry premise: request finished while principal barrier remained held: status=%d", call.rr.Code)
		case <-ctx.Done():
			t.Fatalf("target-expiry premise: bounded database-clock/lock observation failed: %v", ctx.Err())
		case <-tick.C:
		}
	}
}

func assertTargetExpiryCredential(t *testing.T, ctx context.Context, h http.Handler, secret string, want int) {
	t.Helper()
	rr := credentialCall(ctx, h, "GET", "/v1/agents", nil, http.Header{"X-Api-Key": {secret}})
	if rr.Code != want || rr.Header().Get("x-platform-signed-out") != "" {
		t.Errorf("fresh target-expiry management authentication: status=%d want=%d signed_out=%t", rr.Code, want, rr.Header().Get("x-platform-signed-out") != "")
	}
}

func assertTargetExpiryCookie(t *testing.T, ctx context.Context, h http.Handler, headers http.Header, original login.Session) {
	t.Helper()
	rr := credentialCall(ctx, h, "GET", "/api/auth/session", nil, headers)
	if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
		t.Fatalf("target-expiry premise: unrelated cookie authentication failed: status=%d signed_out=%t", rr.Code, rr.Header().Get("x-platform-signed-out") != "")
	}
	var session login.Session
	if json.Unmarshal(rr.Body.Bytes(), &session) != nil || !session.SignedIn || session.ID != original.ID || session.Role != "admin" || session.CSRFToken != original.CSRFToken || !session.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatal("target-expiry premise: browser session no longer represents the same live human admin")
	}
}
