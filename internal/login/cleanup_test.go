package login

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Execute the cleanup statements from the issuing source, not copies of their
// predicates. Temporary LIKE tables retain the real column/index shape without
// credentials or principal FKs. A full index scan with a Filter is not enough.
func TestExpiredCleanupHasIndexRange(t *testing.T) {
	pool := loginPool(t)
	ctx := context.Background()
	pattern := regexp.MustCompile("DELETE FROM (login_throttle|browser_sessions|browser_login_attempts) WHERE [^;`]+")
	tested := 0
	for _, file := range []string{"throttle.go", "session.go", "oidc.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, match := range pattern.FindAllStringSubmatch(string(source), -1) {
			query, table := match[0], match[1]
			// ConsumeOIDC's parameterized one-attempt deletion is not global cleanup.
			if strings.Contains(query, "$1") {
				continue
			}
			tested++
			t.Run(fmt.Sprintf("%s/%d/%s", file, i, table), func(t *testing.T) {
				conn, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Release()
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := tx.Exec(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL) ON COMMIT DROP`); err != nil {
					t.Fatal(err)
				}
				column := "expires_at"
				var seed string
				switch table {
				case "login_throttle":
					column = "window_start"
					seed = `INSERT INTO login_throttle SELECT md5(i::text)||md5(i::text),statement_timestamp()+interval '1 hour',1 FROM generate_series(1,10000) i;
      INSERT INTO login_throttle VALUES(repeat('f',64),statement_timestamp()-interval '6 minutes',1)`
				case "browser_sessions":
					seed = `INSERT INTO browser_sessions SELECT md5(i::text)||md5(i::text),'principal_fixture',statement_timestamp(),statement_timestamp()+interval '12 hours' FROM generate_series(1,10000) i;
      INSERT INTO browser_sessions VALUES(repeat('f',64),'principal_fixture',statement_timestamp()-interval '13 hours',statement_timestamp()-interval '1 hour')`
				case "browser_login_attempts":
					seed = `INSERT INTO browser_login_attempts SELECT md5(i::text)||md5(i::text),repeat('a',64),'nonce','verifier',statement_timestamp(),statement_timestamp()+interval '10 minutes' FROM generate_series(1,10000) i;
      INSERT INTO browser_login_attempts VALUES(repeat('f',64),repeat('a',64),'nonce','verifier',statement_timestamp()-interval '11 minutes',statement_timestamp()-interval '1 minute')`
				}
				if _, err := tx.Exec(ctx, seed+`; ANALYZE `+table+`; SET LOCAL enable_seqscan=off`); err != nil {
					t.Fatal(err)
				}
				var plan string
				if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+query).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(plan, `"Index Cond"`) || !strings.Contains(plan, column) {
					t.Errorf("cleanup lacks B-tree range condition: %s\n%s", query, plan)
				} else {
					t.Logf("indexable cleanup: %s\n%s", query, plan)
				}
				tag, err := tx.Exec(ctx, query)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("expired-only delete: %v err=%v", tag, err)
				}
				var remaining int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&remaining); err != nil || remaining != 10000 {
					t.Fatalf("unexpired rows=%d err=%v", remaining, err)
				}
			})
		}
	}
	if tested != 4 {
		t.Fatalf("exercised %d cleanup statements, want throttle + session + two OIDC pruners", tested)
	}
}
