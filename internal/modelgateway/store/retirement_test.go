package store_test

import (
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// Current gateway operations must work with the obsolete tables absent.
func TestGatewayStoreDoesNotReadRetiredPolicyTables(t *testing.T) {
	s, pool := newStore(t)
	ctx := t.Context()
	var retired bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('modelgateway.key_policies') IS NULL
		AND to_regclass('modelgateway.rate_windows') IS NULL`).Scan(&retired); err != nil || !retired {
		t.Fatalf("obsolete policy tables remain: %t, %v", retired, err)
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
