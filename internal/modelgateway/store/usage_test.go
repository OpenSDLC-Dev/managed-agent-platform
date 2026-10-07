package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func price(v float64) *float64 { return &v }

func usage(key, alias, dep string, tokens *store.Tokens) store.Usage {
	return store.Usage{RequestID: "req_1", APIKeyID: key, Model: alias, Alias: alias, DeploymentID: dep,
		CredentialID: "cred_1", Protocol: "anthropic", Endpoint: "messages", Status: 200, Tokens: tokens,
		Latency: 1500 * time.Millisecond}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// A request's row carries what the caller asked and got, its tokens costed
// at the deployment's prices — a missing price costing nothing, no reported
// tokens costing nothing and recording none — and it is added to its UTC
// day's rollup for the key, alias and deployment, an error counted as one.
func TestRecordUsageWritesTheLedgerAndItsDay(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	priced, err := s.CreateDeployment(ctx, store.Deployment{ProviderID: p.ID, UpstreamModel: "m", Kind: store.KindChat,
		Enabled: true, Prices: store.Prices{Input: price(2), Output: price(8), CacheWrite: price(2.5), CacheRead: price(0.2)}})
	if err != nil {
		t.Fatal(err)
	}
	partly, err := s.CreateDeployment(ctx, store.Deployment{ProviderID: p.ID, UpstreamModel: "n", Kind: store.KindChat,
		Enabled: true, Prices: store.Prices{Output: price(10)}})
	if err != nil {
		t.Fatal(err)
	}

	ok := usage("key_a", "chat", priced.ID, &store.Tokens{Input: 1000, Output: 500, CacheWrite: 200, CacheRead: 4000})
	ok.SessionID, ok.TTFT, ok.Model = "sesn_1", 300*time.Millisecond, "claude-sonnet"
	refused := usage("key_a", "chat", priced.ID, nil)
	refused.Status, refused.ErrorType = 529, "overloaded_error"
	var costs []*float64
	for _, u := range []store.Usage{ok, refused,
		usage("key_a", "chat", partly.ID, &store.Tokens{Input: 1000, Output: 100}),
		usage("key_a", "chat", "dep_gone", &store.Tokens{Input: 1000, Output: 100}),
		usage("key_b", "chat", priced.ID, nil)} {
		cost, err := s.RecordUsage(ctx, u, false)
		if err != nil {
			t.Fatal(err)
		}
		costs = append(costs, cost)
	}
	if costs[0] == nil || !near(*costs[0], 0.0073) || costs[1] != nil || costs[2] == nil || !near(*costs[2], 0.001) ||
		costs[3] == nil || *costs[3] != 0 || costs[4] != nil {
		t.Errorf("RecordUsage returned costs %v, want each row's", costs)
	}

	rows, more, err := s.ListUsage(ctx, store.UsageFilter{APIKeyID: "key_a"}, 0, 10)
	if err != nil || more || len(rows) != 4 {
		t.Fatalf("ListUsage = %d rows, more %t, %v", len(rows), more, err)
	}
	gone, part, bad, good := rows[0], rows[1], rows[2], rows[3]
	if good.Model != "claude-sonnet" || good.Alias != "chat" || good.DeploymentID != priced.ID || good.CredentialID != "cred_1" ||
		good.SessionID != "sesn_1" || good.Protocol != "anthropic" || good.Endpoint != "messages" || good.Status != 200 ||
		good.ErrorType != "" || good.Latency != 1500*time.Millisecond || good.TTFT != 300*time.Millisecond ||
		*good.Tokens != *ok.Tokens || good.Cost == nil || !near(*good.Cost, 0.0073) || good.CreatedAt.IsZero() || good.RequestID != "req_1" {
		t.Errorf("the answered request reads back as %+v (cost %v)", good, good.Cost)
	}
	if bad.Tokens != nil || bad.Cost != nil || bad.ErrorType != "overloaded_error" || bad.Status != 529 || bad.SessionID != "" || bad.TTFT != 0 {
		t.Errorf("the refused request reads back as %+v", bad)
	}
	if part.Cost == nil || !near(*part.Cost, 0.001) || gone.Cost == nil || *gone.Cost != 0 {
		t.Errorf("an unpriced token costs %v and a deleted deployment's %v, want 0.001 and 0", part.Cost, gone.Cost)
	}

	today := time.Now().UTC()
	days, _, err := s.ListDailyUsage(ctx, store.UsageFilter{APIKeyID: "key_a", DeploymentID: priced.ID}, today, today, 100)
	if err != nil || len(days) != 1 {
		t.Fatalf("ListDailyUsage = %+v, %v", days, err)
	}
	d := days[0]
	if d.Day.Format(time.DateOnly) != today.Format(time.DateOnly) || d.APIKeyID != "key_a" || d.Alias != "chat" ||
		d.Requests != 2 || d.Errors != 1 || d.Tokens != *ok.Tokens || !near(d.Cost, 0.0073) {
		t.Errorf("the day's rollup is %+v", d)
	}
	if all, more, err := s.ListDailyUsage(ctx, store.UsageFilter{APIKeyID: "key_a"}, today.AddDate(0, 0, -1), today, 3); err != nil || len(all) != 3 || more {
		t.Errorf("the key's rollups are %+v, %v; want one per deployment", all, err)
	}
	if cut, more, err := s.ListDailyUsage(ctx, store.UsageFilter{APIKeyID: "key_a"}, today.AddDate(0, 0, -1), today, 2); err != nil || len(cut) != 2 || !more {
		t.Errorf("the three rollups read two at a time: %d rows, more %t, %v", len(cut), more, err)
	}
	if none, _, err := s.ListDailyUsage(ctx, store.UsageFilter{}, today.AddDate(0, 0, -3), today.AddDate(0, 0, -1), 100); err != nil || len(none) != 0 {
		t.Errorf("earlier days hold %+v, %v", none, err)
	}
}

// A request counts in its UTC day, whatever the database's time zone.
func TestADayIsAUTCDay(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	// A zone whose date is not UTC's now: fourteen hours ahead from UTC noon
	// on, twelve behind before it.
	zone := "Etc/GMT+12"
	if time.Now().UTC().Hour() >= 12 {
		zone = "Etc/GMT-14"
	}
	if _, err := pool.Exec(ctx, `DO $$ BEGIN
		EXECUTE format('ALTER DATABASE %I SET timezone = %L', current_database(), '`+zone+`');
	END $$`); err != nil {
		t.Fatal(err)
	}
	pool.Reset()
	var local string
	if err := pool.QueryRow(ctx, `SELECT now()::date::text`).Scan(&local); err != nil {
		t.Fatal(err)
	}
	utc := time.Now().UTC()
	if local == utc.Format(time.DateOnly) {
		t.Fatalf("%s shares UTC's date", zone)
	}
	if _, err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", nil), false); err != nil {
		t.Fatal(err)
	}
	days, _, err := s.ListDailyUsage(ctx, store.UsageFilter{}, utc, utc, 100)
	if err != nil || len(days) != 1 || days[0].Day.Format(time.DateOnly) != utc.Format(time.DateOnly) {
		t.Errorf("UTC's day holds %+v, %v; the database's is %s", days, err, local)
	}
}

