// Package workspace describes the capabilities of a live workspace grant.
// Organization authority is separate, including on credentials bound to Default.
package workspace

import "github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"

const DefaultPublicID = "wrkspc_00000000000000000000000001"

type Role string

const (
	User             Role = "user"
	LimitedDeveloper Role = "limited_developer"
	Developer        Role = "developer"
	Admin            Role = "admin"
	Billing          Role = "billing"
)

func (r Role) CanUseResources() bool {
	return r == Developer || r == LimitedDeveloper || r == Admin
}

func (r Role) CanUsePlayground() bool { return r == User || r.CanUseResources() }

func (r Role) CanReadUsageSummary() bool { return r == Billing || r.CanUseResources() }

// ResourceRole adapts the existing resource handler minima without turning a
// workspace administrator into an organization administrator.
func (r Role) ResourceRole() identity.Role {
	if r == Admin {
		return identity.RoleAdmin
	}
	if r.CanUseResources() {
		return identity.RoleDeveloper
	}
	return identity.RoleNone
}
