package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
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
	for _, u := range []store.Usage{ok, refused,
		usage("key_a", "chat", partly.ID, &store.Tokens{Input: 1000, Output: 100}),
		usage("key_a", "chat", "dep_gone", &store.Tokens{Input: 1000, Output: 100}),
		usage("key_b", "chat", priced.ID, nil)} {
		if err := s.RecordUsage(ctx, u, false); err != nil {
			t.Fatal(err)
		}
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
	days, err := s.ListDailyUsage(ctx, store.UsageFilter{APIKeyID: "key_a", DeploymentID: priced.ID}, today, today)
	if err != nil || len(days) != 1 {
		t.Fatalf("ListDailyUsage = %+v, %v", days, err)
	}
	d := days[0]
	if d.Day.Format(time.DateOnly) != today.Format(time.DateOnly) || d.APIKeyID != "key_a" || d.Alias != "chat" ||
		d.Requests != 2 || d.Errors != 1 || d.Tokens != *ok.Tokens || !near(d.Cost, 0.0073) {
		t.Errorf("the day's rollup is %+v", d)
	}
	if all, err := s.ListDailyUsage(ctx, store.UsageFilter{APIKeyID: "key_a"}, today.AddDate(0, 0, -1), today); err != nil || len(all) != 3 {
		t.Errorf("the key's rollups are %+v, %v; want one per deployment", all, err)
	}
	if none, err := s.ListDailyUsage(ctx, store.UsageFilter{}, today.AddDate(0, 0, -3), today.AddDate(0, 0, -1)); err != nil || len(none) != 0 {
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
	if err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", nil), false); err != nil {
		t.Fatal(err)
	}
	days, err := s.ListDailyUsage(ctx, store.UsageFilter{}, utc, utc)
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
		if err := s.RecordUsage(ctx, u, false); err != nil {
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

// RPM admits a key's requests until the minute's count reaches it and does
// not count the ones it refuses; TPM admits while the minute's completed
// tokens — a cache read not among them — are under it. Each refusal says
// which limit, and how long until the window ends. An earlier minute counts
// for nothing.
func TestAdmitCountsRequestsAndTokens(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
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
	if err := s.RecordUsage(ctx, u, true); err != nil {
		t.Fatal(err)
	}
	if _, tok := window(t, pool, "key_t"); tok != 950 {
		t.Errorf("the window counted %d tokens, want 950 without the cache read", tok)
	}
	if a, err := s.Admit(ctx, "key_t", nil, &tpm); err != nil || !a.Admitted {
		t.Errorf("under the limit: %+v, %v", a, err)
	}
	if err := s.RecordUsage(ctx, usage("key_t", "x", "dep_1", &store.Tokens{Input: 50}), true); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Admit(ctx, "key_t", nil, &tpm); err != nil || a.Admitted || !a.Tokens {
		t.Errorf("at the limit: %+v, %v; want refused for tokens", a, err)
	}
	// A key whose TPM is not limited leaves the windows alone.
	if err := s.RecordUsage(ctx, usage("key_u", "x", "dep_1", &store.Tokens{Input: 5}), false); err != nil {
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
		if err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", &store.Tokens{Input: 1}), true); err != nil {
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
	if err := s.RecordUsage(ctx, usage("key_a", "x", "dep_1", nil), false); err != nil {
		t.Fatal(err)
	}
	swept("a later sweep")
	stop()
	<-done
}
