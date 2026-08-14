package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// mcpHarness is the executor harness with the MCP client pointed at a transport
// that can reach loopback — the production client's dial guard refuses it, which
// is the whole reason a fixture needs its own.
//
// It also writes the networking block, because the shared fixture does not. A
// cloud environment created through the REST surface always carries one — the
// config normalizer defaults it to `{"type":"unrestricted"}` — but pgtest
// inserts its environment row directly, so it lands as bare `{"type":"cloud"}`,
// which is a shape no live cloud environment has. Since the egress check refuses
// a cloud environment naming no policy it recognizes, a fixture left as pgtest
// builds it would have every test here failing on a shape production never
// produces.
func mcpHarness(t *testing.T) *harness {
	t.Helper()
	return mcpHarnessWith(t, Config{})
}

// mcpHarnessWith is the same fixture for a test that needs a configured budget.
func mcpHarnessWith(t *testing.T, cfg Config) *harness {
	t.Helper()
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, cfg)
	h.prov = prov
	h.exec.mcpHTTP = mcptest.Client()
	h.setNetworking(t, domain.Networking{Type: domain.NetUnrestricted})
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

// declareListedMCPServers is declareMCPServers plus the catalog row discovery
// would have written for each: the state every call-path test starts from,
// because a model is only ever offered an MCP tool that some listing published,
// and the driver dials a call only where that listing was read.
func (h *harness) declareListedMCPServers(t *testing.T, servers ...[2]string) {
	t.Helper()
	h.declareMCPServers(t, servers...)
	for _, s := range servers {
		h.listMCPServer(t, s[0], s[1])
	}
}

func (h *harness) listMCPServer(t *testing.T, server, url string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, status)
		 VALUES ($1, $2, $3, 'ready')
		 ON CONFLICT (session_id, server_name) DO UPDATE SET url = EXCLUDED.url, status = 'ready'`,
		h.sid.String(), server, url); err != nil {
		t.Fatalf("write mcp catalog row: %v", err)
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

// TestDiscoveryStoresOnlyABoundedListing is the cap seen from the outside, and
// it exists because TestStorableToolsCapsTheListing cannot see this: that test
// calls storableTools directly, so it stays green wherever — or whether — the
// driver applies it. What matters to the database is that a real server offering
// more than the cap allows produces a bounded row, so this drives one and reads
// the row back.
func TestDiscoveryStoresOnlyABoundedListing(t *testing.T) {
	// Eight tools of 64 KiB of description each: half a megabyte offered against
	// a quarter-megabyte cap, so the row must hold a strict prefix of them.
	var offered []mcptest.Tool
	for i := 0; i < 8; i++ {
		offered = append(offered, mcptest.Tool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: strings.Repeat("x", 64<<10),
		})
	}
	url := mcptest.Server(t, offered...)
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "ready" {
		t.Fatalf("row = %+v, want the listing stored", got)
	}
	if len(got.tools) == 0 || len(got.tools) >= len(offered) {
		t.Fatalf("stored %d of %d tools, want a bounded prefix of them", len(got.tools), len(offered))
	}
	var size int
	for _, tool := range got.tools {
		size += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	if size > maxCatalogTools {
		t.Errorf("row holds %d bytes of server-chosen text, want at most %d", size, maxCatalogTools)
	}
}

// TestTheMCPPassBudgetDefaultsToFiveMinutes pins the one number in this
// driver's configuration that nothing else would notice changing. The budget is
// what stands between a server that accepts a connection and never answers and
// every other session's work on the host — whether the pass is listing that
// server's tools or running one — and its default is quoted in three
// places an operator reads — the Config comment, the Helm value and the compose
// file — none of which a test can check. A non-positive value resolves to the
// default rather than to "no bound", so an operator who unsets the variable, or
// writes 0 expecting to disable it, gets the bound instead.
func TestTheMCPPassBudgetDefaultsToFiveMinutes(t *testing.T) {
	for _, supplied := range []time.Duration{0, -time.Second} {
		cfg := Config{MCPPassTimeout: supplied}.withDefaults()
		if want := 5 * time.Minute; cfg.MCPPassTimeout != want {
			t.Errorf("MCPPassTimeout %v resolved to %v, want %v", supplied, cfg.MCPPassTimeout, want)
		}
	}
	// A supplied bound is not overridden, or the variable would do nothing.
	if got := (Config{MCPPassTimeout: 90 * time.Second}).withDefaults().MCPPassTimeout; got != 90*time.Second {
		t.Errorf("a supplied budget resolved to %v, want it kept", got)
	}
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
	// The exact set, not a count and a spot check: those would accept two
	// copies of one tool while the other was dropped.
	sort.Strings(names)
	if want := []string{"create_issue", "search_issues"}; !slices.Equal(names, want) {
		t.Errorf("stored tools = %v, want %v", names, want)
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
	// A counting listener rather than an MCP server: the assertion is that
	// nothing reaches it at all. A fixture that only checked the stored row
	// would pass against an implementation that dialled first and wrote the
	// refusal afterwards — which is the ordering this test exists for, since a
	// connection that opened has already told the server the session exists.
	reached := &atomic.Int32{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	h := mcpHarness(t)
	h.setNetworking(t, domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"elsewhere.test"}})
	h.declareMCPServers(t, [2]string{"github", ts.URL})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "failed" {
		t.Fatalf("row = %+v, want the dial refused by the networking policy", got)
	}
	// The `limited` wording specifically, not merely "networking policy": the
	// other refusal this check can produce says that too, and a test that
	// accepted either would let the two swap places unnoticed.
	if !strings.Contains(got.reason, "`limited` networking policy") ||
		!strings.Contains(got.reason, "allowed_hosts") {
		t.Errorf("reason = %q, want it to name `limited` and the list that would admit the host", got.reason)
	}
	if len(got.tools) != 0 {
		t.Errorf("tools = %v, want none: the server was never reached", got.tools)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the refused server was contacted %d times; the check must run before the dial", n)
	}
}

// TestACloudEnvironmentNamingNoPolicyRefusesRatherThanAdmits pins the direction
// the egress check fails in. Only the REST surface normalizes a cloud config's
// networking to a literal type — the schema constrains `config->>'type'` against
// the environment kind and leaves `config->'networking'` alone, and a
// config-preserving update carries a stored value forward without revalidating
// it. Reading a missing discriminator as the wire default would make exactly
// that row the one admitted everywhere, so an unrecognized policy on a cloud
// environment refuses, the way `gate.newPolicy` does.
//
// The config deliberately satisfies *both* of `limited`'s admitting
// conditions — the host is in allowed_hosts and allow_mcp_servers is set — so
// the refusal can only be coming from the missing discriminator. A fixture
// that named neither would be refused by a check that had merely fallen
// through to the allow-list, and would prove nothing about the default.
func TestACloudEnvironmentNamingNoPolicyRefusesRatherThanAdmits(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET config = $2::jsonb WHERE id = $1`, h.envID.String(),
		`{"type":"cloud","networking":{"allowed_hosts":["127.0.0.1"],"allow_mcp_servers":true}}`); err != nil {
		t.Fatalf("write the malformed config: %v", err)
	}
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "failed" {
		t.Fatalf("row = %+v, want the dial refused: the environment names no policy the platform recognizes", got)
	}
	// The two refusals are told apart, because one of them has no advice an
	// operator can act on. This config carries both admitting fields — the host
	// is in allowed_hosts under `limited`'s own reading — so a reason telling
	// its owner to add the host to that list sends them to a list nothing
	// consults, and to a change they have already made.
	if strings.Contains(got.reason, "allowed_hosts") {
		t.Errorf("reason = %q, want it to blame the unrecognized policy rather than a list that is not consulted", got.reason)
	}
	if !strings.Contains(got.reason, "does not recognize") {
		t.Errorf("reason = %q, want it to name the unrecognized policy as the cause", got.reason)
	}
}

