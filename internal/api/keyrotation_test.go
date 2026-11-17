package api_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// countLive reports how many unrevoked rows the query finds, failing the test on
// a query error so callers read as assertions.
func countLive(t *testing.T, pool *pgxpool.Pool, query string, arg any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, arg).Scan(&n); err != nil {
		t.Fatalf("count live keys: %v", err)
	}
	return n
}

// raceMints runs mint concurrently from a standing start and returns how many
// calls succeeded, failing the test only if none did — a credential path where
// *every* caller fails is broken whatever its concurrency contract.
//
// How many *should* succeed is the caller's to assert, and the callers disagree
// on purpose. Two require every racer to succeed and say so:
// TestConcurrentEnvironmentKeyIssuesAllSurvive (per-host keys — provisioning a
// fleet must not drop a host, which is what migration 0021's retirement of
// environment_keys_one_live bought) and TestConcurrentSameValueAPIKeyMintsAllSucceed
// (replicas booting with one shared bootstrap value converge on one row).
