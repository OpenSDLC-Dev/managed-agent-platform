package apikey_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestOwnedPresetCheck(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,role) VALUES ('principal_owner','preset-test','owner','developer')`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"active", "inactive", "archived", "expired"} {
		status := state
		if state == "expired" {
			status = "active"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id,name,owner_principal_id,owned_key_hash,created_by,status,expires_at)
			VALUES ($1,$1,'principal_owner',$2,'principal_owner',$3,
			CASE WHEN $4 THEN now() - interval '1 hour' ELSE NULL END)`, "apikey_"+state, apikey.Hash("owned-"+state), status, state == "expired"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id,name,key_hash,status,expires_at)
			VALUES ($1,$1,$2,$3,CASE WHEN $4 THEN now() - interval '1 hour' ELSE NULL END)`,
			"apikey_unowned_"+state, apikey.Hash("unowned-"+state), status, state == "expired"); err != nil {
			t.Fatal(err)
		}
		for _, keys := range [][]string{{"owned-" + state, "unknown"}, {"unknown", "owned-" + state}} {
			err := apikey.CheckPresetOwnership(ctx, pool, keys...)
			if !errors.Is(err, apikey.ErrOwnedPreset) {
				t.Fatalf("%s: want owned refusal in either position, got %v", state, err)
			}
			if strings.Contains(err.Error(), "owned-"+state) || strings.Contains(err.Error(), apikey.Hash("owned-"+state)) {
				t.Fatal("refusal disclosed value or digest")
			}
		}
		if err := apikey.CheckPresetOwnership(ctx, pool, "", "unowned-"+state, "unknown"); err != nil {
			t.Fatalf("%s: unowned/unknown candidate refused: %v", state, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := apikey.CheckPresetOwnership(cancelled, pool, "unknown"); !errors.Is(err, context.Canceled) {
		t.Fatalf("database failure became success/owned refusal: %v", err)
	}
	pool.Close()
	if err := apikey.CheckPresetOwnership(ctx, pool, "unknown"); err == nil || errors.Is(err, apikey.ErrOwnedPreset) {
		t.Fatalf("closed database error not propagated: %v", err)
	}
	if err := apikey.CheckPresetOwnership(ctx, pool, "", ""); err != nil {
		t.Fatalf("empty values queried the database: %v", err)
	}
}
