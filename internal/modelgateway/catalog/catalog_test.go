package catalog_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func seed(t *testing.T, s *store.Store, pool *pgxpool.Pool) (store.Provider, store.Deployment) {
	t.Helper()
	ctx := context.Background()
	p, err := s.CreateProvider(ctx, store.Provider{Name: "p", Profile: "deepseek", Enabled: true,
		Endpoints: map[profile.Protocol]string{profile.Anthropic: "https://api.deepseek.com/anthropic"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{1}, KeyID: "k", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeployment(ctx, store.Deployment{ProviderID: p.ID, UpstreamModel: "deepseek-flash", Kind: store.KindChat, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "fast", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_app', 'app', 'h')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app", Aliases: []string{"fast"}}); err != nil {
		t.Fatal(err)
	}
	return p, d
}

// run starts the catalog's reload loop for the test's lifetime.
func run(t *testing.T, c *catalog.Catalog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	})
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// New reads the whole configuration before it returns, so the first request
// a replica serves already sees it.
func TestNewLoadsTheConfiguration(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := store.New(pool)
	p, d := seed(t, s, pool)
	c, err := catalog.New(context.Background(), s, pool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if got, ok := snap.Provider(p.ID); !ok || got.Name != "p" {
		t.Errorf("provider = %+v, %t", got, ok)
	}
	if creds := snap.Credentials(p.ID); len(creds) != 1 {
		t.Errorf("credentials = %+v", creds)
	}
	if got, ok := snap.Deployment(d.ID); !ok || got.UpstreamModel != "deepseek-flash" {
		t.Errorf("deployment = %+v, %t", got, ok)
	}
	if got, ok := snap.KeyPolicy("key_app"); !ok || len(got.Aliases) != 1 {
		t.Errorf("key policy = %+v, %t", got, ok)
	}
	if _, ok := snap.KeyPolicy("key_other"); ok {
		t.Error("a key without a grant has one")
	}
}

// A write through the store reaches every running catalog by its
// notification, well before the periodic reload.
func TestAWriteReloadsByNotification(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := store.New(pool)
	p, _ := seed(t, s, pool)
	c, err := catalog.New(context.Background(), s, pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)
	// The write may land before Run subscribes; the reload Run makes once it
	// is listening covers that race.
	if _, err := s.UpdateProvider(context.Background(), p.ID, func(p *store.Provider) error { p.Name = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, "renamed provider visible", 5*time.Second, func() bool {
		got, _ := c.Snapshot().Provider(p.ID)
		return got.Name == "renamed"
	})
}

// A change that arrives without a notification, as a missed one would, is
// picked up by the periodic reload.
func TestThePeriodicReloadHealsAMissedNotification(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := store.New(pool)
	p, _ := seed(t, s, pool)
	c, err := catalog.New(context.Background(), s, pool, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)
	time.Sleep(300 * time.Millisecond) // several ticks on one connection
	if _, err := pool.Exec(context.Background(), `UPDATE modelgateway.providers SET name = 'unannounced' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "unannounced change visible", 5*time.Second, func() bool {
		got, _ := c.Snapshot().Provider(p.ID)
		return got.Name == "unannounced"
	})
	// The connection that timed out is still the one listening.
	if _, err := s.UpdateProvider(context.Background(), p.ID, func(p *store.Provider) error { p.Name = "announced"; return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, "announced change visible", 5*time.Second, func() bool {
		got, _ := c.Snapshot().Provider(p.ID)
		return got.Name == "announced"
	})
}

// A listening connection the server drops is replaced, and the change made
// while it was down is read on resubscribing.
func TestALostConnectionIsReplaced(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := store.New(pool)
	p, _ := seed(t, s, pool)
	c, err := catalog.New(context.Background(), s, pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)
	ctx := context.Background()
	listener := func() (pid int32) {
		_ = pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND query = 'LISTEN `+store.NotifyChannel+`'`).Scan(&pid)
		return pid
	}
	eventually(t, "catalog listening", 5*time.Second, func() bool { return listener() != 0 })
	old := listener()
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE modelgateway.providers SET name = 'while-down' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a new listener", 10*time.Second, func() bool { pid := listener(); return pid != 0 && pid != old })
	eventually(t, "the change made while down visible", 5*time.Second, func() bool {
		got, _ := c.Snapshot().Provider(p.ID)
		return got.Name == "while-down"
	})
}

// An alias resolves by exact name first, then by the wildcard; with no
// wildcard, an unknown name resolves to nothing.
func TestAliasResolution(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := store.New(pool)
	_, d := seed(t, s, pool)
	c, err := catalog.New(context.Background(), s, pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := c.Snapshot().Alias("fast"); !ok || a.Name != "fast" {
		t.Errorf("exact = %+v, %t", a, ok)
	}
	if _, ok := c.Snapshot().Alias("claude-sonnet-4-5"); ok {
		t.Error("an unknown name resolved with no wildcard configured")
	}
	if _, err := s.CreateAlias(context.Background(), store.Alias{Name: "*", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	c, err = catalog.New(context.Background(), s, pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := c.Snapshot().Alias("claude-sonnet-4-5"); !ok || a.Name != "*" {
		t.Errorf("wildcard = %+v, %t", a, ok)
	}
	if a, ok := c.Snapshot().Alias("fast"); !ok || a.Name != "fast" {
		t.Errorf("exact beside a wildcard = %+v, %t", a, ok)
	}
}
