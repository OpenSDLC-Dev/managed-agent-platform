package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Observe PostgreSQL's actual list result count, not SQL predicate spelling or
// elapsed time. Authentication queries have no ORDER BY. Never retain arguments.
type managementListTrace struct {
	mu      sync.Mutex
	results []pgx.TraceQueryEndData
}

type managementListTraceKey struct{}

func (*managementListTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM api_keys") && strings.Contains(d.SQL, "ORDER BY") {
		return context.WithValue(ctx, managementListTraceKey{}, true)
	}
	return ctx
}

func (l *managementListTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if ctx.Value(managementListTraceKey{}) != true {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, d)
}

func (l *managementListTrace) take() []pgx.TraceQueryEndData {
	l.mu.Lock()
	defer l.mu.Unlock()
	results := l.results
	l.results = nil
	return results
}

// Returning all rows and filtering in Go preserves the HTTP body but must fail
// the database row-count assertions, including when the developer owns no key.
func TestOwnedKeyListRestrictsDatabaseRows(t *testing.T) {
	s, developer, legacy := historicalKeyServer(t)
	admin := seedMember(t, s, "list-admin", "admin", "console")
	empty := seedMember(t, s, "list-empty", "developer", "console")
	other := seedMember(t, s, "list-other", "developer", "console")
	devHeaders, adminHeaders := ownedCookie(t, s, developer), ownedCookie(t, s, admin)
	owned := map[string]string{}
	for _, status := range []string{"active", "inactive", "archived", "expired"} {
		_, id := issueOwnedHTTP(t, s, devHeaders, status, nil)
		owned[id] = status
		if status == "expired" {
			if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET expires_at='2000-01-01' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2 WHERE id=$1`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	// An adversarial storage row separates owner from creator. The old unowned
	// human-issued row above tests the converse; neither can infer ownership.
	_, differentCreator := ownedKeyFixture(t, s.pool, developer, admin)
	owned[differentCreator] = "active"
	_, otherKey := issueOwnedHTTP(t, s, ownedCookie(t, s, other), "other", nil)
	sa := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
	alias := principal.PublicID(sa, "service_account")
	_, serviceKey := issueOwnedHTTP(t, s, adminHeaders, "service", &alias)
	var preset string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM api_keys WHERE created_by IS NULL`).Scan(&preset); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{legacy: "active", preset: "active", otherKey: "active", serviceKey: "active"}
	for id, status := range owned {
		all[id] = status
	}
	// Equal timestamps exercise the ID tie-breaker on both filtered and full lists.
	if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET created_at='2020-01-01'`); err != nil {
		t.Fatal(err)
	}
	var trace managementListTrace
	cfg := s.pool.Config()
	cfg.ConnConfig.Tracer = &trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil, api.WithLogin(s.manager, nil)))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	legacyHeaders := devHeaders.Clone()
	legacyHeaders.Set("X-Api-Key", testKey)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    map[string]string
		rows    int64
	}{
		{"zero-owned", ownedCookie(t, s, empty), map[string]string{}, 0},
		{"owned", devHeaders, owned, 5},
		{"admin", adminHeaders, all, 9},
		{"selected-legacy", legacyHeaders, all, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.take()
			raw := expectManagement(t, s, "GET", consoleAPIKeysPath, "", tc.headers, 200)
			var rows []struct {
				ID        string `json:"id"`
				Status    string `json:"status"`
				CanManage bool   `json:"can_manage"`
			}
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
			got, ids := map[string]string{}, []string{}
			for _, row := range rows {
				got[row.ID] = row.Status
				ids = append(ids, row.ID)
				if row.CanManage != (row.ID != preset) {
					t.Errorf("row %s can_manage=%v", row.ID, row.CanManage)
				}
			}
			if !reflect.DeepEqual(got, tc.want) || int64(len(rows)) != tc.rows {
				t.Errorf("HTTP list=%v want=%v (%d rows)", got, tc.want, tc.rows)
			}
			if !slices.IsSortedFunc(ids, func(a, b string) int { return strings.Compare(b, a) }) {
				t.Error("equal-time keys are not in descending ID order")
			}
			results := trace.take()
			if len(results) != 1 {
				t.Fatalf("observed %d list queries, want one", len(results))
			}
			result := results[0]
			if result.Err != nil {
				t.Fatalf("list query failed: %v", result.Err)
			}
			t.Logf("PostgreSQL list rows=%d; HTTP rows=%d", result.CommandTag.RowsAffected(), len(rows))
			if result.CommandTag.RowsAffected() != tc.rows {
				t.Errorf("PostgreSQL returned %d key rows, want %d; owner restriction must precede projection and decoding", result.CommandTag.RowsAffected(), tc.rows)
			}
		})
	}
}