// TestASelfHostedEnvironmentNamingAPolicyIsHeldToIt is the same direction on
// the other arm of the union, and it is the arm where the fail-open is easier
// to miss: a self_hosted config is unconstrained *because* it carries no
// networking block — the REST surface rejects every field but `type` and stores
// exactly `{"type":"self_hosted"}` — so a row that does carry one was not
// written through the API, and the block it carries is a policy somebody meant.
// Ignoring it because of the kind would make the unrecognized-policy refusal
// reachable on cloud rows alone, and the malformed row the permissive one.
//
// The reachable path is the same one the cloud test names: the schema
// constrains `config->>'type'` against the kind and leaves `config->'networking'`
// alone, so an import, a restore or a hand-written UPDATE produces this row.
func TestASelfHostedEnvironmentNamingAPolicyIsHeldToIt(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET kind = 'self_hosted', config = $2::jsonb WHERE id = $1`,
		h.envID.String(), `{"type":"self_hosted","networking":{"type":"mesh"}}`); err != nil {
		t.Fatalf("write the malformed config: %v", err)
	}
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "failed" {
		t.Fatalf("row = %+v, want the dial refused: the config names a policy the platform does not recognize", got)
	}
	if !strings.Contains(got.reason, "does not recognize") {
		t.Errorf("reason = %q, want it to name the unrecognized policy as the cause", got.reason)
	}
}

// TestASelfHostedEnvironmentWithNoPolicyIsStillUnconstrained is the other half
// of the pair, and the one that keeps the rung above from becoming a refusal of
// every self_hosted session: the ordinary config — the only one the API can
// write — carries no networking block, and nothing is applied to it.
func TestASelfHostedEnvironmentWithNoPolicyIsStillUnconstrained(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET kind = 'self_hosted', config = '{"type":"self_hosted"}'::jsonb WHERE id = $1`,
		h.envID.String()); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := h.catalog(t)["github"]; got.status != "ready" {
		t.Errorf("row = %+v, want a self_hosted session's dial unconstrained", got)
	}
}

