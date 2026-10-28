package store_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestBrowserLoginSchema(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES ('principal_browser','urn:managed-agent-platform:local','local-user');
		INSERT INTO browser_sessions(token_hash,principal_id,expires_at) VALUES(repeat('a',64),'principal_browser',clock_timestamp()+interval '12 hours');
		INSERT INTO local_accounts(principal_id,password_hash) VALUES('principal_browser','malformed-but-bounded');
		INSERT INTO browser_login_attempts(state_hash,binding_hash,nonce,code_verifier,expires_at) VALUES(repeat('b',64),repeat('c',64),'nonce','verifier',clock_timestamp()+interval '10 minutes');
		INSERT INTO login_throttle(bucket_hash,window_start,attempts) VALUES(repeat('d',64),clock_timestamp(),1)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO browser_sessions(token_hash,principal_id,expires_at) VALUES(repeat('a',64),'principal_browser',clock_timestamp()+interval '12 hours')`,
		`INSERT INTO browser_sessions(token_hash,principal_id,expires_at) VALUES(repeat('e',64),'principal_absent',clock_timestamp()+interval '12 hours')`,
		`INSERT INTO browser_sessions(token_hash,principal_id,expires_at) VALUES('raw-token','principal_browser',clock_timestamp()+interval '12 hours')`,
		`INSERT INTO browser_sessions(token_hash,principal_id,expires_at) VALUES(repeat('f',64),'principal_browser',clock_timestamp()-interval '12 hours')`,
		`INSERT INTO local_accounts(principal_id,password_hash) VALUES('principal_browser','another-hash')`,
		`INSERT INTO local_accounts(principal_id,password_hash) VALUES('principal_absent','another-hash')`,
		`UPDATE local_accounts SET password_hash = repeat('x',1024)`,
		`INSERT INTO browser_login_attempts(state_hash,binding_hash,nonce,code_verifier,expires_at) VALUES(repeat('b',64),repeat('c',64),'nonce','verifier',clock_timestamp()+interval '10 minutes')`,
		`UPDATE browser_login_attempts SET binding_hash = 'raw-browser-binding'`,
		`UPDATE login_throttle SET attempts=0`,
		`UPDATE login_throttle SET bucket_hash='raw-peer-ip'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("credential schema accepted invalid row: %s", q)
		}
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version='0057_browser_login.sql'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("0057 applied %d times", version)
	}
}