// The ledger reads newest first, a page at a time, narrowed by key, alias,
// deployment or session.
func TestListUsagePagesAndFilters(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	var all []store.Usage
	for i, u := range []store.Usage{
		usage("key_a", "x", "dep_1", nil), usage("key_b", "x", "dep_1", nil), usage("key_a", "y", "dep_2", nil),
		usage("key_a", "x", "dep_2", nil), usage("key_b", "y", "dep_1", nil),
	} {
		u.SessionID = []string{"s1", "s2", "s1", "", "s2"}[i]
		if _, err := s.RecordUsage(ctx, u, false); err != nil {
			t.Fatal(err)
		}
		all = append(all, u)
	}
	var unset int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM modelgateway.usage WHERE session_id IS NULL`).Scan(&unset); err != nil || unset != 1 {
		t.Errorf("%d rows store no session (%v), want the 1 sent without one", unset, err)
	}
	var ids []int64
	before := int64(0)
	for range 10 {
		page, more, err := s.ListUsage(ctx, store.UsageFilter{}, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range page {
			ids = append(ids, u.ID)
		}
		if !more {
			break
		}
		before = page[len(page)-1].ID
	}
	if len(ids) != 5 || ids[0] <= ids[1] || ids[3] <= ids[4] {
		t.Fatalf("paged ids %v, want five newest first", ids)
	}
	for name, tc := range map[string]struct {
		f    store.UsageFilter
		want int
	}{
		"key":        {store.UsageFilter{APIKeyID: "key_a"}, 3},
		"alias":      {store.UsageFilter{Alias: "y"}, 2},
		"deployment": {store.UsageFilter{DeploymentID: "dep_2"}, 2},
		"session":    {store.UsageFilter{SessionID: "s2"}, 2},
		"combined":   {store.UsageFilter{APIKeyID: "key_a", Alias: "x", SessionID: "s1"}, 1},
	} {
		rows, more, err := s.ListUsage(ctx, tc.f, 0, 10)
		if err != nil || more || len(rows) != tc.want {
			t.Errorf("%s: %d rows, more %t, %v; want %d", name, len(rows), more, err, tc.want)
		}
	}
}

func window(t *testing.T, pool *pgxpool.Pool, key string) (requests, tokens int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT coalesce(sum(requests), 0), coalesce(sum(tokens), 0)
		FROM modelgateway.rate_windows WHERE api_key_id = $1 AND minute = date_trunc('minute', now())`, key).Scan(&requests, &tokens)
	if err != nil {
		t.Fatal(err)
	}
	return requests, tokens
}