// TestASessionEndedMidPassGetsNoRowsAndNoTurn pins the settlement's second
// liveness check. A discovery pass may run for minutes, so the check
// sessionForRun made when the item was claimed is stale by the time the rows are
// written — and a session archived or terminated in between must not have
// catalog rows written for it, nor a model turn chained on its behalf for a
// brain to claim and drain. Every other executor settlement inherits this from
// events.AppendInTx, which refuses an archived session under the same row lock;
// discovery appends no event, so it has to ask.
//
// The end lands after the dials and before the settlement, which is the window
// under test: the server is reachable and the pass succeeds, so nothing but the
// check can be what suppresses the write. Both of the two ways a session stops
// being live are driven, because they are separate conditions and a test for one
// says nothing about the other.
func TestASessionEndedMidPassGetsNoRowsAndNoTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  string
	}{
		{"archived", `UPDATE sessions SET archived_at = now() WHERE id = $1`},
		{"terminated", `UPDATE sessions SET status = 'terminated' WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
			h := mcpHarness(t)
			h.declareMCPServers(t, [2]string{"github", url})
			h.enqueueMCP(t)

			item, err := h.queue.Claim(context.Background(), queue.MCPExec, time.Minute)
			if err != nil || item == nil {
				t.Fatalf("claim: %+v %v", item, err)
			}
			sess, live, err := h.exec.sessionForRun(context.Background(), item)
			if err != nil || !live {
				t.Fatalf("sessionForRun: live=%v err=%v", live, err)
			}
			rows, err := h.exec.discoverServers(
				context.Background(), sess.envConfig, sess.vaultIDs, sess.mcpServers)
			if err != nil {
				t.Fatalf("discoverServers: %v", err)
			}
			if len(rows) != 1 || rows[0].status != "ready" {
				t.Fatalf("rows = %+v, want the pass to have succeeded before the session ended", rows)
			}

			if _, err := h.pool.Exec(context.Background(), tc.end, h.sid.String()); err != nil {
				t.Fatalf("end the session: %v", err)
			}

			if err := h.exec.settleMCP(context.Background(), item, rows); err != nil {
				t.Fatalf("settleMCP: %v", err)
			}

			if cat := h.catalog(t); len(cat) != 0 {
				t.Errorf("catalog rows = %v, want none written for a session that ended", cat)
			}
			if n := h.liveOf(t, queue.ModelTurn); n != 0 {
				t.Errorf("live model_turn items = %d, want no turn chained for a session that ended", n)
			}
			if n := h.liveOf(t, queue.MCPExec); n != 0 {
				t.Errorf("live mcp_exec items = %d, want the item completed rather than left to reclaim", n)
			}
		})
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

// TestAServerControlledNULDoesNotWedgeDiscovery pins the catalog's storage
// boundary against a byte the server chooses. Postgres jsonb cannot hold
// U+0000 inside a string, so an unsanitized description would fail the INSERT
// before queue.Complete — the lease would lapse, the reclaim would re-list the
// same server, and the settlement would fail again, forever. The assertion that
// carries that is the completed work item: a stored row alone would not
// distinguish a pass that settled from one that had not run yet.
func TestAServerControlledNULDoesNotWedgeDiscovery(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool", Description: "bad\x00description"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "ready" {
		t.Fatalf("row = %+v, want the listing stored", got)
	}
	if len(got.tools) != 1 {
		t.Fatalf("tools = %+v, want the one tool the server offers", got.tools)
	}
	if strings.Contains(got.tools[0].Description, "\x00") {
		t.Errorf("stored description still carries a NUL: %q", got.tools[0].Description)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d: the settlement faulted and will reclaim-loop", n)
	}
}

// TestASchemaCarryingNULCostsItsToolAndNotTheListing covers the arm the
// end-to-end fixture cannot reach: in a schema a NUL is not a byte to strip but
// the six characters of a JSON escape, and stripping those textually would
// corrupt a schema whose own text contains a literal backslash-u-0000. So the
// tool is dropped and the rest of the listing survives.
func TestASchemaCarryingNULCostsItsToolAndNotTheListing(t *testing.T) {
	got := storableTools([]mcp.Tool{
		{Name: "keep", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "drop", InputSchema: json.RawMessage(`{"type":"object","title":"a\u0000b"}`)},
		{Name: "keep_too", Description: "fine", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	names := []string{}
	for _, tool := range got {
		names = append(names, tool.Name)
	}
	if want := []string{"keep", "keep_too"}; !slices.Equal(names, want) {
		t.Errorf("storable tools = %v, want %v", names, want)
	}
}

// TestSettlementSkipsAServerTheAgentNoLongerDeclares pins the settlement's own
// re-read. The dials happen outside the session row lock, so a mid-session
// agent patch can land while they are in flight — and `mcp_servers` is one of
// only two agent fields a running session may patch. A patch that removes or
// repoints a server finds no row to invalidate yet, so without this check the
// pass would write one behind it for a server the agent no longer declares,
// which nothing revisits: discovery only ever walks the declared array.
//
// settleMCP is called directly because the window it closes cannot be opened
// from outside — it needs the agent to change between the pass's read and its
// write, which is exactly the interleaving the row lock now prevents.
func TestSettlementSkipsAServerTheAgentNoLongerDeclares(t *testing.T) {
	h := mcpHarness(t)
	h.declareMCPServers(t,
		[2]string{"kept", "https://kept.test/mcp"},
		[2]string{"moved", "https://now.test/mcp"})
	h.enqueueMCP(t)
	item, err := h.queue.Claim(context.Background(), queue.MCPExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim mcp_exec: %+v %v", item, err)
	}

	// What a pass that read the agent before the patch would be holding: one
	// server still declared unchanged, one removed, one repointed since.
	if err := h.exec.settleMCP(context.Background(), item, []catalogRow{
		{name: "kept", url: "https://kept.test/mcp", status: "ready"},
		{name: "removed", url: "https://gone.test/mcp", status: "ready"},
		{name: "moved", url: "https://before.test/mcp", status: "ready"},
	}); err != nil {
		t.Fatalf("settleMCP: %v", err)
	}

	cat := h.catalog(t)
	if len(cat) != 1 {
		t.Fatalf("catalog = %v, want only the server still declared at that url", cat)
	}
	if _, ok := cat["kept"]; !ok {
		t.Errorf("catalog = %v, want the row for the unchanged server", cat)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the turn chained regardless", n)
	}
}

// TestAFailureReasonKeepsTheServersCredentialsOutOfTheCatalog pins the storage
// boundary for the text half of a row. An mcp_servers url is customer-supplied
// and may carry a credential in its userinfo or its query, and everything that
// builds a failure reason quotes the endpoint back: this package's client, and
// net/http under it. A reason is a stored column that later slices surface, so
// the whole class is cut off where the text becomes storage.
//
// Port 1 on loopback refuses immediately, which is what makes the reason a
// transport error rather than something this driver wrote itself.
func TestAFailureReasonKeepsTheServersCredentialsOutOfTheCatalog(t *testing.T) {
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", "http://tok3n:hunter2@127.0.0.1:1/mcp?api_key=SECRET"})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["github"]
	if got.status != "failed" || got.reason == "" {
		t.Fatalf("row = %+v, want a failed row carrying a reason", got)
	}
	for _, secret := range []string{"tok3n", "hunter2", "SECRET", "api_key"} {
		if strings.Contains(got.reason, secret) {
			t.Errorf("stored reason leaks %q: %q", secret, got.reason)
		}
	}
	// The host has to survive, or the redaction has taken the diagnosis with it.
	if !strings.Contains(got.reason, "127.0.0.1:1") {
		t.Errorf("reason = %q, want it to still name the host that could not be reached", got.reason)
	}
}

// TestStorableReasonRedactsTruncatesAndSanitizes covers the arms an end-to-end
// fixture cannot produce on demand: a reason megabytes long (a server's own
// JSON-RPC error message, bounded only by mcp.MaxResponseBytes), and one whose
// truncation lands mid-rune — which would leave a byte sequence Postgres rejects
// on a text column, re-opening by the back door the wedge the NUL strip closes.
func TestStorableReasonRedactsTruncatesAndSanitizes(t *testing.T) {
	t.Run("every url is cut to scheme and host", func(t *testing.T) {
		const endpoint = `https://u:p@mcp.example:8443/x?token=SECRET`
		got := storableReason(`mcp: connect to `+endpoint+`: Post "`+endpoint+`": dial tcp: refused`, endpoint)
		for _, secret := range []string{"SECRET", "token", "u:p", "/x"} {
			if strings.Contains(got, secret) {
				t.Errorf("reason %q still carries %q", got, secret)
			}
		}
		if want := "https://mcp.example:8443"; !strings.Contains(got, want) {
			t.Errorf("reason %q no longer names the endpoint's host (%q)", got, want)
		}
		if !strings.Contains(got, "dial tcp: refused") {
			t.Errorf("reason %q lost the cause", got)
		}
	})

	t.Run("a quote inside the url does not end the redaction early", func(t *testing.T) {
		// An apostrophe is an RFC 3986 sub-delim and legal in a query, so a
		// server URL may carry one ahead of its credential. A matcher that
		// stopped at it would leave the credential in the text *next to a host
		// that looks redacted*, which reads as a redaction that worked.
		const endpoint = `https://mcp.example/x?note='&api_key=SECRET`
		got := storableReason(`mcp: connect to `+endpoint+`: dial tcp: refused`, endpoint)
		for _, secret := range []string{"SECRET", "api_key"} {
			if strings.Contains(got, secret) {
				t.Errorf("reason %q still carries %q past the apostrophe", got, secret)
			}
		}
		if !strings.Contains(got, "dial tcp: refused") {
			t.Errorf("reason %q lost the cause", got)
		}
	})

	t.Run("a raw space inside the url does not end the redaction early", func(t *testing.T) {
		// The case no character class can reach: a URL is recognized in prose
		// by where it ends, and a space has to end it — yet this is a URL the
		// platform accepts and dials, so the pattern pass stops mid-query. Only
		// knowing the endpoint by value closes it.
		const endpoint = `http://mcp.example/mcp?agent=my agent&api_key=SECRET`
		got := storableReason(`mcp: connect to `+endpoint+`: Post "`+endpoint+`": dial tcp: refused`, endpoint)
		for _, secret := range []string{"SECRET", "api_key"} {
			if strings.Contains(got, secret) {
				t.Errorf("reason %q still carries %q past the space", got, secret)
			}
		}
		if !strings.Contains(got, "http://mcp.example") {
			t.Errorf("reason %q no longer names the host", got)
		}
	})

	// Four writers spell the same endpoint four ways, and each rendering below
	// carries a space so the pattern pass cannot reach it: what is under test is
	// that the *value* pass enumerates that rendering, not that something else
	// happens to catch it.
	for _, tc := range []struct {
		name     string
		endpoint string
		render   func(t *testing.T, endpoint string) string
	}{{
		name:     "net/http's masked password",
		endpoint: `https://u:p@mcp.example/x?q=a b&api_key=SECRET`,
		render: func(t *testing.T, endpoint string) string {
			t.Helper()
			u, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			// net/http writes `***` where url.URL.Redacted writes `xxxxx`.
			return strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
		},
	}, {
		name:     "url.Error's %q escaping",
		endpoint: `https://mcp.example/x?q=a "b" c&api_key=SECRET`,
		render: func(t *testing.T, endpoint string) string {
			t.Helper()
			quoted := strconv.Quote(endpoint)
			return quoted[1 : len(quoted)-1]
		},
	}, {
		name:     "url.URL.Redacted's own",
		endpoint: `https://u:p@mcp.example/x?q=a b&api_key=SECRET`,
		render: func(t *testing.T, endpoint string) string {
			t.Helper()
			u, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			return u.Redacted()
		},
	}, {
		// An empty port is legal and http.NewRequest deletes it (Go issue
		// 14836), so net/http names a URL the agent never declared. The
		// rendering is taken from a real request rather than written out here:
		// what is under test is that the enumeration covers what the platform's
		// own HTTP stack produces, and a hand-spelled copy would only test
		// itself.
		name:     "net/http's empty-port normalization",
		endpoint: `https://u:p@mcp.example:/x?q=a b&api_key=SECRET`,
		render: func(t *testing.T, endpoint string) string {
			t.Helper()
			req, err := http.NewRequest(http.MethodPost, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			return strings.Replace(req.URL.String(),
				req.URL.User.String()+"@", req.URL.User.Username()+":***@", 1)
		},
	}} {
		t.Run("a rendering by "+tc.name+" is redacted too", func(t *testing.T) {
			rendered := tc.render(t, tc.endpoint)
			got := storableReason("mcp: connect to "+rendered+": refused", tc.endpoint)
			for _, secret := range []string{"SECRET", "api_key", "u:p"} {
				if strings.Contains(got, secret) {
					t.Errorf("reason %q still carries %q (from %q)", got, secret, rendered)
				}
			}
		})
	}

	t.Run("a url the driver never supplied is redacted by the pattern pass", func(t *testing.T) {
		// The pass the other subtests cannot reach: they all hand storableReason
		// the endpoint that appears in the text, so the by-value pass satisfies
		// them and deleting the pattern pass leaves them green. A server naming
		// a *different* credential-bearing URL in its own error text — an OAuth
		// endpoint, a redirect target — is the case only the pattern can catch,
		// because the driver has no value to enumerate for it.
		got := storableReason(
			`mcp: list tools: server said: token refresh at https://svc.example/oauth?client_secret=THIRDPARTY failed`,
			"https://mcp.example/mcp")
		for _, secret := range []string{"THIRDPARTY", "client_secret"} {
			if strings.Contains(got, secret) {
				t.Errorf("reason %q still carries %q from a url the driver did not supply", got, secret)
			}
		}
		if !strings.Contains(got, "https://svc.example") {
			t.Errorf("reason %q no longer names the host the server blamed", got)
		}
	})

	t.Run("an ipv6 endpoint keeps its host", func(t *testing.T) {
		// The trailing-punctuation trim eats `]` as readily as it eats a full
		// stop, and an IPv6 authority ends in one. Trimmed away, the URL no
		// longer parses and the whole thing becomes the fallback — so every
		// failure reason for every IPv6-addressed server would lose the one
		// piece of it an operator needs.
		const endpoint = "https://[fd00::1]/mcp"
		got := storableReason("mcp: connect to "+endpoint+": dial tcp: refused", endpoint)
		if !strings.Contains(got, "https://[fd00::1]") {
			t.Errorf("reason %q lost the ipv6 host", got)
		}
		if strings.Contains(got, "[redacted url]") {
			t.Errorf("reason %q fell back to the placeholder for a url it could have named", got)
		}
	})

	t.Run("a NUL never reaches the column", func(t *testing.T) {
		if got := storableReason("before\x00after", ""); strings.Contains(got, "\x00") {
			t.Errorf("reason %q still carries a NUL", got)
		}
	})

	t.Run("invalid utf-8 shorter than the cap is still repaired", func(t *testing.T) {
		// Postgres refuses an invalid byte sequence on a text column exactly as
		// it refuses a NUL, and faults the item the same way. The bytes need
		// not come from a truncation: Go's HTTP parser accepts obs-text in a
		// header value, so a server that answers with one in `Mcp-Session-Id`
		// has it quoted back inside an SDK error and stored from here.
		got := storableReason("mcp: list tools: session id \xff\xfe rejected", "")
		if !utf8.ValidString(got) {
			t.Errorf("reason %q is not valid utf-8 and will fault the insert", got)
		}
	})

	t.Run("an oversized reason is capped and stays valid utf-8", func(t *testing.T) {
		// A multi-byte rune straddling the cut: with maxCatalogReason bytes of
		// ASCII ahead of it, the cap lands inside the "…" that follows.
		got := storableReason(strings.Repeat("x", maxCatalogReason-1)+"…"+strings.Repeat("y", 100), "")
		// The cap plus the ellipsis the cut appends, and not a byte more: a
		// looser bound leaves room for a truncation that keeps part of the
		// straddling rune, which is the failure this arm exists to catch.
		if want := maxCatalogReason + len("…"); len(got) > want {
			t.Errorf("reason kept %d bytes, want at most %d", len(got), want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("capped reason is not valid utf-8: %q", got)
		}
	})
}

// errorsOfType returns the `error` object of every session.error the session
// carries whose type is typ, so a test can count the ones it means without
// counting the ones another rung emits.
func (h *harness) errorsOfType(t *testing.T, typ string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range h.sessionErrors(t) {
		var body struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(ev.Body, &body); err != nil {
			t.Fatalf("decode session error: %v", err)
		}
		if body.Error["type"] == typ {
			out = append(out, body.Error)
		}
	}
	return out
}

// TestATarpitServerDoesNotStarveTheOnesDeclaredAfterIt pins what the pass owes
// a session with more than one server. Serially, position decided reach: a
// server that accepts a connection and never answers spent the whole budget, so
// the servers behind it were recorded unreached for the life of the session and
// the model was never offered their tools. Position is not a
// fact about a server.
//
// The aggregate budget is still what stops the pass — the tarpit's own row is
// the failure it earns — but it stops that server rather than the queue behind
// it.
func TestATarpitServerDoesNotStarveTheOnesDeclaredAfterIt(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	client := mcptest.Client()
	// Registered before the teardown below, because t.Cleanup is LIFO and this
	// server's own cleanup has to run first — the ordering the comment there is
	// about.
	healthy := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	// Teardown in one place, because its order matters and the default is
	// wrong twice over. The tarpit is released explicitly rather than by its
	// request context — a client that abandons a request does not reliably
	// cancel the server's side of it — and the client's pooled connections are
	// dropped before the server closes, because httptest.Server.Close waits for
	// every connection to go idle and the abandoned response bodies never do.
	t.Cleanup(func() {
		close(release)
		client.CloseIdleConnections()
		hang.Close()
	})

	// Headroom over what a loopback handshake and listing take, because what
	// this test is about is that the healthy server is *dialled at all* while
	// the tarpit holds the budget — a budget so tight that a loaded runner
	// could miss it would report this test's own regression.
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{MCPPassTimeout: 5 * time.Second})
	h.exec.mcpHTTP = client
	h.setNetworking(t, domain.Networking{Type: domain.NetUnrestricted})
	// Declared behind the tarpit, which is the whole point: this is the entry a
	// serial pass never reached.
	h.declareMCPServers(t, [2]string{"slow", hang.URL}, [2]string{"after", healthy})
	h.enqueueMCP(t)

	h.stepOnce(t)

	cat := h.catalog(t)
	if got := cat["slow"]; got.status != "failed" {
		t.Errorf("row = %+v, want the tarpit server recorded as a failure", got)
	}
	if got := cat["after"]; got.status != "ready" || len(got.tools) != 1 {
		t.Errorf("row = %+v, want the server behind the tarpit listed in this same pass", got)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d: a spent budget is a recorded failure, not a faulted item", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the turn chained regardless", n)
	}
}

