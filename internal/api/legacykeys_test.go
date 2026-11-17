package api_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Historical human issuers cannot create new unowned rows after the cutover.
func TestHistoricalHumanIssuerCannotCreateUnownedKey(t *testing.T) {
	s, caller, _ := historicalKeyServer(t)
	before := ownedSnapshot(t, s)
	_, err := s.pool.Exec(t.Context(), `INSERT INTO api_keys(id,name,key_hash,created_by) VALUES ('apikey_forbidden','unowned','hash',$1)`, caller)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "api_keys_human_issuer_requires_owner" {
		t.Fatalf("legacy human issuer=%v, want cutover constraint refusal", err)
	}
	if before != ownedSnapshot(t, s) {
		t.Fatal("legacy helper inferred ownership or wrote an unowned human key")
	}
}
