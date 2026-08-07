package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// Unit M's brain rows (docs/plan/25_git-repo-mounting.md): the "Mounted
// repositories" block the agent sees, its cloud-only gating, and the token
// sweep over the whole assembled request.

const (
	repoResources = `[{"id":"sesrsc_a","type":"github_repository",` +
		`"url":"https://github.com/example-org/widget","mount_path":"/workspace/widget",` +
		`"checkout":{"type":"branch","name":"release"},` +
		`"created_at":"2026-08-07T00:00:00Z","updated_at":"2026-08-07T00:00:00Z"},` +
		`{"id":"sesrsc_b","type":"github_repository",` +
		`"url":"https://github.com/example-org/lib","mount_path":"/workspace/lib","checkout":null,` +
		`"created_at":"2026-08-07T00:00:00Z","updated_at":"2026-08-07T00:00:00Z"},` +
		`{"id":"sesrsc_c","type":"github_repository",` +
		`"url":"https://github.com/example-org/pin","mount_path":"/workspace/pin",` +
		`"checkout":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"},` +
		`"created_at":"2026-08-07T00:00:00Z","updated_at":"2026-08-07T00:00:00Z"}]`

	repoResolvedAgent = `{"type":"agent","id":"agent_fixture","version":1,"name":"fixture",` +
		`"model":{"id":"fixture-model"},"system":"base prompt","description":"",` +
		`"tools":[],"mcp_servers":[],"skills":[],"multiagent":null}`
)

// seedRepos points the session at the repository resources above and gives it
// a resolvable agent snapshot, then drives one turn and returns the assembled
// system prompt.
func seedRepos(t *testing.T, h *harness) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET resources = $1, resolved_agent = $2 WHERE id = $3`,
		repoResources, repoResolvedAgent, h.sessionID.String()); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	h.wake(t, "hi")
	h.runOnce(t)
	if len(h.provider.calls) == 0 {
		t.Fatal("the provider was never called")
	}
	return h.provider.calls[0].System
}

// TestReposBlockInjected is m-brain-block: a cloud session's repositories are
// named in the system prompt with their path, url, and checkout descriptor.
func TestReposBlockInjected(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	sys := seedRepos(t, h)

	if !strings.Contains(sys, "Mounted repositories") {
		t.Fatalf("system prompt carries no repositories block:\n%s", sys)
	}
	for _, want := range []string{
		"/workspace/widget (https://github.com/example-org/widget, branch release)",
		"/workspace/lib (https://github.com/example-org/lib, default branch)",
		"/workspace/pin (https://github.com/example-org/pin, commit 0123456789abcdef0123456789abcdef01234567)",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sys)
		}
	}
	// The agent's own prompt still leads; the block is appended metadata.
	if !strings.HasPrefix(sys, "base prompt") {
		t.Errorf("the block displaced the agent's system prompt:\n%s", sys)
	}
}

// TestReposBlockSkippedOnSelfHosted is m-brain-byoc 🔍: nothing materializes
// repositories on self_hosted, so asserting a checkout there would be a false
// statement to the model — the block is absent even though the resources are.
func TestReposBlockSkippedOnSelfHosted(t *testing.T) {
	h := newHarnessEnv(t, "self_hosted", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	sys := seedRepos(t, h)

	if strings.Contains(sys, "Mounted repositories") {
		t.Errorf("a self_hosted session was told repositories are mounted:\n%s", sys)
	}
	if strings.Contains(sys, "example-org/widget") {
		t.Errorf("a self_hosted session's prompt names a repository:\n%s", sys)
	}
}

// TestReposBlockCarriesNoToken is m-brain-no-token 🔍: the brain never reads
// the credential table, so no part of the assembled request can carry a token.
// The sweep is over the whole request, not just the block.
func TestReposBlockCarriesNoToken(t *testing.T) {
	const token = "ghp_BRAIN-SWEEP-TOKEN-77c1"
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	ctx := context.Background()
	// A sealed credential row exists for the resource, as a real create would
	// have left it — the sweep is meaningless without one.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO session_resource_credentials (resource_id, session_id, token_ciphertext, token_key_id)
		 VALUES ($1, $2, $3, $4)`, "sesrsc_a", h.sessionID.String(), []byte(token), "plain-for-the-sweep"); err != nil {
		t.Fatalf("seed the credential row: %v", err)
	}
	sys := seedRepos(t, h)

	req := h.provider.calls[0]
	if strings.Contains(sys, token) {
		t.Errorf("the system prompt carries the token:\n%s", sys)
	}
	for i, m := range req.Messages {
		if strings.Contains(string(m.Content), token) {
			t.Errorf("message %d carries the token: %s", i, m.Content)
		}
	}
	for i, tool := range req.Tools {
		if strings.Contains(string(tool), token) {
			t.Errorf("tool definition %d carries the token: %s", i, tool)
		}
	}
}
