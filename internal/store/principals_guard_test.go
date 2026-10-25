package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

const pgRestrictViolation = "23001"

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
