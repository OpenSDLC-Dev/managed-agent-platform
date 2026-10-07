package store_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewPool(t)
	return store.New(pool), pool
}

func both() map[profile.Protocol]string {
	return map[profile.Protocol]string{
		profile.Anthropic: "https://api.deepseek.com/anthropic",
		profile.OpenAI:    "https://api.deepseek.com",
	}
}

func mkProvider(t *testing.T, s *store.Store, endpoints map[profile.Protocol]string) store.Provider {
	t.Helper()
	p, err := s.CreateProvider(context.Background(), store.Provider{
		Name: "deepseek prod", Profile: "deepseek", Endpoints: endpoints, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mkDeployment(t *testing.T, s *store.Store, providerID string, kind store.Kind) store.Deployment {
	t.Helper()
	d, err := s.CreateDeployment(context.Background(), store.Deployment{
		ProviderID: providerID, UpstreamModel: "deepseek-v4-pro", Kind: kind, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkKey(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $1, $1)`, id); err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, kind error, contains string) {
	t.Helper()
	if !errors.Is(err, kind) || !strings.Contains(err.Error(), contains) {
		t.Fatalf("err = %v, want %v naming %q", err, kind, contains)
	}
}

// A provider round-trips every field, gets an id of its own prefix, takes its
// mutable fields through Update and keeps its endpoints, and is gone once
// deleted.
func TestProviderLifecycle(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	in := store.Provider{
		Name: "deepseek prod", Profile: "deepseek", Endpoints: both(),
		Headers: map[string]string{"X-Gateway-Route": "a"}, StallTimeout: 90 * time.Second, PropagateTrace: true, Enabled: true,
	}
	p, err := s.CreateProvider(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ID, "gwprov_") || p.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", p)
	}
	got, err := s.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != in.Name || got.Profile != in.Profile || got.StallTimeout != in.StallTimeout || !got.PropagateTrace || !got.Enabled ||
		got.Endpoints[profile.Anthropic] != in.Endpoints[profile.Anthropic] || got.Endpoints[profile.OpenAI] != in.Endpoints[profile.OpenAI] ||
		got.Headers["X-Gateway-Route"] != "a" {
		t.Fatalf("got %+v, want %+v", got, in)
	}

	up, err := s.UpdateProvider(ctx, p.ID, func(p *store.Provider) error {
		p.Name, p.Headers, p.StallTimeout, p.PropagateTrace, p.Enabled = "renamed", nil, 0, false, false
		p.Endpoints = map[profile.Protocol]string{profile.OpenAI: "https://elsewhere.example"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "renamed" || len(up.Headers) != 0 || up.StallTimeout != 0 || up.PropagateTrace || up.Enabled ||
		up.Endpoints[profile.Anthropic] != in.Endpoints[profile.Anthropic] || !up.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("updated = %+v", up)
	}
	if list, err := s.ListProviders(ctx); err != nil || len(list) != 1 || list[0].Name != "renamed" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProvider(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.DeleteProvider(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.UpdateProvider(ctx, p.ID, func(*store.Provider) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update after delete: %v", err)
	}
}

// An update whose function fails writes nothing.
func TestUpdateFunctionErrorWritesNothing(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	refuse := errors.New("refused")
	if _, err := s.UpdateProvider(ctx, p.ID, func(p *store.Provider) error {
		p.Name = "changed"
		return refuse
	}); !errors.Is(err, refuse) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := s.GetProvider(ctx, p.ID); got.Name != p.Name {
		t.Fatalf("name = %q after a refused update", got.Name)
	}
}

// A credential's protocols default to every protocol its provider has an
// endpoint for, may name only those, and its sealed key round-trips.
func TestCredentials(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, map[profile.Protocol]string{profile.Anthropic: "https://api.deepseek.com/anthropic"})
	c, err := s.CreateCredential(ctx, store.Credential{
		ProviderID: p.ID, Ciphertext: []byte{0, 1, 2}, KeyID: "local-1", LastFour: "wxyz", Weight: 2, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ID, "gwcred_") || c.Kind != "api_key" || !slices.Equal(c.Protocols, []profile.Protocol{profile.Anthropic}) {
		t.Fatalf("created = %+v", c)
	}
	got, err := s.GetCredential(ctx, p.ID, c.ID)
	if err != nil || string(got.Ciphertext) != "\x00\x01\x02" || got.KeyID != "local-1" || got.LastFour != "wxyz" || got.Weight != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{1}, KeyID: "k",
		Protocols: []profile.Protocol{profile.OpenAI}, Weight: 1})
	wantErr(t, err, store.ErrInvalid, "openai")
	_, err = s.CreateCredential(ctx, store.Credential{ProviderID: "gwprov_missing", Ciphertext: []byte{1}, KeyID: "k", Weight: 1})
	wantErr(t, err, store.ErrNotFound, "gwprov_missing")
	_, err = s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{1}, KeyID: "k", Weight: 1,
		Protocols: []profile.Protocol{}})
	wantErr(t, err, store.ErrInvalid, "empty")

	up, err := s.UpdateCredential(ctx, p.ID, c.ID, func(c *store.Credential) error {
		c.Weight, c.Enabled, c.Ciphertext = 5, false, []byte("ignored")
		return nil
	})
	if err != nil || up.Weight != 5 || up.Enabled || string(up.Ciphertext) != "\x00\x01\x02" {
		t.Fatalf("updated = %+v, %v", up, err)
	}
	_, err = s.UpdateCredential(ctx, p.ID, c.ID, func(c *store.Credential) error {
		c.Protocols = []profile.Protocol{profile.OpenAI}
		return nil
	})
	wantErr(t, err, store.ErrInvalid, "openai")
	if _, err := s.GetCredential(ctx, "gwprov_other", c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential read under another provider: %v", err)
	}
	if list, err := s.ListCredentials(ctx, p.ID); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteCredential(ctx, p.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCredential(ctx, p.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// A provider with a deployment cannot be deleted; its credentials go with it
// once it can.
func TestProviderDeleteRespectsDeployments(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	c, err := s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{1}, KeyID: "k", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	d := mkDeployment(t, s, p.ID, store.KindChat)
	wantErr(t, s.DeleteProvider(ctx, p.ID), store.ErrConflict, d.ID)
	if err := s.DeleteDeployment(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, p.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential outlived its provider: %v", err)
	}
}

// A deployment round-trips its capabilities and prices, keeps its provider,
// model and kind through Update, and cannot be deleted while an alias routes
// to it.
func TestDeployments(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	in := 0.28
	d, err := s.CreateDeployment(ctx, store.Deployment{
		ProviderID: p.ID, UpstreamModel: "deepseek-v4-pro", Kind: store.KindChat, DisplayName: "V4 Pro",
		Capabilities: store.Capabilities{Tools: true, Thinking: true, MaxInputTokens: 128000, MaxTokens: 8192},
		Prices:       store.Prices{Input: &in}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil || !strings.HasPrefix(got.ID, "gwdep_") || got.Capabilities != d.Capabilities ||
		got.Prices.Input == nil || *got.Prices.Input != 0.28 || got.Prices.Output != nil || got.DisplayName != "V4 Pro" {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = s.CreateDeployment(ctx, store.Deployment{ProviderID: "gwprov_missing", UpstreamModel: "m", Kind: store.KindChat})
	wantErr(t, err, store.ErrInvalid, "gwprov_missing")

	out := 1.1
	up, err := s.UpdateDeployment(ctx, d.ID, func(d *store.Deployment) error {
		d.DisplayName, d.Prices.Output, d.Enabled = "renamed", &out, false
		d.UpstreamModel, d.Kind, d.ProviderID = "other", store.KindEmbedding, "gwprov_x"
		return nil
	})
	if err != nil || up.DisplayName != "renamed" || *up.Prices.Output != 1.1 || up.Enabled ||
		up.UpstreamModel != "deepseek-v4-pro" || up.Kind != store.KindChat || up.ProviderID != p.ID {
		t.Fatalf("updated = %+v, %v", up, err)
	}
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "fast", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.DeleteDeployment(ctx, d.ID), store.ErrConflict, "fast")
	if list, err := s.ListDeployments(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// An alias takes its kind from its targets, which must share one; an
// embedding alias has exactly one target, fixed at creation; a chat alias's
// targets are replaced whole.
func TestAliases(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := mkProvider(t, s, both())
	chat1, chat2 := mkDeployment(t, s, p.ID, store.KindChat), mkDeployment(t, s, p.ID, store.KindChat)
	emb1, emb2 := mkDeployment(t, s, p.ID, store.KindEmbedding), mkDeployment(t, s, p.ID, store.KindEmbedding)

	a, err := s.CreateAlias(ctx, store.Alias{Name: "claude-sonnet-4-5", DisplayName: "Sonnet stand-in",
		Targets: []store.Target{{DeploymentID: chat1.ID, Priority: 0, Weight: 3}, {DeploymentID: chat2.ID, Priority: 1, Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != store.KindChat || len(a.Targets) != 2 || a.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", a)
	}
	_, err = s.CreateAlias(ctx, store.Alias{Name: "claude-sonnet-4-5", Targets: []store.Target{{DeploymentID: chat1.ID, Weight: 1}}})
	wantErr(t, err, store.ErrConflict, "claude-sonnet-4-5")
	_, err = s.CreateAlias(ctx, store.Alias{Name: "mixed", Targets: []store.Target{{DeploymentID: chat1.ID, Weight: 1}, {DeploymentID: emb1.ID, Weight: 1}}})
	wantErr(t, err, store.ErrInvalid, "kind")
	_, err = s.CreateAlias(ctx, store.Alias{Name: "ghost", Targets: []store.Target{{DeploymentID: "gwdep_missing", Weight: 1}}})
	wantErr(t, err, store.ErrInvalid, "gwdep_missing")
	_, err = s.CreateAlias(ctx, store.Alias{Name: "two-spaces", Targets: []store.Target{{DeploymentID: emb1.ID, Weight: 1}, {DeploymentID: emb2.ID, Weight: 1}}})
	wantErr(t, err, store.ErrInvalid, "exactly one")
	_, err = s.CreateAlias(ctx, store.Alias{Name: "empty"})
	wantErr(t, err, store.ErrInvalid, "target")

	emb, err := s.CreateAlias(ctx, store.Alias{Name: "embed", Targets: []store.Target{{DeploymentID: emb1.ID, Weight: 1}}})
	if err != nil || emb.Kind != store.KindEmbedding {
		t.Fatalf("embedding alias = %+v, %v", emb, err)
	}
	_, err = s.UpdateAlias(ctx, "embed", func(a *store.Alias) error {
		a.Targets = []store.Target{{DeploymentID: emb2.ID, Weight: 1}}
		return nil
	})
	wantErr(t, err, store.ErrInvalid, "fixed")
	if up, err := s.UpdateAlias(ctx, "embed", func(a *store.Alias) error {
		a.DisplayName = "renamed"
		a.Targets[0].Weight = 4 // weight on its one target is not its deployment
		return nil
	}); err != nil || up.DisplayName != "renamed" || up.Targets[0].Weight != 4 {
		t.Fatalf("embedding rename = %+v, %v", up, err)
	}

	up, err := s.UpdateAlias(ctx, "claude-sonnet-4-5", func(a *store.Alias) error {
		a.Targets = []store.Target{{DeploymentID: chat2.ID, Priority: 2, Weight: 7}}
		return nil
	})
	if err != nil || len(up.Targets) != 1 || up.Targets[0] != (store.Target{DeploymentID: chat2.ID, Priority: 2, Weight: 7}) {
		t.Fatalf("replaced = %+v, %v", up, err)
	}
	_, err = s.UpdateAlias(ctx, "claude-sonnet-4-5", func(a *store.Alias) error {
		a.Targets = []store.Target{{DeploymentID: emb1.ID, Weight: 1}}
		return nil
	})
	wantErr(t, err, store.ErrInvalid, "kind")
	if list, err := s.ListAliases(ctx); err != nil || len(list) != 2 || list[0].Name != "claude-sonnet-4-5" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteAlias(ctx, "claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDeployment(ctx, chat2.ID); err != nil {
		t.Fatalf("deployment still held after its alias went: %v", err)
	}
	if _, err := s.GetAlias(ctx, "claude-sonnet-4-5"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

// A key policy grants one existing api key, names only existing aliases, and
// is replaced whole.
func TestKeyPolicies(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	mkKey(t, pool, "key_app")
	p := mkProvider(t, s, both())
	d := mkDeployment(t, s, p.ID, store.KindChat)
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "fast", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	rpm := int32(60)
	kp, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app", Aliases: []string{"fast"}, RPM: &rpm})
	if err != nil || !slices.Equal(kp.Aliases, []string{"fast"}) || *kp.RPM != 60 || kp.TPM != nil {
		t.Fatalf("put = %+v, %v", kp, err)
	}
	kp2, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app"})
	if err != nil || kp2.Aliases != nil || kp2.RPM != nil || !kp2.CreatedAt.Equal(kp.CreatedAt) || !kp2.UpdatedAt.After(kp.UpdatedAt) {
		t.Fatalf("replaced = %+v, %v", kp2, err)
	}
	_, err = s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_missing"})
	wantErr(t, err, store.ErrNotFound, "key_missing")
	_, err = s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app", Aliases: []string{"fast", "typo"}})
	wantErr(t, err, store.ErrInvalid, "typo")
	if got, err := s.GetKeyPolicy(ctx, "key_app"); err != nil || got.Aliases != nil {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if list, err := s.ListKeyPolicies(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteKeyPolicy(ctx, "key_app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetKeyPolicy(ctx, "key_app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.DeleteKeyPolicy(ctx, "key_app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// An alias a key policy grants cannot be deleted: the grant would outlive it
// and pass to the next alias of its name.
func TestDeleteAliasRespectsGrants(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	mkKey(t, pool, "key_app")
	p := mkProvider(t, s, both())
	d := mkDeployment(t, s, p.ID, store.KindChat)
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "fast", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app", Aliases: []string{"fast"}}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.DeleteAlias(ctx, "fast"), store.ErrConflict, "key_app")
	if _, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAlias(ctx, "fast"); err != nil {
		t.Fatalf("an alias no grant names: %v", err)
	}
}

// Every write commits a notification on the channel the catalog listens on,
// so each replica reloads; a failed write commits none.
func TestEveryWriteNotifies(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
		t.Fatal(err)
	}
	expect := func(what string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := conn.Conn().WaitForNotification(wctx)
			cancel()
			if err != nil {
				t.Fatalf("%s: notification %d of %d: %v", what, i+1, n, err)
			}
		}
		wctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if n, err := conn.Conn().WaitForNotification(wctx); err == nil {
			t.Fatalf("%s: an extra notification %+v", what, n)
		}
	}
	mkKey(t, pool, "key_app")
	p := mkProvider(t, s, both())
	expect("create provider", 1)
	if _, err := s.UpdateProvider(ctx, p.ID, func(p *store.Provider) error { p.Name = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	expect("update provider", 1)
	c, _ := s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{1}, KeyID: "k", Weight: 1})
	_, _ = s.UpdateCredential(ctx, p.ID, c.ID, func(c *store.Credential) error { c.Weight = 2; return nil })
	_ = s.DeleteCredential(ctx, p.ID, c.ID)
	expect("credential writes", 3)
	d := mkDeployment(t, s, p.ID, store.KindChat)
	_, _ = s.UpdateDeployment(ctx, d.ID, func(d *store.Deployment) error { d.Enabled = false; return nil })
	expect("deployment writes", 2)
	_, _ = s.CreateAlias(ctx, store.Alias{Name: "a", Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}})
	_, _ = s.UpdateAlias(ctx, "a", func(a *store.Alias) error { a.DisplayName = "x"; return nil })
	_, _ = s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app", Aliases: []string{"a"}})
	_ = s.DeleteKeyPolicy(ctx, "key_app")
	_ = s.DeleteAlias(ctx, "a")
	_ = s.DeleteDeployment(ctx, d.ID)
	_ = s.DeleteProvider(ctx, p.ID)
	expect("alias, policy and delete writes", 7)
	_, _ = s.CreateAlias(ctx, store.Alias{Name: "ghost", Targets: []store.Target{{DeploymentID: "gwdep_missing", Weight: 1}}})
	expect("a refused write", 0)
}

// Load reads one consistent configuration: every row of every table, with
// each alias's targets.
func TestLoad(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	mkKey(t, pool, "key_app")
	p := mkProvider(t, s, both())
	if _, err := s.CreateCredential(ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte{9}, KeyID: "k", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	d := mkDeployment(t, s, p.ID, store.KindChat)
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "*", Targets: []store.Target{{DeploymentID: d.ID, Priority: 1, Weight: 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: "key_app"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || len(cfg.Credentials) != 1 || string(cfg.Credentials[0].Ciphertext) != "\x09" ||
		len(cfg.Deployments) != 1 || len(cfg.Aliases) != 1 || len(cfg.KeyPolicies) != 1 ||
		cfg.Aliases[0].Targets[0] != (store.Target{DeploymentID: d.ID, Priority: 1, Weight: 2}) {
		t.Fatalf("loaded %+v", cfg)
	}
}
