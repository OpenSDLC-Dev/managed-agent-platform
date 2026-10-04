// Package catalog is the model gateway's view of its configuration: an
// immutable Snapshot of every row, replaced whole when the configuration
// changes, so the request path reads it without a query or a lock
// (docs/plan/59_model-gateway.md, "Configuration model").
//
// A replica learns of a change three ways. Every admin write commits a
// notification on store.NotifyChannel, and Run reloads on each. Run also
// reloads on a periodic tick, so a notification lost to a dropped connection
// costs at most one tick of staleness. And Run reloads each time it
// subscribes, so whatever changed while it was not listening — before the
// first subscription, or across a reconnect — is read then.
//
// The subscription holds a connection of its own, dialled from the pool's
// configuration rather than taken from the pool: a pooled connection would
// count against the pool's limit while it waits, so on a small pool a reload
// could wait forever for the connection the listener holds, and a connection
// left subscribed must never go back to a pool that would hand it out.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Snapshot is one consistent reading of the configuration. It is never
// modified after it is built, and everything it returns shares its storage
// with the snapshot every request reads: a caller copies a map or a slice
// before it changes one.
type Snapshot struct {
	providers   map[string]store.Provider
	credentials map[string][]store.Credential // by provider id
	deployments map[string]store.Deployment
	aliases     map[string]store.Alias
	policies    map[string]store.KeyPolicy
}

func newSnapshot(cfg store.Config) *Snapshot {
	s := &Snapshot{
		providers:   make(map[string]store.Provider, len(cfg.Providers)),
		credentials: make(map[string][]store.Credential, len(cfg.Providers)),
		deployments: make(map[string]store.Deployment, len(cfg.Deployments)),
		aliases:     make(map[string]store.Alias, len(cfg.Aliases)),
		policies:    make(map[string]store.KeyPolicy, len(cfg.KeyPolicies)),
	}
	for _, p := range cfg.Providers {
		s.providers[p.ID] = p
	}
	for _, c := range cfg.Credentials {
		s.credentials[c.ProviderID] = append(s.credentials[c.ProviderID], c)
	}
	for _, d := range cfg.Deployments {
		s.deployments[d.ID] = d
	}
	for _, a := range cfg.Aliases {
		s.aliases[a.Name] = a
	}
	for _, k := range cfg.KeyPolicies {
		s.policies[k.APIKeyID] = k
	}
	return s
}

// Provider returns the provider with id.
func (s *Snapshot) Provider(id string) (store.Provider, bool) {
	p, ok := s.providers[id]
	return p, ok
}

// Credentials returns a provider's credentials, oldest first.
func (s *Snapshot) Credentials(providerID string) []store.Credential {
	return s.credentials[providerID]
}

// Deployment returns the deployment with id.
func (s *Snapshot) Deployment(id string) (store.Deployment, bool) {
	d, ok := s.deployments[id]
	return d, ok
}

// Alias resolves the model name a caller sent: the alias of that exact name,
// else the wildcard alias "*". The returned alias's Name is the configured
// name that matched — "*" for a wildcard match — which is what a metric may
// carry without a caller growing it.
func (s *Snapshot) Alias(model string) (store.Alias, bool) {
	if a, ok := s.aliases[model]; ok {
		return a, true
	}
	a, ok := s.aliases["*"]
	return a, ok
}

// Aliases returns every alias, ordered by name.
func (s *Snapshot) Aliases() []store.Alias {
	out := make([]store.Alias, 0, len(s.aliases))
	for _, a := range s.aliases {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KeyPolicy returns the grant of the platform API key with id.
func (s *Snapshot) KeyPolicy(apiKeyID string) (store.KeyPolicy, bool) {
	k, ok := s.policies[apiKeyID]
	return k, ok
}

// Catalog holds the current Snapshot.
type Catalog struct {
	store   *store.Store
	conn    *pgx.ConnConfig // the subscription's, the pool's own settings
	tick    time.Duration
	current atomic.Pointer[Snapshot]
}

// New loads the configuration once, so the catalog is ready to serve before
// Run starts; tick is the periodic reload's interval.
func New(ctx context.Context, pool *pgxpool.Pool, tick time.Duration) (*Catalog, error) {
	if tick <= 0 {
		return nil, fmt.Errorf("modelgateway catalog: the reload interval must be positive, not %s", tick)
	}
	s := store.New(pool)
	c := &Catalog{store: s, conn: pool.Config().ConnConfig.Copy(), tick: tick}
	cfg, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	c.current.Store(newSnapshot(cfg))
	return c, nil
}

// Snapshot returns the current configuration.
func (c *Catalog) Snapshot() *Snapshot { return c.current.Load() }

// reconnectBackoff paces resubscribing after a lost connection.
const reconnectBackoff = time.Second

// Run keeps the snapshot current until ctx ends.
func (c *Catalog) Run(ctx context.Context) {
	for {
		err := c.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.WarnContext(ctx, "modelgateway: configuration listener lost; resubscribing", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectBackoff):
		}
	}
}

// dialTimeout bounds dialling the subscription's connection.
const dialTimeout = 10 * time.Second

// listen subscribes on a connection of its own and reloads on each
// notification and each tick, returning when the connection fails or ctx
// ends.
func (c *Catalog) listen(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := pgx.ConnectConfig(dctx, c.conn)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		// Under a deadline rather than context.Background(), which pgconn
		// answers by watching nothing, so a dead peer cannot hold Run's
		// shutdown on the Terminate flush.
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Close(cctx)
		cancel()
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
		return err
	}
	c.reload(ctx)
	for {
		wctx, cancel := context.WithTimeout(ctx, c.tick)
		_, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil, errors.Is(err, context.DeadlineExceeded):
			// A notification, or the tick: pgconn leaves a connection
			// open across a timed-out wait.
			c.reload(ctx)
		default:
			return err
		}
	}
}

// reload replaces the snapshot, keeping the current one if the read fails.
func (c *Catalog) reload(ctx context.Context) {
	cfg, err := c.store.Load(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "modelgateway: configuration reload failed; serving the previous snapshot", "error", err)
		}
		return
	}
	c.current.Store(newSnapshot(cfg))
}
