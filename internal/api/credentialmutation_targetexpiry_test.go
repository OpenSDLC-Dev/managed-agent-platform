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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
)

// Sampling expiry in the locking SELECT can precede its row-lock wait, even
// with clock_timestamp(). Only the target row is held; caller authority stays live.
func TestNativeCredentialTargetExpiryAfterTargetRowWait(t *testing.T) {
	for _, expiry := range []string{"expires-during-target-wait", "no-expiry"} {
		t.Run(expiry, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			// These shared setup helpers use background contexts and remain subject
			// to the test-binary timeout, not the request/observer deadline above.
			s, h := newCredentialHandler(t)
			caller := seedMember(t, s, "target-row-expiry-admin", "admin", "console")
			cookie, session := s.issue(caller)
			headers := nativeHeaders(cookie, session.CSRFToken)
			assertTargetExpiryCookie(t, ctx, h, headers, session)
			var validCaller bool
			if err := s.pool.QueryRow(ctx, `SELECT kind='user' AND status='active' AND role='admin'
				AND EXISTS (SELECT 1 FROM browser_sessions WHERE principal_id=$1
				AND expires_at>clock_timestamp()+interval '2 minutes') FROM principals WHERE id=$1`, caller).Scan(&validCaller); err != nil || !validCaller {
				t.Fatalf("target-row expiry premise: caller/session is not a durable active human admin: valid=%v err=%v", validCaller, err)
			}
			secret, keyID := issueOwnedHTTP(t, s, headers, "target-row-before", nil)
			target := api.ManagementKey{ID: keyID}
			admitted := credentialCall(ctx, h, "GET", "/v1/agents", nil, http.Header{"X-Api-Key": {secret}})
			if admitted.Code != 200 || admitted.Header().Get("x-platform-signed-out") != "" {
				t.Fatalf("target-row expiry premise: newly issued target did not authenticate: status=%d signed_out=%t", admitted.Code, admitted.Header().Get("x-platform-signed-out") != "")
			}

			blockerConn := credentialFenceConnection(t, ctx, s)
			observer := credentialFenceConnection(t, ctx, s)
			var configuredAt, deadline time.Time
			if err := observer.QueryRow(ctx, `WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at)
				SELECT at,at+interval '10 seconds' FROM stamp`).Scan(&configuredAt, &deadline); err != nil {
				t.Fatal(err)
			}
			var expiresAt *time.Time
			if expiry == "expires-during-target-wait" {
				expiresAt = &deadline
			}
			if tag, err := observer.Exec(ctx, `UPDATE api_keys SET expires_at=$2 WHERE id=$1`, target.ID, expiresAt); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("target-row expiry premise: configure target: rows=%d err=%v", tag.RowsAffected(), err)
			}
			var beforeKeys, beforeWorkers int
			var validTarget bool
			if err := observer.QueryRow(ctx, `SELECT name='target-row-before' AND status='active'
				AND created_by=$2 AND owned_key_hash=$3 AND expires_at IS NOT DISTINCT FROM $4::timestamptz
				AND clock_timestamp()<$5::timestamptz,
				(SELECT count(*) FROM api_keys),(SELECT count(*) FROM environment_keys)
				FROM api_keys WHERE id=$1`, target.ID, caller, apikey.Hash(secret), expiresAt, deadline).Scan(&validTarget, &beforeKeys, &beforeWorkers); err != nil || !validTarget {
				t.Fatalf("target-row expiry premise: target is not issued, active and configured before the future deadline: valid=%v err=%v", validTarget, err)
			}

			blocker, err := blockerConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Release the row and cancel before the request cleanup joins its worker.
				cancel()
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				if err := blocker.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Errorf("release target-row expiry blocker: %v", err)
				}
			}()
			var blockerPID int
			// No principal or session prelock: this transaction holds only the target.
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM api_keys WHERE id=$1 FOR UPDATE`, target.ID).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			body := io.NopCloser(strings.NewReader(`{"name":"target-row-after"}`))
			defer func() {
				if err := body.Close(); err != nil {
					t.Errorf("close target-row expiry request body: %v", err)
				}
			}()
			call := startCredentialRequest(t, ctx, h, "POST", consoleAPIKey(target.ID), body, headers)
			issuerPID, crossedAt := waitCredentialTargetRowExpiry(t, ctx, observer, blockerPID, call, configuredAt, deadline, target.ID, expiresAt)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatalf("release observed target-row expiry wait: %v", err)
			}
			rr := call.result(t)
			var completedAt time.Time
			if err := observer.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&completedAt); err != nil || completedAt.Before(crossedAt) {
				t.Fatalf("target-row expiry premise: completion did not follow the clock crossing: at=%s crossed=%s err=%v", completedAt.Format(time.RFC3339Nano), crossedAt.Format(time.RFC3339Nano), err)
			}
			t.Logf("TARGET_ROW_EXPIRY_COMPLETED test=%s issuer_pid=%d blocker_pid=%d crossed_at=%s completed_at=%s status=%d", t.Name(), issuerPID, blockerPID, crossedAt.Format(time.RFC3339Nano), completedAt.Format(time.RFC3339Nano), rr.Code)

			wantCode, wantName, wantRendered, wantLane := 200, "target-row-after", "active", 200
			if expiresAt != nil {
				wantCode, wantName, wantRendered, wantLane = 400, "target-row-before", "expired", 401
			}
			var obj map[string]any
			expiredError := rr.Code == 400 && json.Unmarshal(rr.Body.Bytes(), &obj) == nil && errMessage(obj) == "Expired API keys can only be deleted, not renamed or reactivated."
			if rr.Header().Get("x-platform-signed-out") != "" || rr.Code != 200 && !expiredError {
				t.Errorf("target-row expiry premise: response is neither a successful update nor the exact expired-key refusal: status=%d signed_out=%t", rr.Code, rr.Header().Get("x-platform-signed-out") != "")
			} else if rr.Code != wantCode {
				t.Errorf("target-row expiry mutation after observed clock crossing: status=%d want=%d", rr.Code, wantCode)
			}
			if wantCode == 200 && rr.Code == 200 {
				var resource struct {
					ID, Name, Status string
					CreatedBy        *struct{ ID, Type string } `json:"created_by"`
					ExpiresAt        *time.Time                 `json:"expires_at"`
					RawKey           string                     `json:"raw_key"`
				}
				if json.Unmarshal(rr.Body.Bytes(), &resource) != nil || resource.ID != target.ID || resource.Name != wantName || resource.Status != "active" || resource.CreatedBy == nil || resource.CreatedBy.ID != principal.PublicID(caller, "user") || resource.CreatedBy.Type != "user" || resource.ExpiresAt != nil || resource.RawKey != "" {
					t.Error("target-row expiry control returned invalid renamed key metadata or disclosed a credential")
				}
			}
			// Preserve durable checks even when the response contract fails.
			var storedName, storedStatus, issuer string
			var storedExpiry *time.Time
			var matchesHash bool
			if err := observer.QueryRow(ctx, `SELECT name,status,created_by,expires_at,owned_key_hash=$2 FROM api_keys WHERE id=$1`, target.ID, apikey.Hash(secret)).Scan(&storedName, &storedStatus, &issuer, &storedExpiry, &matchesHash); err != nil {
				t.Fatalf("read durable target-row expiry metadata: %v", err)
			}
			if storedName != wantName || storedStatus != "active" || issuer != caller || !matchesHash {
				t.Errorf("durable target-row expiry metadata: name_matches=%t status=%q issuer_matches=%t credential_matches=%t", storedName == wantName, storedStatus, issuer == caller, matchesHash)
			}
			if (storedExpiry == nil) != (expiresAt == nil) || expiresAt != nil && !storedExpiry.Equal(*expiresAt) {
				t.Error("durable target-row expiry metadata did not preserve the configured expiry")
			}
			var afterKeys, afterWorkers int
			if err := observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_keys),(SELECT count(*) FROM environment_keys)`).Scan(&afterKeys, &afterWorkers); err != nil {
				t.Fatal(err)
			}
			if afterKeys != beforeKeys || afterWorkers != beforeWorkers {
				t.Errorf("target-row expiry row counts: management=%d want=%d worker=%d want=%d", afterKeys, beforeKeys, afterWorkers, beforeWorkers)
			}
			listing := credentialCall(ctx, h, "GET", consoleAPIKeysPath, nil, headers)
			var keys []struct{ ID, Name, Status string }
			if listing.Code != 200 || listing.Header().Get("x-platform-signed-out") != "" || json.Unmarshal(listing.Body.Bytes(), &keys) != nil {
				t.Errorf("target-row expiry premise: fresh cookie listing failed: status=%d signed_out=%t", listing.Code, listing.Header().Get("x-platform-signed-out") != "")
			} else {
				found := false
				for _, key := range keys {
					if key.ID == target.ID {
						found = true
						if key.Name != wantName || key.Status != wantRendered {
							t.Errorf("fresh target-row expiry listing: name_matches=%t status=%q want=%q", key.Name == wantName, key.Status, wantRendered)
						}
					}
				}
				if !found {
					t.Error("target-row expiry key disappeared from the fresh listing")
				}
			}
			assertTargetExpiryCredential(t, ctx, h, secret, wantLane)
			assertTargetExpiryCookie(t, ctx, h, headers, session)
			if err := observer.QueryRow(ctx, `SELECT kind='user' AND status='active' AND role='admin'
				AND EXISTS (SELECT 1 FROM browser_sessions WHERE principal_id=$1
				AND expires_at>clock_timestamp()) FROM principals WHERE id=$1`, caller).Scan(&validCaller); err != nil || !validCaller {
				t.Fatalf("target-row expiry premise: caller/session lost durable active human admin authority: valid=%v err=%v", validCaller, err)
			}
			if !t.Failed() {
				fmt.Printf("TARGET_ROW_EXPIRY_ASSERTIONS_PASSED %s\n", t.Name())
			}
		})
	}
}

