package api

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

func TestCanManageAPIKey(t *testing.T) {
	owner, creator := "principal_owner", "principal_creator"
	for _, tc := range []struct {
		name   string
		key    ManagementKey
		caller identityPrincipal
		want   bool
	}{
		{"preset-machine", ManagementKey{}, identityPrincipal{}, false},
		{"preset-admin", ManagementKey{}, identityPrincipal{ID: owner, Role: identity.RoleAdmin}, false},
		{"machine-unowned", ManagementKey{CreatedBy: &creator}, identityPrincipal{}, true},
		{"admin-unowned", ManagementKey{CreatedBy: &creator}, identityPrincipal{ID: owner, Role: identity.RoleAdmin}, true},
		{"developer-owned", ManagementKey{CreatedBy: &creator, OwnerPrincipalID: &owner}, identityPrincipal{ID: owner, Role: identity.RoleDeveloper}, true},
		{"developer-creator", ManagementKey{CreatedBy: &creator, OwnerPrincipalID: &owner}, identityPrincipal{ID: creator, Role: identity.RoleDeveloper}, false},
		{"developer-legacy-creator", ManagementKey{CreatedBy: &owner}, identityPrincipal{ID: owner, Role: identity.RoleDeveloper}, false},
		{"user-owned", ManagementKey{CreatedBy: &creator, OwnerPrincipalID: &owner}, identityPrincipal{ID: owner, Role: identity.RoleUser}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canManageAPIKey(tc.key, tc.caller); got != tc.want {
				t.Errorf("can_manage=%v want %v", got, tc.want)
			}
		})
	}
}
