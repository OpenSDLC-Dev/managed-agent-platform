package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRateAccountingAcrossWindowRowWaits(t *testing.T) {
	_, fixture := newStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	cfg := fixture.Config()
	cfg.MaxConns = 20 // held rows, queued callers, independent writers, and observer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := store.New(pool)
	now := func() time.Time {
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	at := now()
	edge := at.Truncate(time.Minute).Add(time.Minute)
	if edge.Sub(at) < 15*time.Second {
		edge = edge.Add(time.Minute)
	}
	old := edge.Add(-time.Minute)
	type result struct {
		admission store.Admission
		err       error
	}
	type waiter struct {
		model, key string
		old        int64
		tx         pgx.Tx
		pid        uint32
		result     chan result
	}
	cases := []waiter{
		{model: "org-full", old: 1}, {model: "org-open"},
		{model: "completed", key: "key_completed"},
	}
	rpm := int64(1)
	for i := range cases {
		c := &cases[i]
		if _, err := s.PutModelLimit(ctx, store.ModelLimit{Model: c.model, RPM: &rpm}, nil); err != nil {
			t.Fatal(err)
		}
		table, column, id := "model_rate_windows", "model", c.model
		if _, err := pool.Exec(ctx, `INSERT INTO modelgateway.`+table+` (`+column+`,minute,requests) VALUES ($1,$2,$3)`, id, old, c.old); err != nil {
			t.Fatal(err)
		}
		c.tx, err = pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.tx.Rollback(context.Background())
		if _, err := c.tx.Exec(ctx, `UPDATE modelgateway.`+table+` SET requests=requests WHERE `+column+`=$1 AND minute=$2`, id, old); err != nil {
			t.Fatal(err)
		}
		if err := c.tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&c.pid); err != nil {
			t.Fatal(err)
		}
		c.result = make(chan result, 1)
	}
	// Queue well before the real database boundary. This observes actual row
	// waits; no application clock substitution or arbitrary sleep proves them.
	for remaining := edge.Sub(now()); remaining > 10*time.Second; remaining = edge.Sub(now()) {
		time.Sleep(min(remaining-10*time.Second, time.Second))
	}
	for i := range cases {
		c := &cases[i]
		go func() {
			if c.model == "completed" {
				u := usage(c.key, c.model, "", &store.Tokens{Input: 10, CacheWrite: 3, Output: 2, CacheRead: 99})
				u.RequestID = "completed"
				_, err := s.RecordUsage(ctx, u)
				c.result <- result{err: err}
				return
			}
			a, err := s.AdmitModel(ctx, c.model)
			c.result <- result{a, err}
		}()
	}
	for _, c := range cases {
		for {
			var blocked bool
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp(), EXISTS (
				SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
				AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, c.pid).Scan(&at, &blocked); err != nil {
				t.Fatal(err)
			}
			if !at.Before(edge) {
				t.Fatalf("did not observe %s queued before the minute boundary", c.model)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for now().Before(edge.Add(150 * time.Millisecond)) {
		time.Sleep(20 * time.Millisecond)
	}
	for _, c := range cases {
		if err := c.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		got := <-c.result
		if got.err != nil {
			t.Fatal(got.err)
		}
		if c.model == "completed" {
			var orgMinute, recorded time.Time
			var input, output int64
			if err := pool.QueryRow(ctx, `SELECT minute,input_tokens,output_tokens FROM modelgateway.model_rate_windows WHERE model=$1`, c.model).Scan(&orgMinute, &input, &output); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT created_at FROM modelgateway.usage WHERE request_id='completed'`).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if !orgMinute.Equal(old) || !recorded.Truncate(time.Minute).Equal(old) || input != 13 || output != 2 {
				t.Errorf("completion split across buckets: model=%v ledger=%v tokens=%d/%d", orgMinute, recorded, input, output)
			}
			continue
		}
		if !got.admission.Admitted {
			t.Errorf("%s checked an expired window: %+v", c.model, got.admission)
		}
		var prior, current int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(sum(requests) FILTER (WHERE minute=$2),0), coalesce(sum(requests) FILTER (WHERE minute=$3),0)
			FROM modelgateway.model_rate_windows WHERE model=$1`, c.model, old, edge).Scan(&prior, &current); err != nil {
			t.Fatal(err)
		}
		wantOld := c.old
		if prior != wantOld || current != 1 {
			t.Errorf("%s organization count old=%d current=%d, want %d/1", c.model, prior, current, wantOld)
		}
	}
}

func TestUsageWritesHaveIndependentWaits(t *testing.T) {
	for _, blocked := range []string{"ledger", "model"} {
		t.Run(blocked, func(t *testing.T) {
			s, pool := newStore(t)
			ctx := t.Context()
			clearOfAMinute(t, pool)
			u := usage("key_blocked", "blocked", "", &store.Tokens{})
			u.PrincipalID = "principal_budget"
			if _, err := s.RecordUsage(ctx, u); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			lock := map[string]string{
				"ledger": `UPDATE modelgateway.usage_actor_daily SET requests=requests WHERE actor_id='principal_budget'`,
				"model":  `UPDATE modelgateway.model_rate_windows SET requests=requests WHERE model='blocked'`,
			}[blocked]
			if _, err := tx.Exec(ctx, lock); err != nil {
				t.Fatal(err)
			}
			limited, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			defer cancel()
			u.RequestID = "completed"
			u.Tokens = &store.Tokens{Input: 5, Output: 3, CacheWrite: 1, CacheRead: 99}
			cost, err := s.RecordUsage(limited, u)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s did not hit its bound: %v", blocked, err)
			}
			rows, _, err := s.ListUsage(ctx, store.UsageFilter{PrincipalID: u.PrincipalID}, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if blocked == "ledger" {
				if len(rows) != 1 || cost != nil {
					t.Errorf("blocked ledger committed: rows=%+v cost=%v", rows, cost)
				}
			} else if len(rows) != 2 || rows[0].Tokens == nil || rows[0].Tokens.Input != 5 || cost == nil {
				t.Errorf("counter wait lost completed usage: rows=%+v cost=%v", rows, cost)
			}
			var input, output int64
			if err := pool.QueryRow(ctx, `SELECT input_tokens,output_tokens FROM modelgateway.model_rate_windows WHERE model='blocked'`).Scan(&input, &output); err != nil {
				t.Fatal(err)
			}
			wantInput, wantOutput := int64(6), int64(3)
			if blocked == "model" {
				wantInput, wantOutput = 0, 0
			}
			if input != wantInput || output != wantOutput {
				t.Errorf("%s wait suppressed model accounting: input/output=%d/%d, want %d/%d", blocked, input, output, wantInput, wantOutput)
			}

		})
	}
}