// clearOfAMinute waits out the database's current minute when less than
// five seconds of it remain, so the admissions a test counts share one
// window.
func clearOfAMinute(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var left float64
	if err := pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM date_trunc('minute', now()) + interval '1 minute' - now())::float8`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left < 5 {
		time.Sleep(time.Duration(left*float64(time.Second)) + 100*time.Millisecond)
	}
}

// RPM admits a key's requests until the minute's count reaches it and does
// not count the ones it refuses; TPM admits while the minute's completed
// tokens — a cache read not among them — are under it. Each refusal says
// which limit, and how long until the window ends. An earlier minute counts
// for nothing.
func TestAdmitCountsRequestsAndTokens(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	clearOfAMinute(t, pool)
	rpm, tpm := int32(2), int64(1000)

	if _, err := pool.Exec(ctx, `INSERT INTO modelgateway.rate_windows (api_key_id, minute, requests, tokens)
		VALUES ('key_r', date_trunc('minute', now()) - interval '2 minutes', 100, 100000)`); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, false, false} {
		a, err := s.Admit(ctx, "key_r", &rpm, nil)
		if err != nil || a.Admitted != want || a.Tokens || a.RetryAfter < 1 || a.RetryAfter > 60 {
			t.Errorf("request %d: %+v, %v; want admitted %t", i, a, err, want)
		}
	}
	if r, _ := window(t, pool, "key_r"); r != 2 {
		t.Errorf("the window counted %d requests, want the 2 admitted", r)
	}

	if a, err := s.Admit(ctx, "key_t", nil, &tpm); err != nil || !a.Admitted {
		t.Fatalf("the first request: %+v, %v", a, err)
	}
	u := usage("key_t", "x", "dep_1", &store.Tokens{Input: 600, Output: 300, CacheWrite: 50, CacheRead: 50000})
	if _, err := s.RecordUsage(ctx, u, true); err != nil {
		t.Fatal(err)
	}
	if _, tok := window(t, pool, "key_t"); tok != 950 {
		t.Errorf("the window counted %d tokens, want 950 without the cache read", tok)
	}
	if a, err := s.Admit(ctx, "key_t", nil, &tpm); err != nil || !a.Admitted {
		t.Errorf("under the limit: %+v, %v", a, err)
	}
	if _, err := s.RecordUsage(ctx, usage("key_t", "x", "dep_1", &store.Tokens{Input: 50}), true); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Admit(ctx, "key_t", nil, &tpm); err != nil || a.Admitted || !a.Tokens {
		t.Errorf("at the limit: %+v, %v; want refused for tokens", a, err)
	}
	// With both limits set, the refusal names the one reached.
	both, one := int32(5), int32(1)
	if _, err := s.RecordUsage(ctx, usage("key_both", "x", "dep_1", &store.Tokens{Input: 1000}), true); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Admit(ctx, "key_both", &both, &tpm); err != nil || a.Admitted || !a.Tokens {
		t.Errorf("both limits, tokens reached: %+v, %v", a, err)
	}
	for i, want := range []bool{true, false} {
		if a, err := s.Admit(ctx, "key_rpm", &one, &tpm); err != nil || a.Admitted != want || a.Tokens {
			t.Errorf("both limits, request %d: %+v, %v; want admitted %t and never for tokens", i, a, err, want)
		}
	}
	// A key whose TPM is not limited leaves the windows alone.
	if _, err := s.RecordUsage(ctx, usage("key_u", "x", "dep_1", &store.Tokens{Input: 5}), false); err != nil {
		t.Fatal(err)
	}
	if r, tok := window(t, pool, "key_u"); r != 0 || tok != 0 {
		t.Errorf("an unlimited key's window holds %d requests and %d tokens", r, tok)
	}
}

// A sweep deletes the rows past retention a batch at a time, and the rate
// windows no admission reads, and keeps the daily rollups; a replica that
// finds the sweep's lock held deletes nothing. RunRetention sweeps at once
// and again each interval until its context ends.
func TestSweepUsage(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	defer store.SetSweepBatch(2)()
	for range 7 {
		if _, err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", &store.Tokens{Input: 1}), true); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE modelgateway.usage SET created_at = now() - interval '91 days'
		WHERE id IN (SELECT id FROM modelgateway.usage ORDER BY id LIMIT 5)`)
	exec(`INSERT INTO modelgateway.rate_windows (api_key_id, minute, requests)
		VALUES ('key_a', date_trunc('minute', now()) - interval '2 hours', 3)`)
	count := func(sql string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, store.SweepLock); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepUsage(ctx, 90*24*time.Hour); err != nil || n != 0 {
		t.Errorf("a sweep under another's lock deleted %d, %v", n, err)
	}
	_ = tx.Rollback(ctx)

	if n, err := s.SweepUsage(ctx, 90*24*time.Hour); err != nil || n != 5 {
		t.Errorf("the sweep deleted %d, %v; want the 5 past retention", n, err)
	}
	if left := count(`SELECT count(*) FROM modelgateway.usage`); left != 2 {
		t.Errorf("%d rows left, want 2", left)
	}
	if w := count(`SELECT count(*) FROM modelgateway.rate_windows`); w != 1 {
		t.Errorf("%d rate windows left, want the current one", w)
	}
	if d := count(`SELECT requests FROM modelgateway.usage_daily`); d != 7 {
		t.Errorf("the rollup counts %d requests, want all 7", d)
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.RunRetention(runCtx, 10*time.Millisecond, time.Millisecond) }()
	swept := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for count(`SELECT count(*) FROM modelgateway.usage`) != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if left := count(`SELECT count(*) FROM modelgateway.usage`); left != 0 {
			t.Fatalf("%s: RunRetention left %d rows older than a millisecond", what, left)
		}
	}
	swept("the first sweep")
	if _, err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", nil), false); err != nil {
		t.Fatal(err)
	}
	swept("a later sweep")
	stop()
	<-done
}