// TestDiscoveryStopsWhenThePassRunsOutOfTime pins the aggregate budget. The
// endpoints are third-party and this process runs one work item at a time, so
// without a bound on the pass as a whole a tarpit server holds every other
// session's tool calls on the host behind it for as long as it cares to. What
// the fan-out changed is who pays: the bound is now one server's dial-and-list
// rather than the sum of every declared server's, so the row a tarpit earns is
// its own.
//
// It costs about ten seconds of wall clock for a budget of a third of a second,
// and that is the behaviour under test rather than a slow fixture: the SDK
// answers a cancelled request with a `notifications/cancelled` sent on a
// context detached from the one that was cancelled and bounded at five seconds
// of its own, and a connection can have two calls outstanding.
func TestDiscoveryStopsWhenThePassRunsOutOfTime(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	client := mcptest.Client()
	t.Cleanup(func() {
		close(release)
		client.CloseIdleConnections()
		hang.Close()
	})

	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{MCPPassTimeout: 300 * time.Millisecond})
	h.exec.mcpHTTP = client
	h.setNetworking(t, domain.Networking{Type: domain.NetUnrestricted})
	h.declareMCPServers(t, [2]string{"slow", hang.URL})
	h.enqueueMCP(t)

	h.stepOnce(t)

	got := h.catalog(t)["slow"]
	if got.status != "failed" {
		t.Errorf("row = %+v, want the tarpit server recorded as a failure", got)
	}
	// The reason says what this pass did rather than blaming the server for a
	// clock this platform owns, and it is marked so the settlement keeps a
	// finding an earlier pass earned instead of overwriting it.
	if !strings.Contains(got.reason, "out of time") {
		t.Errorf("reason = %q, want the pass's own scheduling and not a verdict on the server", got.reason)
	}
	// A pass that ran out of time has no connection for an operator to heal, so
	// it says nothing on the wire.
	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 0 {
		t.Errorf("session errors = %d, want none for a budget this platform spent", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d: a spent budget is a recorded failure, not a faulted item", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the turn chained regardless", n)
	}
}

