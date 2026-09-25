package egress_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// The policy's type decides, not the environment's kind: a self_hosted config
// with no networking has nothing to apply, a block found anywhere is a policy
// somebody meant, and one naming no recognized type admits nothing.
func TestMCPServerAdmitted(t *testing.T) {
	limited := func(allowMCP bool, hosts ...string) domain.Networking {
		return domain.Networking{Type: domain.NetLimited, AllowedHosts: hosts, AllowMCPServers: allowMCP}
	}
	for _, tc := range []struct {
		name string
		cfg  domain.EnvironmentConfig
		host string
		want bool
	}{
		{"self_hosted, no block", domain.EnvironmentConfig{Type: domain.EnvSelfHosted}, "mcp.example", true},
		{"self_hosted, a limited block", domain.EnvironmentConfig{Type: domain.EnvSelfHosted, Networking: limited(false)}, "mcp.example", false},
		{"unrestricted", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: domain.Networking{Type: domain.NetUnrestricted}}, "mcp.example", true},
		{"limited, the flag", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: limited(true)}, "mcp.example", true},
		{"limited, exact host", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: limited(false, "mcp.example")}, "mcp.example", true},
		{"limited, wildcard", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: limited(false, "*.example")}, "mcp.example", true},
		{"limited, wildcard apex", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: limited(false, "*.example")}, "example", false},
		{"limited, not listed", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: limited(false, "other.example")}, "mcp.example", false},
		{"cloud, no type", domain.EnvironmentConfig{Type: domain.EnvCloud}, "mcp.example", false},
		{"unrecognized type", domain.EnvironmentConfig{Type: domain.EnvCloud, Networking: domain.Networking{Type: "open"}}, "mcp.example", false},
	} {
		if got := egress.MCPServerAdmitted(tc.cfg, tc.host); got != tc.want {
			t.Errorf("%s: MCPServerAdmitted(%q) = %v, want %v", tc.name, tc.host, got, tc.want)
		}
	}
}
