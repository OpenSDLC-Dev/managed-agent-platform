package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

// TestReposBlockMarksAFailedClone 🔍 is the per-repository twin of the
// self_hosted gate: a repository the executor could not clone must not be
// described to the model as checked out.
//
// The block is rebuilt from the configured resources on every turn, and a clone
// failure is deliberately tolerated — the session runs on with an absent mount
// and a session.error the model never sees in replay. Left unqualified, the
// block would then assert a checkout that is not there for the rest of the
// session, and the model would search a path that does not exist. The other
// repositories are unaffected: one failure must not blind the model to the
// checkouts it does have.
func TestReposBlockMarksAFailedClone(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	// The event the executor writes when a clone fails, for the first of the
	// three repositories seeded below.
	seedCloneError(t, h, "sesrsc_a", "auth")

	sys := seedRepos(t, h)

	line := repoLine(t, sys, "/workspace/widget")
	if !strings.Contains(line, "auth") || !strings.Contains(strings.ToLower(line), "not available") {
		t.Errorf("the failed repository is described as if it were checked out: %q", line)
	}
	// The two that did not fail are still offered plainly.
	for _, path := range []string{"/workspace/lib", "/workspace/pin"} {
		if l := repoLine(t, sys, path); strings.Contains(strings.ToLower(l), "not available") {
			t.Errorf("%s is marked unavailable, but only sesrsc_a failed: %q", path, l)
		}
	}
}

// TestReposBlockNamesTheLatestFailure 🔍: the reason the block reports is the
// last one recorded for a repository, not the first one.
//
// The executor's dedupe is per (resource, reason), so it deliberately re-emits
// when the reason changes: a repository that first failed to authenticate and
// then failed to reach its host has two clone errors on the log, in that order.
// Reading the older one sends the model — and the operator reading the prompt
// back — after a credential that stopped being the problem, while the block
// still claims to describe "the last clone attempt".
func TestReposBlockNamesTheLatestFailure(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{textChunk(0, "ok"), done("end_turn", 1)}}, nil)
	seedCloneError(t, h, "sesrsc_a", "auth")
	seedCloneError(t, h, "sesrsc_a", "network")

	line := repoLine(t, seedRepos(t, h), "/workspace/widget")
	if !strings.Contains(line, "network") || strings.Contains(line, "auth") {
		t.Errorf("the block reports a stale clone failure: %q — the last reason "+
			"recorded for this repository was \"network\"", line)
	}
}

// seedCloneError appends the session.error the executor writes when a clone
// fails, for the repository the block calls /workspace/widget.
func seedCloneError(t *testing.T, h *harness, resourceID, reason string) {
	t.Helper()
	if _, err := events.NewLog(h.pool).Append(context.Background(), h.sessionID, []events.NewEvent{{
		Type: domain.EventSessionError,
		Payload: []byte(`{"error":{"type":"github_repository_clone_error","resource_id":"` + resourceID + `",` +
			`"url":"https://github.com/example-org/widget","mount_path":"/workspace/widget",` +
			`"reason":"` + reason + `","message":"the repository could not be cloned into the sandbox",` +
			`"retry_status":{"type":"retrying"}}}`),
	}}); err != nil {
		t.Fatalf("seed the clone error: %v", err)
	}
}

// repoLine returns the block's bullet for one mount path.
func repoLine(t *testing.T, sys, mount string) string {
	t.Helper()
	for _, l := range strings.Split(sys, "\n") {
		if strings.HasPrefix(l, "- "+mount+" ") {
			return l
		}
	}
	t.Fatalf("no repositories-block line for %s in:\n%s", mount, sys)
	return ""
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
