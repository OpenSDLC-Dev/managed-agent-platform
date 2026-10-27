package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

const pgRestrictViolation = "23001"

func TestAnAdminPermanentlyClosesBootstrap(t *testing.T) {
	for _, grant := range []string{"insert", "promotion"} {
		t.Run(grant, func(t *testing.T) {
			pool := open(t, pgtest.FreshDB(t))
			ctx := context.Background()
			role := "user"
			if grant == "insert" {
				role = "admin"
			}
			if _, err := pool.Exec(ctx, `INSERT INTO principals (id, issuer, subject, role)
				VALUES ('principal_first', 'https://idp.example', 'first', $1)`, role); err != nil {
				t.Fatal(err)
			}
			if grant == "promotion" {
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM install_bootstrap`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("before promotion: flag rows = %d, err = %v; want 0", n, err)
				}
				if _, err := pool.Exec(ctx, `UPDATE principals SET role = 'admin' WHERE id = 'principal_first'`); err != nil {
					t.Fatal(err)
				}
			}
			var first time.Time
			if err := pool.QueryRow(ctx, `SELECT first_admin_at FROM install_bootstrap`).Scan(&first); err != nil {
				t.Fatalf("admin %s did not record bootstrap: %v", grant, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE principals SET role = 'user', status = 'deactivated';
				INSERT INTO principals (id, issuer, subject, role) VALUES ('principal_next', 'https://idp.example', 'next', 'admin')`); err != nil {
				t.Fatal(err)
			}
			var kept time.Time
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*), min(first_admin_at) FROM install_bootstrap`).Scan(&n, &kept); err != nil || n != 1 || !kept.Equal(first) {
				t.Fatalf("bootstrap after demotion/deactivation and another admin = (%d, %v, %v), want original %v", n, kept, err, first)
			}
		})
	}
}

// Principals are never deleted (plan 69 decision 4): a principal's row is what
// keeps a removed person removed, so the database refuses every DELETE and
// TRUNCATE on the table. A retention job written to the DELETE that
// docs/self-hosted-security.md §8 used to recommend then fails loudly instead
// of undoing a removal. The scrub that replaces it, an UPDATE that clears the
// personal fields, still works.
func TestPrincipalsRefuseDeleteAndTruncate(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	// Last seen a year ago, so the retention DELETE below selects this row:
	// a refusal is then the only reason it survives.
	if _, err := pool.Exec(ctx, `
		INSERT INTO principals (id, issuer, subject, email, display_name, last_seen_at)
		VALUES ('principal_1', 'https://idp.example', 'user-1', 'ada@corp.example', 'Ada Lovelace',
		        now() - interval '1 year')`); err != nil {
		t.Fatalf("insert principal: %v", err)
	}

	for _, stmt := range []string{
		`DELETE FROM principals WHERE last_seen_at < now() - interval '180 days'`,
		`TRUNCATE principals`,
	} {
		_, err := pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgRestrictViolation {
			t.Errorf("%s => %v, want the refusal (SQLSTATE %s)", stmt, err, pgRestrictViolation)
			continue
		}
		if !strings.Contains(pgErr.Message, "docs/self-hosted-security.md") {
			t.Errorf("%s refused with %q, which does not point at the scrub", stmt, pgErr.Message)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM principals`).Scan(&n); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	if n != 1 {
		t.Errorf("principals rows = %d after the refused DELETE and TRUNCATE, want 1", n)
	}

	tag, err := pool.Exec(ctx, `UPDATE principals SET email = '', display_name = '' WHERE id = 'principal_1'`)
	if err != nil {
		t.Fatalf("scrub the personal fields: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("scrub updated %d rows, want 1", tag.RowsAffected())
	}
}

// Each column 0056 added takes only its listed values, and a service account
// is developer or admin, as in the reference. The first row breaks only the
// table constraint: each of its values is valid alone.
func TestPrincipalsCheckTheirColumns(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	for i, tc := range []struct {
		cols, vals string
		ok         bool
	}{
		{"kind, role", "'service_account', 'user'", false},
		{"kind, role", "'service_account', 'developer'", true},
		{"role", "'viewer'", false},
		{"role_source", "'scim'", false},
		{"status", "'archived'", false},
		{"kind", "'robot'", false},
	} {
		_, err := pool.Exec(context.Background(), fmt.Sprintf(`INSERT INTO principals (id, issuer, subject, %s)
			VALUES ('principal_%d', 'https://idp.example', 'user-%d', %s)`, tc.cols, i, i, tc.vals))
		var pgErr *pgconn.PgError
		switch {
		case tc.ok && err != nil:
			t.Errorf("(%s) = (%s) => %v, want it accepted", tc.cols, tc.vals, err)
		case !tc.ok && (!errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation):
			t.Errorf("(%s) = (%s) => %v, want check violation %s", tc.cols, tc.vals, err, pgCheckViolation)
		}
	}
}