// A row the ledger refuses — here for a NUL, which Postgres text refuses —
// still counts its tokens against the key's TPM: the window is written on
// its own.
func TestATokenCountOutlivesItsLedgerRow(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	clearOfAMinute(t, pool)
	u := usage("key_n", "x", "dep_1", &store.Tokens{Input: 40, Output: 2, CacheRead: 9})
	u.Model = "bad\x00name"
	if cost, err := s.RecordUsage(ctx, u, true); err == nil || cost != nil {
		t.Fatalf("the ledger took a NUL: cost %v, %v", cost, err)
	}
	if _, tok := window(t, pool, "key_n"); tok != 42 {
		t.Errorf("the window counted %d tokens, want 42", tok)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM modelgateway.usage WHERE api_key_id = 'key_n'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d ledger rows (%v), want none", n, err)
	}
}

// A cost no float64 holds, or that is no number — from a price the admin
// API refuses, written to the database some other way — loses its caller the
// reading, never the ledger row.
func TestAnUnreadableCostStillWritesItsRow(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	for i, v := range []float64{1e308, math.NaN(), math.Inf(1)} {
		d, err := s.CreateDeployment(ctx, store.Deployment{ProviderID: p.ID, UpstreamModel: fmt.Sprint("m", i), Kind: store.KindChat,
			Enabled: true, Prices: store.Prices{Input: price(v)}})
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprint("key_big", i)
		if cost, err := s.RecordUsage(ctx, usage(key, "x", d.ID, &store.Tokens{Input: 2_000_000}), false); err != nil || cost != nil {
			t.Fatalf("price %v: RecordUsage = cost %v, %v; want the row written without a cost", v, cost, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM modelgateway.usage WHERE api_key_id = $1`, key).Scan(&n); err != nil || n != 1 {
			t.Errorf("price %v: %d ledger rows (%v), want 1", v, n, err)
		}
	}
}

// A database an operator defaults to REPEATABLE READ refuses to update a row
// another transaction updated since the statement began. Admission, a
// window's tokens and a day's rollup each upsert a row other requests
// update too, and each goes on after waiting out such an update.
func TestTheLimitsAndTheLedgerAreReadCommitted(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	pgtest.DefaultRepeatableRead(t, pool)
	clearOfAMinute(t, pool)
	rpm := int32(100)
	if _, err := s.Admit(ctx, "key_rr", &rpm, nil); err != nil {
		t.Fatal(err)
	}
	record := func(tpmLimited bool) error {
		_, err := s.RecordUsage(ctx, usage("key_rr", "x", "dep_1", &store.Tokens{Input: 1}), tpmLimited)
		return err
	}
	if err := record(true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, lock string
		run        func() error
	}{
		{"admission", `UPDATE modelgateway.rate_windows SET requests = requests WHERE api_key_id = 'key_rr'`,
			func() error { _, err := s.Admit(ctx, "key_rr", &rpm, nil); return err }},
		{"window", `UPDATE modelgateway.rate_windows SET tokens = tokens WHERE api_key_id = 'key_rr'`,
			func() error { return record(true) }},
		{"rollup", `UPDATE modelgateway.usage_daily SET requests = requests WHERE api_key_id = 'key_rr'`,
			func() error { return record(false) }},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, c.lock); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.run() }()
		awaitALockWait(t, pool)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Errorf("%s after a concurrent update: %v", c.name, err)
		}
	}
}

// awaitALockWait returns once a statement on pool's database waits on a lock.
func awaitALockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no statement waited on the lock")
		}
	}
}