// A server the discovery pass could not reach is said out loud on the cadence it
// is re-dialled at: once per work cycle, not once per turn. The row alone was
// the whole record before this — an operator watching the session's events saw a
// model quietly offered fewer tools than the agent declared.
//
// Both halves are pinned here, because the quiet half is what a wrong dedupe
// gets right by accident: a second pass within the cycle must add nothing, and a
// new cycle must speak again.
func TestADiscoveryFailureIsAnnouncedOncePerWorkCycle(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer down.Close()

	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", down.URL})
	h.enqueueMCP(t)
	h.stepOnce(t)

	if got := h.catalog(t)["github"]; got.status != "failed" {
		t.Fatalf("row = %+v, want the unreachable server recorded as a failure", got)
	}
	errs := h.errorsOfType(t, "mcp_connection_failed_error")
	if len(errs) != 1 {
		t.Fatalf("session errors = %d, want exactly one naming the server", len(errs))
	}
	if got := errs[0]["mcp_server_name"]; got != "github" {
		t.Errorf("mcp_server_name = %v, want the declared name", got)
	}
	// The endpoint is customer-supplied and may carry a credential, so what the
	// message carries is what the row carries — already cut to scheme://host by
	// storableReason — and nothing more.
	if got, want := errs[0]["message"], h.catalog(t)["github"].reason; got != want {
		t.Errorf("message = %v, want the row's own reason %q", got, want)
	}

	// A second pass inside the same work cycle — an agent patch adding a server
	// enqueues one, so this is reachable without a new cycle.
	h.enqueueMCP(t)
	h.stepOnce(t)
	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 1 {
		t.Errorf("session errors after a second pass = %d, want still 1 — the row this pass wrote is what keeps it quiet", n)
	}

	// A new work cycle drops the session's failed rows, which is what puts those
	// servers back in the never-reached state a turn suspends for (internal/api,
	// startWorkCycle — the reference's documented retry cadence). The next pass
	// therefore dials again, and a server still down is worth saying again: the
	// operator sent another message, and this is the answer to it.
	if _, err := h.pool.Exec(context.Background(),
		`DELETE FROM mcp_catalogs WHERE session_id = $1 AND status = 'failed'`,
		h.sid.String()); err != nil {
		t.Fatalf("start a new work cycle: %v", err)
	}
	h.enqueueMCP(t)
	h.stepOnce(t)
	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 2 {
		t.Errorf("session errors after a new work cycle = %d, want 2 — the operator asked again", n)
	}
}

