package apitest

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OwnedKey stages an active admin and an owned credential for component tests.
// It inserts migration 0060's immutable row shape and stores only the hash.
// Acceptance and eval stacks use SetupKey to exercise actual setup and issuance.
func OwnedKey(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	owner := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,kind,role)
		VALUES ($1,'component-fixture',$1,'user','admin')`, owner); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 32)
	rand.Read(body)
	raw := api.IssuedKeyPrefix + base64.RawURLEncoding.EncodeToString(body)
	if _, err := pool.Exec(t.Context(), `INSERT INTO api_keys(id,name,owned_key_hash,owner_principal_id,created_by)
		VALUES ($1,'component-fixture',$2,$3,$3)`, domain.NewID(domain.PrefixAPIKey).String(), apikey.Hash(raw), owner); err != nil {
		t.Fatal(err)
	}
	return raw
}
