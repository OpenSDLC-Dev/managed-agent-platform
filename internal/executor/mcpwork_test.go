package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// mcpHarness is the executor harness with the MCP client pointed at a transport
// that can reach loopback — the production client's dial guard refuses it, which
// is the whole reason a fixture needs its own.
func mcpHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{})
	h.exec.mcpHTTP = mcptest.Client()
	return h
}

// declareMCPServers writes the agent's mcp_servers array, the shape
// {type: "url", name, url} the wire pins.
func (h *harness) declareMCPServers(t *testing.T, servers ...[2]string) {
	t.Helper()
	entries := make([]map[string]string, len(servers))
	for i, s := range servers {
		entries[i] = map[string]string{"type": "url", "name": s[0], "url": s[1]}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{mcp_servers}', $2::jsonb) WHERE id = $1`,
		h.sid.String(), raw); err != nil {
		t.Fatalf("set session mcp_servers: %v", err)
	}
}

// setNetworking rewrites the session's environment config networking block.
func (h *harness) setNetworking(t *testing.T, net domain.Networking) {
	t.Helper()
	raw, err := json.Marshal(net)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET config = jsonb_set(config, '{networking}', $2::jsonb) WHERE id = $1`,
		h.envID.String(), raw); err != nil {
		t.Fatalf("set environment networking: %v", err)
	}
}

func (h *harness) enqueueMCP(t *testing.T) {
	t.Helper()
	if _, err := h.queue.Enqueue(context.Background(), h.pool, h.envID, h.sid, queue.MCPExec); err != nil {
		t.Fatalf("enqueue mcp_exec: %v", err)
	}
}

type catalogEntry struct {
	server string
	url    string
	status string
	reason string
	tools  []mcp.Tool
}

func (h *harness) catalog(t *testing.T) map[string]catalogEntry {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT server_name, url, status, coalesce(error, ''), tools FROM mcp_catalogs
		  WHERE session_id = $1 ORDER BY server_name`, h.sid.String())
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	defer rows.Close()
	out := map[string]catalogEntry{}
	for rows.Next() {
		var e catalogEntry
		var toolsJSON []byte
		if err := rows.Scan(&e.server, &e.url, &e.status, &e.reason, &toolsJSON); err != nil {
			t.Fatalf("scan catalog: %v", err)
		}
		if err := json.Unmarshal(toolsJSON, &e.tools); err != nil {
			t.Fatalf("decode catalog tools: %v", err)
		}
		out[e.server] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	return out
}

// TestDiscoveryWritesTheCatalogAndWakesTheBrain is the driver's happy path: an
// mcp_exec item reaches every declared server, records what it offers, and
// chains the model turn the discovery was for. The tools land in the Anthropic
// tool-definition field names, so request assembly needs no second translation.
func TestDiscoveryWritesTheCatalogAndWakesTheBrain(t *testing.T) {
	url := mcptest.Server(t,
		mcptest.Tool{Name: "search_issues", Description: "searches issues"},
		mcptest.Tool{Name: "create_issue", Description: "opens an issue"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	cat := h.catalog(t)
	got, ok := cat["github"]
	if !ok {
		t.Fatalf("no catalog row for the declared server; have %v", cat)
	}
	if got.status != "ready" || got.reason != "" {
		t.Errorf("row = %+v, want a ready row with no error", got)
	}
	if got.url != url {
		t.Errorf("row url = %q, want the endpoint discovery used (%q)", got.url, url)
	}
	names := []string{}
	for _, tool := range got.tools {
		names = append(names, tool.Name)
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q stored without an input schema", tool.Name)
		}
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), "search_issues") {
		t.Errorf("stored tools = %v, want both of the server's", names)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the chained turn", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d, want the item completed", n)
	}
}

// TestAnUnreachableServerIsRecordedAndTheTurnStillRuns pins that a server being
// down is a fact about that server, not a fault of the work item: it lands as a
// failed row, the session proceeds without those tools, and the item completes
// rather than reclaim-looping against an endpoint that is simply gone. The
// reference documents exactly this — the session keeps running, and the
// connection is retried on the next idle-to-running transition, which the failed
// row is what makes possible.
func TestAnUnreachableServerIsRecordedAndTheTurnStillRuns(t *testing.T) {
	up := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	// Port 1 on loopback: nothing listens, and the connection is refused
	// rather than left hanging.
	h.declareMCPServers(t, [2]string{"down", "http://127.0.0.1:1/mcp"}, [2]string{"up", up})
	h.enqueueMCP(t)

	h.stepOnce(t)

	cat := h.catalog(t)
	if got := cat["down"]; got.status != "failed" || got.reason == "" {
		t.Errorf("unreachable server row = %+v, want a failed row carrying a reason", got)
	}
	if got := cat["up"]; got.status != "ready" || len(got.tools) != 1 {
		t.Errorf("reachable server row = %+v, want its listing regardless of the other's failure", got)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the turn chained despite the failure", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d, want the item completed rather than retried in place", n)
	}
}

