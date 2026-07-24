package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// attachVault inserts a vault, sets it as the session's only attached vault, and
// returns its id.
func (h *harness) attachVault(t *testing.T) string {
	t.Helper()
	id := domain.NewID("vlt").String()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO vaults (id, display_name) VALUES ($1, 'fixture')`, id); err != nil {
		t.Fatalf("insert vault: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET vault_ids = $2 WHERE id = $1`, h.sid.String(), []string{id}); err != nil {
		t.Fatalf("attach vault: %v", err)
	}
	return id
}

// addEnvCred inserts an environment_variable credential (active unless archived)
// with the given secret_name into a vault.
func (h *harness) addEnvCred(t *testing.T, vaultID, secretName string, archived bool) {
	t.Helper()
	id := domain.NewID("vcrd").String()
	auth := fmt.Sprintf(`{"type":"environment_variable","secret_name":%q,`+
		`"networking":{"type":"unrestricted"},"injection_location":{"body":true,"header":true}}`, secretName)
	archivedAt := "NULL"
	if archived {
		archivedAt = "now()"
	}
	if _, err := h.pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, cred_key, archived_at)
		             VALUES ($1, $2, 'environment_variable', $3::jsonb, $4, %s)`, archivedAt),
		id, vaultID, auth, "name:"+secretName); err != nil {
		t.Fatalf("insert env cred: %v", err)
	}
}

// TestProvisionInjectsVaultPlaceholders proves the sandbox is provisioned with
// one secret_name=placeholder env var per active, valid environment_variable
// credential of the session's attached vaults — and that a credential with an
// invalid env-var name (which would fault the provision) or an archived one is
// left out rather than injected.
func TestProvisionInjectsVaultPlaceholders(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	vaultID := h.attachVault(t)
	h.addEnvCred(t, vaultID, "API_KEY", false)  // active, valid → injected
	h.addEnvCred(t, vaultID, "bad-name", false) // invalid env-var name → skipped
	h.addEnvCred(t, vaultID, "OLD_KEY", true)   // archived → excluded

	h.suspend(t, writeUse("out.txt", "hi"))
	worked, err := h.exec.step(context.Background())
	if err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}

	env := h.prov.lastSpec.Env
	ph, ok := env["API_KEY"]
	if !ok {
		t.Fatalf("Spec.Env missing API_KEY; got %v", env)
	}
	if !strings.HasPrefix(ph, egress.PlaceholderPrefix) {
		t.Errorf("API_KEY = %q, want an %s… placeholder", ph, egress.PlaceholderPrefix)
	}
	if _, bad := env["bad-name"]; bad {
		t.Error("an invalid-named credential was injected; it must be skipped")
	}
	if _, old := env["OLD_KEY"]; old {
		t.Error("an archived credential was injected; it must be excluded")
	}
	if len(env) != 1 {
		t.Errorf("Spec.Env = %v, want exactly API_KEY", env)
	}
}

// TestProvisionNoVaultsLeavesEnvNil proves the common path is untouched: a
// session with no attached vaults provisions with no injected env.
func TestProvisionNoVaultsLeavesEnvNil(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)

	h.suspend(t, writeUse("out.txt", "hi"))
	if worked, err := h.exec.step(context.Background()); err != nil || !worked {
		t.Fatalf("step worked=%v err=%v", worked, err)
	}
	if env := h.prov.lastSpec.Env; env != nil {
		t.Errorf("Spec.Env = %v, want nil for a vault-less session", env)
	}
}
