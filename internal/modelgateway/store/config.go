package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Load reads every row in one repeatable-read snapshot, so a catalog never
// sees half of a write.
func (s *Store) Load(ctx context.Context) (Config, error) {
	var cfg Config
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		if cfg.Providers, err = listProviders(ctx, tx); err != nil {
			return err
		}
		if cfg.Credentials, err = listCredentials(ctx, tx, ""); err != nil {
			return err
		}
		if cfg.Deployments, err = listDeployments(ctx, tx); err != nil {
			return err
		}
		if cfg.Aliases, err = listAliases(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	return cfg, err
}
