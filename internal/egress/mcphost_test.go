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

func TestMCPEndpointHost(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://mcp.example/mcp", "mcp.example"},
		{"http://mcp.example:8443/mcp", "mcp.example"},
		{"https://[fd00::1]:8443/mcp", "fd00::1"},
		{"ftp://mcp.example/", ""},
		{"https:///mcp", ""},
		// An authority with a port and no host: u.Host is ":443".
		{"https://:443/mcp", ""},
		{"not a url", ""},
	} {
		got, err := egress.MCPEndpointHost(tc.url)
		if (err != nil) != (tc.want == "") || got != tc.want {
			t.Errorf("MCPEndpointHost(%q) = %q, %v; want %q", tc.url, got, err, tc.want)
		}
	}
}