// A credential the server refused is the other of the wire's two failures, and
// discovery has to tell them apart the way execution already does: a refused
// credential is not a connection that failed, because the connection worked
// well enough to be refused.
func TestADiscoveryAuthenticationFailureIsTypedAsOne(t *testing.T) {
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer refuses.Close()

	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", refuses.URL})
	h.enqueueMCP(t)
	h.stepOnce(t)

	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 0 {
		t.Errorf("connection-failed errors = %d, want none: the server answered, it refused", n)
	}
	errs := h.errorsOfType(t, "mcp_authentication_failed_error")
	if len(errs) != 1 {
		t.Fatalf("authentication-failed errors = %d, want exactly one", len(errs))
	}
	if got := errs[0]["mcp_server_name"]; got != "github" {
		t.Errorf("mcp_server_name = %v, want the declared name", got)
	}
}

// TestNoTurnIsChainedWhileAToolCallIsUnanswered pins the condition the turn is
// chained on, which is the one every other settlement uses. mcp_exec and
// tool_exec are different queue kinds, so nothing keeps one from settling while
// the other is in flight — and a model_turn sent with an agent.tool_use still
// unanswered assembles a request the Messages API rejects, failing the turn the
// user sees. Whichever settlement finishes last finds the set answered, so the
// gate costs no wakeup.
func TestNoTurnIsChainedWhileAToolCallIsUnanswered(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h := mcpHarness(t)
	if _, err := h.log.Append(context.Background(), h.sid, []events.NewEvent{{
		Type:    domain.EventAgentToolUse,
		Payload: json.RawMessage(`{"id":"toolu_pending","name":"bash","input":{"command":"sleep 1"}}`),
	}}); err != nil {
		t.Fatalf("append the outstanding tool_use: %v", err)
	}
	h.declareMCPServers(t, [2]string{"github", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if got := h.catalog(t)["github"]; got.status != "ready" {
		t.Errorf("row = %+v, want discovery to have run and stored its listing", got)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec items = %d, want the item completed", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn items = %d: waking the brain now sends a request the API rejects", n)
	}
}

// TestStorableToolsTellsANULFromTheEscapeThatSpellsIt pins the distinction the
// byte search could not make. `"^[^\\u0000-\\u001f]*$"` is how a JSON Schema
// says "no control characters": the value holds a backslash and the text u0000,
// and re-marshaling escapes the backslash, so the bytes contain the six
// characters a NUL is spelled with while the schema holds no NUL at all.
// Dropping that tool costs an agent a tool over an idiom, and the drop is
// silent — no row, no reason. A schema that does carry the code point still
// costs its tool, because Postgres will not store it.
func TestStorableToolsTellsANULFromTheEscapeThatSpellsIt(t *testing.T) {
	nulInSchema, err := json.Marshal(map[string]any{"type": "object", "title": "a\u0000b"})
	if err != nil {
		t.Fatal(err)
	}
	tools := []mcp.Tool{
		{Name: "idiom", InputSchema: json.RawMessage(`{"type":"object","properties":` +
			`{"q":{"type":"string","pattern":"^[^\\u0000-\\u001f]*$"}}}`)},
		{Name: "real_nul", InputSchema: nulInSchema},
		{Name: "undecodable", InputSchema: json.RawMessage(`{"type":`)},
	}

	got := storableTools(tools)
	if len(got) != 1 || got[0].Name != "idiom" {
		names := make([]string, 0, len(got))
		for _, t := range got {
			names = append(names, t.Name)
		}
		t.Fatalf("kept %v, want only the tool whose schema merely spells the escape", names)
	}
}

// TestStorableToolsCapsTheListing pins the bound on the column a server fills.
// `tools` is server-chosen text with no length of its own — a listing may run to
// the whole response budget, and the catalog is per session, so the same agent
// copies it into every session it starts. Tools are kept in the server's order
// until the next would not fit, and dropped whole rather than truncated.
func TestStorableToolsCapsTheListing(t *testing.T) {
	const each = 64 << 10
	var tools []mcp.Tool
	for i := 0; i < 8; i++ {
		tools = append(tools, mcp.Tool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: strings.Repeat("x", each),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}

	got := storableTools(tools)
	if len(got) == len(tools) {
		t.Fatalf("kept all %d tools, want the listing capped", len(tools))
	}
	var size int
	for _, tool := range got {
		size += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	if size > maxCatalogTools {
		t.Errorf("kept %d bytes of server-chosen text, want at most %d", size, maxCatalogTools)
	}
	// Kept in the server's own order, from the front: a cap that reordered or
	// sampled would make the catalog depend on nothing the server can see.
	for i, tool := range got {
		if want := fmt.Sprintf("tool_%d", i); tool.Name != want {
			t.Errorf("tool %d is %q, want %q — the cap must keep a prefix, in order", i, tool.Name, want)
		}
	}
}

// roundTripAfter delegates to base and runs after once, on the first request it
// carries. It is how a test puts an event on the log *during* a pass: between
// the driver's own check and its settlement, which is the window this file's
// concurrency arguments are about and which no fixture hook can reach.
type roundTripAfter struct {
	base  http.RoundTripper
	once  sync.Once
	after func()
}

func (r *roundTripAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := r.base.RoundTrip(req)
	r.once.Do(r.after)
	return res, err
}

// A discovery pass is the fifth settlement site, and the one whose pass is long
// enough for a call to arrive underneath it: the dials run for minutes, and an
// mcp_tool_use committed while they do cannot have been queued behind them —
// Enqueue is keyed (session_id, kind) over the live states and this very item is
// one. Completing here would leave the call with nothing scheduled to answer it,
// the session running, and archive and delete both refused.
func TestDiscoveryHandsItsItemBackForACallThatArrivedUnderneathIt(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Description: "searches"})
	h := mcpHarness(t)
	// The call arrives after the driver has looked for one and before the pass
	// settles — the only window in which settleMCP sees an unanswered call.
	h.exec.mcpHTTP = &http.Client{Transport: &roundTripAfter{
		base:  h.exec.mcpHTTP.Transport,
		after: func() { h.appendMCPToolUse(t, "docs", "search", `{}`) },
	}}
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Errorf("mcp_exec live = %d, want 1 — the call needs this driver and nothing else answers it", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn = %d, want 0 — a call is unanswered", n)
	}
	// The listing still landed: handing the item back must not throw the pass away.
	if got := h.catalog(t)["docs"]; got.status != "ready" {
		t.Errorf("catalog row = %+v, want the listing this pass fetched", got)
	}
}

// A credential the platform could not resolve at all never reaches the server,
// and the reference counts it as an authentication failure all the same — so
// discovery has to type it as one. The row alone was already pinned; the event
// this slice adds needs its own assertion, because typing it wrongly sends an
// operator after a connection that is fine.
func TestACredentialThePlatformCannotOpenIsAnAuthenticationFailure(t *testing.T) {
	h := mcpHarness(t)
	url := mcptest.Server(t, mcptest.Tool{Name: "ok_tool"})
	h.attachVaultWithAnUnopenableCredential(t, url)
	h.declareMCPServers(t, [2]string{"docs", url})
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 0 {
		t.Errorf("connection-failed errors = %d, want none: nothing was dialled", n)
	}
	errs := h.errorsOfType(t, "mcp_authentication_failed_error")
	if len(errs) != 1 {
		t.Fatalf("authentication-failed errors = %d, want exactly one", len(errs))
	}
	if got := errs[0]["mcp_server_name"]; got != "docs" {
		t.Errorf("mcp_server_name = %v, want the declared name", got)
	}
}

// A pass that ran out of time before a server has told nobody anything, so it
// must not be what silences that server's first real verdict. The two states are
// one status in the table — "failed" — which is why the dedupe asks about the
// reason rather than the status.
func TestABudgetRowDoesNotSilenceTheFirstRealFailure(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer down.Close()

	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"github", down.URL})
	// The row an earlier, timed-out pass would have left: failed, and carrying
	// the fallback reason that says so.
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status, error, fetched_at)
		 VALUES ($1, 'github', $2, '[]'::jsonb, 'failed', $3, now())`,
		h.sid.String(), down.URL, passRanOutOfTime); err != nil {
		t.Fatalf("seed the timed-out row: %v", err)
	}
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := len(h.errorsOfType(t, "mcp_connection_failed_error")); n != 1 {
		t.Errorf("session errors = %d, want the first real failure said out loud "+
			"even though a timed-out pass had already written a failed row", n)
	}
}

// The pass's budget is one deadline every dial in it shares, so a verdict a
// server earned microseconds before that instant arrives with the context
// already dead. Deciding on the clock alone would relabel it as this platform's
// scheduling, throwing the finding away and saying nothing about it — so the
// error has to agree that it was a cancellation. Asserted on the function rather
// than through a pass, because the interleaving it is about is a race no test
// can schedule.
func TestAVerdictThatBeatTheBudgetIsNotRelabelledAsScheduling(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	row := catalogRow{name: "docs", url: "https://example.test/mcp", status: "failed"}

	earned := failedDial(dead, row, errors.New("boom"))
	if earned.notReached {
		t.Error("a verdict the server earned was marked as the pass's own scheduling")
	}
	if !strings.Contains(earned.reason, "boom") {
		t.Errorf("reason = %q, want the verdict the server earned", earned.reason)
	}

	cut := failedDial(dead, row, context.DeadlineExceeded)
	if !cut.notReached || cut.reason != passRanOutOfTime {
		t.Errorf("row = %+v, want a dial the budget cut short marked as scheduling", cut)
	}
}
