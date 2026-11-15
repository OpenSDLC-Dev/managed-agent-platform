package modelgateway_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

func (e *env) ownedKey(owner, kind, role, name string) string {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, `INSERT INTO principals (id,issuer,subject,kind,role)
		VALUES ($1,'gateway-owned-test',$1,$2,$3) ON CONFLICT (id) DO NOTHING`, owner, kind, role)
	e.must(err)
	key := "sk-map-api01-owned-" + name
	_, err = e.pool.Exec(e.ctx, `INSERT INTO api_keys (id,name,owner_principal_id,owned_key_hash,created_by)
		VALUES ($1,$1,$2,$3,$2)`, name, owner, apikey.Hash(key))
	e.must(err)
	return key
}

func TestOwnedUsageCombinesKeysByPrincipal(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	keys := []string{
		e.ownedKey("principal_human", "user", "developer", "key_one"),
		e.ownedKey("principal_human", "user", "developer", "key_two"),
	}
	e.start()
	for _, key := range keys {
		res, raw := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("inference: %d %s", res.StatusCode, raw)
		}
	}
	rows, more, err := e.s.ListUsage(e.ctx, store.UsageFilter{PrincipalID: "principal_human"}, 0, 10)
	e.must(err)
	if more || len(rows) != 2 || rows[0].PrincipalID != "principal_human" || rows[0].APIKeyID != "key_two" || rows[1].APIKeyID != "key_one" {
		t.Fatalf("principal ledger: %+v", rows)
	}
	today := time.Now().UTC()
	days, more, err := e.s.ListDailyUsage(e.ctx, store.UsageFilter{PrincipalID: "principal_human"}, today, today, 10)
	e.must(err)
	if more || len(days) != 1 || days[0].PrincipalID != "principal_human" || days[0].Requests != 2 || days[0].APIKeyID != "" {
		t.Fatalf("principal rollup: %+v", days)
	}
}

func TestOwnedKeyUsesCurrentOwnerRoleWithoutGrant(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m")
	e.alias("fast", target(d, 0))
	e.alias("slow", target(d, 0))
	human := e.ownedKey("principal_human", "user", "developer", "key_human")
	service := e.ownedKey("principal_service", "service_account", "admin", "key_service")
	e.start()
	for _, key := range []string{human, service} {
		for _, headers := range []map[string]string{{"x-api-key": key}, {"Authorization": "Bearer " + key}} {
			for _, prefix := range []string{"", "/anthropic", "/openai"} {
				res, raw := e.do("GET", prefix+"/v1/models", "", headers)
				if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "fast") || !strings.Contains(string(raw), "slow") {
					t.Fatalf("owned models: %d %s", res.StatusCode, raw)
				}
			}
			res, raw := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, headers)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("owned inference: %d %s", res.StatusCode, raw)
			}
		}
	}
	_, err := e.pool.Exec(e.ctx, `UPDATE principals SET role='user' WHERE id='principal_human'`)
	e.must(err)
	res, raw := e.do("GET", "/v1/models", "", map[string]string{"x-api-key": human})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("demoted owner: %d %s", res.StatusCode, raw)
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE principals SET role='admin' WHERE id='principal_human'`)
	e.must(err)
	res, raw = e.do("GET", "/v1/models", "", map[string]string{"x-api-key": human})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("promoted owner: %d %s", res.StatusCode, raw)
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE principals SET status='deactivated' WHERE id='principal_human'`)
	e.must(err)
	res, raw = e.do("GET", "/v1/models", "", map[string]string{"x-api-key": human})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed owner: %d %s", res.StatusCode, raw)
	}
}
