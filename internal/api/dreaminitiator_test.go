package api_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
)

func TestDreamSessionRecordsInitiator(t *testing.T) {
	s := newTestServer(t)
	storeID, sessions := createDreamInputs(t, s, 1)
	dreamID, sessionID := startedDream(t, s, dreamBody(storeID, sessions))
	var dreamKey, sessionKey *string
	if err := s.pool.QueryRow(t.Context(), `SELECT d.initiator_api_key_id,s.initiator_api_key_id FROM dreams d JOIN sessions s ON s.id=d.session_id WHERE d.id=$1 AND s.id=$2`, dreamID, sessionID).Scan(&dreamKey, &sessionKey); err != nil {
		t.Fatal(err)
	}
	if dreamKey == nil || sessionKey == nil || *dreamKey != *sessionKey {
		t.Fatalf("dream initiator=%v session initiator=%v", dreamKey, sessionKey)
	}
}

func TestRemovedDreamInitiatorFailsThroughSession(t *testing.T) {
	s := newTestServer(t)
	storeID, sessions := createDreamInputs(t, s, 1)
	dreamID := createDream(t, s, dreamBody(storeID, sessions))["id"].(string)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,status) VALUES ('principal_dream','test','dream','deactivated');
		UPDATE dreams SET initiator_principal_id='principal_dream',initiator_api_key_id=NULL`); err != nil {
		t.Fatal(err)
	}
	tick(t, s)
	d := getDream(t, s, dreamID)
	if d["status"] != "running" {
		t.Fatalf("dream did not start: %v", d)
	}
	b := brain.New(s.pool, nil, s.blobs, brain.Config{})
	if found, err := b.RunOnce(t.Context()); err != nil || !found {
		t.Fatalf("brain found=%v err=%v", found, err)
	}
	tick(t, s)
	d = getDream(t, s, dreamID)
	typ, _ := dreamError(t, d)
	if d["status"] != "failed" || typ != "internal_error" {
		t.Fatalf("dream=%v", d)
	}
}
