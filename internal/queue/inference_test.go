package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/inferencetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestInferenceTokenEndsWithItsClaimAndMembership(t *testing.T) {
	for _, change := range []string{"requeue", "expiry", "old binary reclaim", "removal", "archive", "wrong organization"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			pool := pgtest.NewPool(t)
			sid, eid := pgtest.NewSession(t, pool, "cloud")
			if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_owner','test','owner','developer');
				UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by='principal_owner'`); err != nil {
				t.Fatal(err)
			}
			q := queue.New(pool)
			if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
				t.Fatal(err)
			}
			item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
			if err != nil || item == nil || item.InferenceToken == "" {
				t.Fatalf("claim: %v %v", item, err)
			}
			switch change {
			case "requeue":
				err = q.Requeue(ctx, pool, item)
			case "expiry", "old binary reclaim":
				_, err = pool.Exec(ctx, `UPDATE work_items SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, item.ID)
				if err == nil && change == "old binary reclaim" {
					// The old claim does not know the generation column or tokens.
					_, err = pool.Exec(ctx, `UPDATE work_items SET state='active',lease_expires_at=now()+interval '1 minute',updated_at=now() WHERE id=$1`, item.ID)
				}
			case "removal":
				_, err = pool.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE id='principal_owner'`)
			case "archive":
				_, err = pool.Exec(ctx, `UPDATE sessions SET archived_at=now() WHERE id=$1`, sid)
			case "wrong organization":
				_, err = pool.Exec(ctx, `UPDATE principals SET org_id='another' WHERE id='principal_owner'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
			if err != nil || p.WorkID != "" {
				t.Fatalf("stale authority: %+v %v", p, err)
			}
			if change == "requeue" || change == "expiry" {
				next, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
				if err != nil || next == nil || next.InferenceToken == "" || next.InferenceToken == item.InferenceToken {
					t.Fatalf("fresh claim: %v %v", next, err)
				}
				p, err = inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
				if err != nil || p.WorkID != "" {
					t.Fatalf("reclaim revived old token: %+v %v", p, err)
				}
				p, err = inferencetoken.Authenticate(ctx, pool, next.InferenceToken)
				if err != nil || p.PrincipalID != "principal_owner" {
					t.Fatalf("successor authority: %+v %v", p, err)
				}
			}
		})
	}
}

func TestModelClaimRefusesMissingAndRemovedInitiators(t *testing.T) {
	for _, actor := range []string{"", "principal_removed", "apikey_archived", "principal_deleted_before_retention"} {
		t.Run("actor="+actor, func(t *testing.T) {
			ctx := t.Context()
			pool := pgtest.NewPool(t)
			sid, eid := pgtest.NewSession(t, pool, "cloud")
			if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,status) VALUES ('principal_removed','test','removed','deactivated');
				INSERT INTO api_keys(id,name,key_hash,status) VALUES ('apikey_archived','archived','fixture-hash','archived')`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by=nullif($2,'') WHERE id=$1`, sid, actor); err != nil {
				t.Fatal(err)
			}
			q := queue.New(pool)
			if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
				t.Fatal(err)
			}
			item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
			if err != nil || item == nil {
				t.Fatalf("claim: %v %v", item, err)
			}
			if item.InferenceToken != "" {
				t.Fatalf("token admission for %q = %t", actor, item.InferenceToken != "")
			}
			if err := q.Complete(ctx, pool, item); err != nil {
				t.Fatalf("refusal must retain the normal lease: %v", err)
			}
		})
	}
}

func TestOutstandingLegacyInferenceTokenIsRefused(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil || item.InferenceToken == "" {
		t.Fatalf("claim: %v %v", item, err)
	}
	// An old binary may have minted this row before the rolling update.
	if _, err := pool.Exec(ctx, `UPDATE inference_tokens SET principal_id=NULL,api_key_id='old-unowned' WHERE work_id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	p, err := inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.WorkID != "" {
		t.Fatalf("legacy claim still carries authority: %+v %v", p, err)
	}
}

func TestModelClaimDoesNotLockItsSession(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, sid); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	item, err := q.Claim(bounded, queue.ModelTurn, time.Minute)
	if err != nil || item == nil || item.InferenceToken == "" {
		t.Fatalf("claim waited for its session: %v %v", item, err)
	}
}

func TestModelClaimRollsBackWhenMintFails(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	if _, err := pool.Exec(ctx, `ALTER TABLE inference_tokens ADD CONSTRAINT reject_mint CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	if item, err := q.Claim(ctx, queue.ModelTurn, time.Minute); err == nil || item != nil {
		t.Fatalf("failed mint committed claim: %v %v", item, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE inference_tokens DROP CONSTRAINT reject_mint`); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil || item.Reclaimed || item.InferenceToken == "" {
		t.Fatalf("retry after mint failure: %v %v", item, err)
	}
}

func TestModelClaimDelegatesMemberAuthority(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_initiator','test','initiator','admin');
		UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by='principal_initiator';
		UPDATE principals SET role='user' WHERE id='principal_initiator'`); err != nil {
		t.Fatal(err)
	}
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %v %v", item, err)
	}
	if item.InferenceToken == "" {
		t.Fatal("a user member's model turn was refused")
	}
	p, err := inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.PrincipalID != "principal_initiator" || p.SessionID != string(sid) {
		t.Fatalf("delegated authority: %+v %v", p, err)
	}
	if err := q.Extend(ctx, item, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	p, err = inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.PrincipalID == "" {
		t.Fatalf("renewal invalidated the token: %+v %v", p, err)
	}
	if err := q.Complete(ctx, pool, item); err != nil {
		t.Fatal(err)
	}
	p, err = inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.PrincipalID != "" {
		t.Fatalf("completed claim retained authority: %+v %v", p, err)
	}
}

func TestInferenceTokenDoesNotReviveWhenRenewalWaitsPastExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 3*time.Second)
	if err != nil || item == nil || item.InferenceToken == "" {
		t.Fatalf("claim: %v %v", item, err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	var pid int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM work_items WHERE id=$1 FOR UPDATE`, item.ID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 1)
	go func() { renewed <- q.Extend(ctx, item, time.Minute) }()
	// Observe the actual lock wait, rather than guessing that the goroutine
	// issued its statement before the original lease expired.
	for {
		var blocked, live bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))),
			(SELECT lease_expires_at>clock_timestamp() FROM work_items WHERE id=$2)`, pid, item.ID).Scan(&blocked, &live); err != nil {
			t.Fatal(err)
		}
		if !live {
			t.Fatal("lease expired before the renewal reached its lock barrier")
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		var live bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM work_items WHERE id=$1`, item.ID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if !live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, err := inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.WorkID != "" {
		t.Fatalf("expired token still admitted: %+v %v", p, err)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-renewed; err != nil {
		t.Fatal(err)
	}
	p, err = inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
	if err != nil || p.WorkID != "" {
		t.Fatalf("renewal revived expired authority: %+v %v", p, err)
	}
}
