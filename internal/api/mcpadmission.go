package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// admitMCPServers is the reference's create-time check on the hosts an agent
// declares MCP servers at (#571): a session whose environment's networking does
// not admit one is refused with a 400 and never exists, where the dial-time
// check alone would have created it to fail one work cycle at a time. The
// predicate is the executor's own (egress.MCPServerAdmitted), which still runs
// before every dial, because mcp_servers is mid-session-mutable.
//
// The message is the reference's, recorded for one server; several are joined
// with ", " in declaration order, which is ours (docs/DIVERGENCES.md). A url
// with no host to judge is left to the dial, which refuses it with its own
// reason — skipping it here admits nothing, since that server is never dialled.
// Only the session's own agent is checked: a coordinator's roster members dial
// under the executor's check alone.
func admitMCPServers(cfg domain.EnvironmentConfig, servers []json.RawMessage) error {
	var blocked []string
	for _, raw := range servers {
		var server struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if json.Unmarshal(raw, &server) != nil {
			continue
		}
		host, err := egress.MCPEndpointHost(server.URL)
		if err != nil {
			continue
		}
		if !egress.MCPServerAdmitted(cfg, host) {
			blocked = append(blocked, fmt.Sprintf("%q (%s)", server.Name, host))
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	return classified("mcp_egress_blocked_error", errInvalid(
		"MCP server host(s) blocked by environment network policy: %s. "+
			"Add these hosts to the environment's allowed_hosts, or set allow_mcp_servers=true.",
		strings.Join(blocked, ", ")))
}
