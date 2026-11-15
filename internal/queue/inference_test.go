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
			allowed := actor == "apikey_archived" || actor == "principal_deleted_before_retention"
			if (item.InferenceToken != "") != allowed {
				t.Fatalf("token admission for %q = %t", actor, item.InferenceToken != "")
			}
			if allowed {
				p, err := inferencetoken.Authenticate(ctx, pool, item.InferenceToken)
				if err != nil || p.APIKeyID != actor || p.PrincipalID != "" {
					t.Fatalf("legacy authority: %+v %v", p, err)
				}
			}
			if err := q.Complete(ctx, pool, item); err != nil {
				t.Fatalf("refusal must retain the normal lease: %v", err)
			}
		})
	}
}

func TestModelClaimDoesNotLockItsSession(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.NewPool(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by='legacy' WHERE id=$1`, sid); err != nil {
		t.Fatal(err)
	}
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
	if _, err := pool.Exec(ctx, `UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by='legacy';ALTER TABLE inference_tokens ADD CONSTRAINT reject_mint CHECK(false) NOT VALID`); err != nil {
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
	if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_initiator','test','initiator','user');
		UPDATE sessions SET initiator_principal_id=NULL,initiator_api_key_id=NULL,created_by='principal_initiator'`); err != nil {
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
	if err != nil || p.PrincipalID != "principal_initiator" || p.SessionID != string(sid) || p.APIKeyID != "" {
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