// TestLimitedNetworkingRefusesAnMCPServerItDoesNotName pins the executor-side
// egress check. The platform dials the MCP server from this process, outside the
// per-session gate, so `limited` is enforced here or nowhere — and it must be
// enforced before the dial, not after: a refusal that still opened the
// connection would have already told the server the session exists.
func TestLimitedNetworkingRefusesAnMCPServerItDoesNotName(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	h.setNetworking(t, domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"elsewhere.test"}})
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "failed" {
		t.Fatalf("row = %+v, want the dial refused by the networking policy", got)
	}
	if !strings.Contains(got.reason, "networking policy") {
		t.Errorf("reason = %q, want it to name the policy that refused", got.reason)
	}
	if len(got.tools) != 0 {
		t.Errorf("tools = %v, want none: the server was never reached", got.tools)
	}
}

// TestLimitedNetworkingAdmitsMCPServersWhenTheFlagIsSet covers the field that
// exists for exactly this: allow_mcp_servers admits "MCP server endpoints
// configured on the agent" on top of allowed_hosts, so a session that names none
// of them still reaches the ones its agent declares.
func TestLimitedNetworkingAdmitsMCPServersWhenTheFlagIsSet(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	h.setNetworking(t, domain.Networking{Type: domain.NetLimited, AllowMCPServers: true})
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := h.catalog(t)["github"]; got.status != "ready" {
		t.Errorf("row = %+v, want the dial admitted by allow_mcp_servers", got)
	}
}

// TestLimitedNetworkingAdmitsAHostItNames is the third arm: a session that lists
// the MCP server's own host reaches it without the flag.
func TestLimitedNetworkingAdmitsAHostItNames(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	h.setNetworking(t, domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"127.0.0.1"}})
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := h.catalog(t)["github"]; got.status != "ready" {
		t.Errorf("row = %+v, want the dial admitted by allowed_hosts", got)
	}
}

// TestDiscoverySkipsAServerAlreadyInTheCatalog pins the retry state the row
// carries. A ready row is a listing the brain can use, so a second pass must not
// spend a round trip re-fetching it; a failed row is an attempt that did not
// work, and the reference retries on the next idle-to-running transition, so
// that one is re-attempted. Both are asserted on the same session so neither
// reading can pass by accident.
func TestDiscoverySkipsAServerAlreadyInTheCatalog(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"ready", url}, [2]string{"stale", url})
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status, error)
		 VALUES ($1, 'ready', $2, '[{"name":"remembered","input_schema":{"type":"object"}}]'::jsonb, 'ready', NULL),
		        ($1, 'stale', $2, '[]'::jsonb, 'failed', 'connection refused')`,
		h.sid.String(), url); err != nil {
		t.Fatal(err)
	}
	h.enqueueMCP(t)

	h.stepOnce(t)

	cat := h.catalog(t)
	if got := cat["ready"]; len(got.tools) != 1 || got.tools[0].Name != "remembered" {
		t.Errorf("ready row = %+v, want it left as it was rather than re-fetched", got)
	}
	if got := cat["stale"]; got.status != "ready" || got.reason != "" || len(got.tools) != 1 {
		t.Errorf("failed row = %+v, want it re-attempted and healed", got)
	}
}

// TestDiscoveryRefetchesAServerThatMoved pins why the url is compared and not
// only the name: a mid-session agent patch may repoint a server, and a listing
// attributed to the wrong endpoint would reach the model as tools that are not
// there. The session patch deletes the rows it invalidates, so this is the
// belt to that braces — a row that outlived its endpoint by any route is
// re-fetched rather than trusted.
func TestDiscoveryRefetchesAServerThatMoved(t *testing.T) {
	moved := mcptest.Server(t, mcptest.Tool{Name: "new_tool"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", moved})
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
		 VALUES ($1, 'github', 'https://old.test/mcp',
		         '[{"name":"old_tool","input_schema":{"type":"object"}}]'::jsonb, 'ready')`,
		h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.url != moved || len(got.tools) != 1 || got.tools[0].Name != "new_tool" {
		t.Errorf("row = %+v, want the listing re-fetched from the endpoint the agent now names", got)
	}
}

// TestAServerWithoutAUsableURLIsRecordedNotDialed pins that a malformed
// endpoint is a failed row rather than a fault: a stored spec predating any
// validation must not wedge a session's every turn on a reclaim loop.
func TestAServerWithoutAUsableURLIsRecordedNotDialed(t *testing.T) {
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"bad", "file:///etc/passwd"})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["bad"]
	if got.status != "failed" || !strings.Contains(got.reason, "http or https") {
		t.Errorf("row = %+v, want a failed row naming the unusable scheme", got)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d, want the item completed", n)
	}
}

// TestDiscoveryDrainsAnItemForADeadSession mirrors the other drivers: work for a
// session that is no longer running is completed with nothing written, so a
// finished session cannot reclaim-loop an item forever.
func TestDiscoveryDrainsAnItemForADeadSession(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'terminated' WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}

	h.stepOnce(t)

	if cat := h.catalog(t); len(cat) != 0 {
		t.Errorf("catalog = %v, want nothing written for a session that is not running", cat)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d, want the item drained", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn items = %d, want no turn chained for a dead session", n)
	}
}
