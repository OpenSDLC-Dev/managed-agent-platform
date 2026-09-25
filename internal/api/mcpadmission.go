package api

import (
	"encoding/json"
	"fmt"
	"net/url"
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
// with ", " in declaration order, which is ours (docs/DIVERGENCES.md). The
// reference extracts the host from each url, so the host is judged whatever the
// scheme: whether the dial can use the url is the dial's question. A url with no
// host to judge is left to the dial, which refuses it as unusable — skipping it
// here admits nothing. Only the session's own agent is checked: a
// coordinator's roster members dial under the executor's check alone.
//
// Only a `limited` policy refuses here, because it is the only one the
// message's advice can fix. unrestricted and a self_hosted config with no
// networking admit everything; a type nothing recognizes — a row the API cannot
// have written — admits nothing at the dial, whose refusal says why, where this
// message would send its owner to a list and a flag that are not consulted.
//
// It reads only the two fields it judges by, and only when a server is declared,
// so a stored config some other field of which will not decode — a tolerated
// corrupt row — creates sessions as it did before the check existed. A type or
// networking block that will not decode is not judged either: the executor
// cannot decode that row, so such a session fails its first work item, as it
// did before, rather than every create on the environment becoming a 500.
func admitMCPServers(config []byte, servers []json.RawMessage) error {
	if len(servers) == 0 {
		return nil
	}
	var judged struct {
		Type       domain.EnvironmentKind `json:"type"`
		Networking domain.Networking      `json:"networking"`
	}
	if json.Unmarshal(config, &judged) != nil || judged.Networking.Type != domain.NetLimited {
		return nil
	}
	cfg := domain.EnvironmentConfig{Type: judged.Type, Networking: judged.Networking}
	var blocked []string
	for _, raw := range servers {
		var server struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if json.Unmarshal(raw, &server) != nil {
			continue
		}
		u, err := url.Parse(server.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := u.Hostname()
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
