package api_test

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/jackc/pgx/v5/pgconn"
)

// The legacy helper must never infer an owner from its creator argument.
func TestLegacyManagementIssuerRemainsUnowned(t *testing.T) {
	s, caller, _ := historicalKeyServer(t)
	before := ownedSnapshot(t, s)
	_, _, err := api.IssueManagementKey(t.Context(), s.pool, "not an ownership API", nil, caller)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "api_keys_human_issuer_requires_owner" {
		t.Fatalf("legacy human issuer=%v, want cutover constraint refusal", err)
	}
	if before != ownedSnapshot(t, s) {
		t.Fatal("legacy helper inferred ownership or wrote an unowned human key")
	}
}

// preIssuerServer is only for historical human-unowned fixtures. Each test
// applies the cutover after staging its old rows and before exercising behavior.
func preIssuerServer(t *testing.T) *tserver {
	t.Helper()
	pool := preIssuerPool(t)
	if err := api.EnsureAPIKey(t.Context(), pool, "test", testKey); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil))
	t.Cleanup(srv.Close)
	return &tserver{t: t, pool: pool, url: srv.URL}
}
