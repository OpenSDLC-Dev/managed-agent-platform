package store_test

import (
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// The first contraction must work after cleanup, while keeping these tables
// available to the previous binary until the rollout has been accepted.
func TestGatewayStoreDoesNotReadRetiredPolicyTables(t *testing.T) {
	s, pool := newStore(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `ALTER TABLE modelgateway.key_policies RENAME TO retired_key_policies;
		ALTER TABLE modelgateway.rate_windows RENAME TO retired_rate_windows`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); err != nil {
		t.Errorf("catalog still depends on retired policies: %v", err)
	}
	p := mkProvider(t, s, both())
	d := mkDeployment(t, s, p.ID, store.KindChat)
	if _, err := s.CreateAlias(ctx, store.Alias{Name: "retirement", Kind: store.KindChat, Targets: []store.Target{{DeploymentID: d.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAlias(ctx, "retirement"); err != nil {
		t.Errorf("alias deletion still depends on retired policies: %v", err)
	}
	if _, err := s.SweepUsage(ctx, time.Hour); err != nil {
		t.Errorf("retention still depends on retired windows: %v", err)
	}
}
