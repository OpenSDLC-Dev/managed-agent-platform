package store_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDefaultWorkspaceUpgrade(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0070_deployment_price_metadata.sql"); err != nil {
		t.Fatal(err)
	}
	keyExec(t, pool, `INSERT INTO principals(id,issuer,subject,kind,role,status,playground_access)
		VALUES ('admin','test','a','user','admin','active',true),
		       ('dev','test','d','user','developer','active',false),
		       ('approved','test','p','user','user','active',true),
		       ('setup','test','u','user','user','active',false),
		       ('removed','test','r','user','developer','deactivated',true),
		       ('service','test','s','service_account','developer','active',false),
		       ('archived','test','x','service_account','developer','deactivated',false)`)
	adminSession, _ := pgtest.NewSession(t, pool, "cloud")
	keyExec(t, pool, `UPDATE sessions SET initiator_principal_id='admin',initiator_api_key_id=NULL WHERE id=$1`, adminSession)
	token := clioauth.AccessPrefix + strings.Repeat("x", 43)
	keyExec(t, pool, `WITH grant_row AS (
		INSERT INTO cli_oauth_grants(principal_id,client_id,scopes,challenge,state_hash,redirect_uri,expires_at)
		VALUES ('dev','client',ARRAY['user:developer'],'challenge','state','redirect',now()+interval '1 day')
		RETURNING id)
		INSERT INTO cli_oauth_tokens(grant_id,access_hash,refresh_hash,access_expires_at)
		SELECT id,$1,'refresh',now()+interval '1 hour' FROM grant_row`, apikey.Hash(token))
	keyExec(t, pool, `INSERT INTO api_keys(id,name,created_by,owner_principal_id,owned_key_hash)
		VALUES ('owned','key','admin','service','owned-hash')`)
	// Read the previous release's policy while the database still has that schema.
	rows, err := pool.Query(ctx, `SELECT id, CASE WHEN status<>'active' THEN ''
		WHEN role IN ('admin','developer') THEN role WHEN playground_access THEN 'user' ELSE '' END
		FROM principals`)
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]string)
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			t.Fatal(err)
		}
		before[id] = role
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err := store.MigrateThrough(ctx, pool, "0071_default_workspace.sql"); err != nil {
		t.Fatal(err)
	}
	var publicID string
	if err := pool.QueryRow(ctx, `SELECT public_id FROM workspaces WHERE id='default' AND archived_at IS NULL`).Scan(&publicID); err != nil || publicID != "wrkspc_00000000000000000000000001" {
		t.Fatalf("Default public ID %q: %v", publicID, err)
	}
	check := func(id, want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT coalesce(workspace_role($1,'default'),'')`, id).Scan(&got); err != nil || got != want {
			t.Errorf("%s: role %q, want %q: %v", id, got, want, err)
		}
	}
	for id, want := range before {
		check(id, want)
	}
	cli, err := clioauth.Authenticate(ctx, pool, token)
	if err != nil || cli.PrincipalID != "dev" || cli.WorkspaceID != publicID || !cli.WorkspaceRole.CanUseResources() {
		t.Fatalf("pre-upgrade CLI grant lost its authority: %v, %v", cli, err)
	}
	if got := keySnapshot(t, pool, `SELECT concat_ws('/',id,created_by,owner_principal_id,workspace_id,owned_key_hash)
		FROM api_keys WHERE id='owned'`); got != "owned/admin/service/default/owned-hash" {
		t.Fatalf("changed key attribution or binding: %s", got)
	}
	keyExec(t, pool, `INSERT INTO principals(id,issuer,subject,role) VALUES ('old','test','old','developer')`)
	check("old", "developer")
	keyExec(t, pool, `INSERT INTO principals(id,issuer,subject,role,workspace_grants_initialized)
		VALUES ('new','test','new','developer',true)`)
	check("new", "")
	keyExec(t, pool, `UPDATE principals SET playground_access=true WHERE id='setup'`)
	check("setup", "user")
	keyExec(t, pool, `UPDATE principals SET playground_access=false WHERE id='setup'`)
	check("setup", "")
	keyExec(t, pool, `DELETE FROM workspace_memberships WHERE principal_id='dev'`)
	keyExec(t, pool, `SELECT backfill_default_workspace_grants()`)
	check("dev", "")
	keyExec(t, pool, `UPDATE principals SET role='billing' WHERE id='approved'`)
	check("approved", "billing")
	keyExec(t, pool, `UPDATE workspace_memberships SET role='admin' WHERE principal_id='approved'`)
	check("approved", "admin")
	keyExec(t, pool, `UPDATE principals SET role='user' WHERE id='approved'`)
	check("approved", "admin")
	keyExec(t, pool, `UPDATE principals SET playground_access=false WHERE id='admin'`)
	check("admin", "admin")
	var canContinue bool
	if err := pool.QueryRow(ctx, `SELECT workspace_can_continue(initiator_principal_id,workspace_id,workspace_grant_generation)
		FROM sessions WHERE id=$1`, adminSession).Scan(&canContinue); err != nil || !canContinue {
		t.Fatalf("clearing a redundant Playground grant interrupted the organization admin's session: %t, %v", canContinue, err)
	}
	keyExec(t, pool, `UPDATE principals SET role='user' WHERE id='admin'`)
	check("admin", "")
}

func TestWorkspaceContinuationAfterUpgrade(t *testing.T) {
	ctx := t.Context()
	pool := rawPool(t, pgtest.FreshDB(t))
	if err := store.MigrateThrough(ctx, pool, "0070_deployment_price_metadata.sql"); err != nil {
		t.Fatal(err)
	}
	sid, _ := pgtest.NewSession(t, pool, "cloud")
	keyExec(t, pool, `INSERT INTO principals(id,issuer,subject,role) VALUES ('former-dev','test','old','user')`)
	keyExec(t, pool, `UPDATE sessions SET initiator_principal_id='former-dev',initiator_api_key_id=NULL WHERE id=$1`, sid)
	keyExec(t, pool, `INSERT INTO deployments(id,name,agent_id,agent_version,environment_id,created_by)
		SELECT 'deployment','old deployment',agent_id,agent_version,environment_id,'former-dev' FROM sessions WHERE id=$1`, sid)
	keyExec(t, pool, `INSERT INTO dreams(id,status,inputs,input_memory_store_id,input_session_ids,model,output_behavior,created_by)
		VALUES ('dream','pending','[]','memstore_test','{}','{"id":"m"}','{"type":"create_new"}','former-dev')`)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	allowed := func(id string, want bool) {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT workspace_can_continue(i.principal_id,s.workspace_id,s.workspace_grant_generation)
			FROM sessions s JOIN session_initiators i ON i.session_id=s.id WHERE s.id=$1`, id).Scan(&ok); err != nil || ok != want {
			t.Errorf("session %s allowed=%t want=%t: %v", id, ok, want, err)
		}
	}
	if got := keySnapshot(t, pool, `SELECT coalesce(workspace_role('former-dev','default'),'none')`); got != "none" {
		t.Fatalf("legacy continuation granted new-work privileges: %s", got)
	}
	const insert = `INSERT INTO sessions(id,agent_id,agent_version,resolved_agent,environment_id,created_by,deployment_id)
		SELECT $1,agent_id,agent_version,resolved_agent,environment_id,
			CASE WHEN $2::text IS NULL THEN 'former-dev' END,$2 FROM sessions WHERE id=$3`
	keyExec(t, pool, insert, "unrelated", nil, sid)
	keyExec(t, pool, insert, "scheduled", "deployment", sid)
	keyExec(t, pool, insert, "pipeline", nil, sid)
	keyExec(t, pool, `UPDATE dreams SET session_id='pipeline' WHERE id='dream'`)
	allowed("unrelated", false)
	for _, id := range []string{string(sid), "scheduled", "pipeline"} {
		allowed(id, true)
	}
	keyExec(t, pool, `SELECT revoke_workspace_runs('former-dev','default')`)
	keyExec(t, pool, `INSERT INTO workspace_memberships VALUES ('default','former-dev','developer')`)
	keyExec(t, pool, insert, "new", nil, sid)
	keyExec(t, pool, insert, "revoked-parent", "deployment", sid)
	allowed("new", true)
	for _, id := range []string{string(sid), "scheduled", "pipeline", "revoked-parent"} {
		allowed(id, false)
	}
}