// Match the target SELECT without prescribing its projection: a stale-clock
// expression in that projection must reach the assertions, not fail observation.
func waitCredentialTargetRowExpiry(t *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID int, call *credentialRequest, configuredAt, deadline time.Time, keyID string, expiresAt *time.Time) (int, time.Time) {
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
			EXISTS (SELECT 1 FROM api_keys WHERE id=$3 AND expires_at IS NOT DISTINCT FROM $4::timestamptz)
			FROM pg_stat_activity a
			WHERE a.datname=current_database() AND a.state='active' AND a.wait_event_type='Lock'
			AND a.query ILIKE '%FROM api_keys%FOR UPDATE%'
			AND $1::integer=ANY(pg_blocking_pids(a.pid)) AND ($2::integer=0 OR a.pid=$2)
			AND EXISTS (SELECT 1 FROM pg_locks waiting JOIN pg_locks held
				ON held.locktype=waiting.locktype AND held.transactionid=waiting.transactionid
				WHERE waiting.pid=a.pid AND waiting.locktype='transactionid'
				AND waiting.mode='ShareLock' AND NOT waiting.granted
				AND held.pid=$1 AND held.mode='ExclusiveLock' AND held.granted)`, blockerPID, issuerPID, keyID, expiresAt).Scan(&pid, &started, &at, &matchesExpiry)
		select {
		case <-call.done:
			t.Fatalf("target-row expiry premise: request ended before releasing its observed target blocker: status=%d", call.rr.Code)
		default:
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("target-row expiry premise: observe actual target transaction lock: %v", err)
		}
		if err == nil {
			if !matchesExpiry {
				t.Fatal("target-row expiry premise: stored target expiry differs from the configured deadline")
			}
			if issuerPID == 0 {
				if started.Before(configuredAt) || !started.Before(deadline) || at.Before(started) || !at.Before(deadline) {
					t.Fatalf("target-row expiry premise: no unexpired pre-deadline transaction wait: test=%s issuer_pid=%d blocker_pid=%d configured_at=%s xact_start=%s observed_at=%s deadline=%s", t.Name(), pid, blockerPID, configuredAt.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
				}
				issuerPID, startedAt = pid, started
				t.Logf("TARGET_ROW_EXPIRY_WAIT test=%s issuer_pid=%d blocker_pid=%d xact_start=%s configured_at=%s deadline=%s observed_at=%s target_has_expiry=%t", t.Name(), issuerPID, blockerPID, startedAt.Format(time.RFC3339Nano), configuredAt.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), expiresAt != nil)
			} else if !started.Equal(startedAt) {
				t.Fatal("target-row expiry premise: observed issuer transaction changed while blocked")
			}
			if !at.Before(deadline) {
				t.Logf("TARGET_ROW_EXPIRY_CLOCK_CROSSED test=%s issuer_pid=%d blocker_pid=%d xact_start=%s deadline=%s observed_at=%s", t.Name(), issuerPID, blockerPID, startedAt.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
				return issuerPID, at
			}
		} else if issuerPID != 0 {
			t.Fatal("target-row expiry premise: issuer lost the exact target transaction-lock wait before release")
		}
		select {
		case <-call.done:
			t.Fatalf("target-row expiry premise: request finished while target blocker remained held: status=%d", call.rr.Code)
		case <-ctx.Done():
			t.Fatalf("target-row expiry premise: bounded database-clock/target-lock observation failed: %v", ctx.Err())
		case <-tick.C:
		}
	}
}
