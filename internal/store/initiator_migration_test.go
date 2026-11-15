package store_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInitiatorsPreserveCreatorsAcrossMigration(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0063_gateway_model_limits.sql"); err != nil {
		t.Fatal(err)
	}
	keyExec(t, pool, `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_owner','test','owner','developer')`)
	keyExec(t, pool, `INSERT INTO api_keys(id,name,owned_key_hash,owner_principal_id,created_by) VALUES ('apikey_owned','owned',repeat('ab',32),'principal_owner','principal_owner')`)
	cases := []struct{ actor, principal, key string }{
		{"principal_owner", "principal_owner", ""},
		{"apikey_owned", "principal_owner", ""},
		{"apikey_old", "", "apikey_old"},
		{"principal_gone", "", "principal_gone"},
	}
	var sessions []domain.ID
	for _, tc := range cases {
		sid, _ := pgtest.NewSession(t, pool, "cloud")
		keyExec(t, pool, `UPDATE sessions SET created_by=$2 WHERE id=$1`, sid, tc.actor)
		sessions = append(sessions, sid)
	}
	keyExec(t, pool, `INSERT INTO deployments(id,name,agent_id,agent_version,environment_id,created_by)
		SELECT 'depl_initiator','fixture',agent_id,agent_version,environment_id,'apikey_owned' FROM sessions WHERE id=$1`, sessions[0])
	scheduled, _ := pgtest.NewSession(t, pool, "cloud")
	keyExec(t, pool, `UPDATE sessions SET created_by=NULL,deployment_id='depl_initiator' WHERE id=$1`, scheduled)
	unresolved, _ := pgtest.NewSession(t, pool, "cloud")
	keyExec(t, pool, `INSERT INTO deployments(id,name,agent_id,agent_version,environment_id)
		SELECT 'depl_unresolved','fixture',agent_id,agent_version,environment_id FROM sessions WHERE id=$1`, unresolved)
	keyExec(t, pool, `UPDATE sessions SET created_by=NULL,deployment_id='depl_unresolved' WHERE id=$1`, unresolved)
	dreamSession, _ := pgtest.NewSession(t, pool, "cloud")
	keyExec(t, pool, `UPDATE sessions SET created_by=NULL WHERE id=$1`, dreamSession)
	keyExec(t, pool, `INSERT INTO dreams(id,status,inputs,input_memory_store_id,input_session_ids,model,output_behavior,created_by,session_id)
		VALUES ('drm_initiator','pending','[]','memstore_fixture','{}','{"id":"fixture"}','{"type":"create_new"}','apikey_owned',$1)`, dreamSession)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, err = conn.Conn().Prepare(ctx, "old_session", `INSERT INTO sessions(id,agent_id,agent_version,resolved_agent,environment_id,created_by,deployment_id)
		SELECT $1,agent_id,agent_version,resolved_agent,environment_id,nullif($2,''),nullif($3,'') FROM sessions WHERE id=$4`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	check := func(sid domain.ID, wantPrincipal, wantKey string, stored bool) {
		t.Helper()
		var principal, key string
		query := `SELECT coalesce(principal_id,''),coalesce(api_key_id,'') FROM session_initiators WHERE session_id=$1`
		if stored {
			query = `SELECT coalesce(initiator_principal_id,''),coalesce(initiator_api_key_id,'') FROM sessions WHERE id=$1`
		}
		if err := pool.QueryRow(ctx, query, sid).Scan(&principal, &key); err != nil {
			t.Fatal(err)
		}
		if principal != wantPrincipal || key != wantKey {
			t.Fatalf("%s initiator=(%q,%q), want (%q,%q)", sid, principal, key, wantPrincipal, wantKey)
		}
	}
	for i, tc := range cases {
		check(sessions[i], tc.principal, tc.key, true)
	}
	check(scheduled, "principal_owner", "", true)
	check(dreamSession, "principal_owner", "", true)
	check(unresolved, "", "", false)
	// Giving the deployment an initiator cannot authorize a historical session
	// whose missing creator could not be resolved during backfill.
	keyExec(t, pool, `UPDATE deployments SET initiator_principal_id='principal_owner' WHERE id='depl_unresolved'`)
	check(unresolved, "", "", false)
	for _, tc := range []struct{ actor, deployment, principal, key string }{
		{"apikey_owned", "", "principal_owner", ""},
		{"", "depl_initiator", "principal_owner", ""},
		{"", "depl_unresolved", "principal_owner", ""},
		{"apikey_old", "depl_initiator", "", "apikey_old"},
	} {
		sid := domain.NewID(domain.PrefixSession)
		if _, err := conn.Exec(ctx, "old_session", sid, tc.actor, tc.deployment, sessions[0]); err != nil {
			t.Fatal(err)
		}
		check(sid, tc.principal, tc.key, true)
	}
	// An old dream runner links after the insert; the claim reads the view.
	late := domain.NewID(domain.PrefixSession)
	if _, err := conn.Exec(ctx, "old_session", late, "", "", sessions[0]); err != nil {
		t.Fatal(err)
	}
	keyExec(t, pool, `UPDATE dreams SET session_id=$1 WHERE id='drm_initiator'`, late)
	check(late, "principal_owner", "", false)
	// Repointing the deployment must not move a session's recorded subject.
	keyExec(t, pool, `UPDATE deployments SET initiator_principal_id=NULL,initiator_api_key_id='other' WHERE id='depl_initiator'`)
	check(scheduled, "principal_owner", "", true)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	check(scheduled, "principal_owner", "", true)
}
