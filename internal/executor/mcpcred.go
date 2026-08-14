package executor

import (
	"context"
	"errors"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
)

// mcpBearer resolves the bearer token this session's attached vaults register
// for endpoint, or "" when they register none — in which case the dial carries
// no credential of this platform's choosing, which is what the reference
// documents for a server no credential matches. An mcp_servers URL written with
// userinfo still authenticates with it: net/http derives a Basic header from the
// URL, and a resolved token replaces that header rather than joining it.
//
// Resolved at every connect, on both dial paths, rather than once per session:
// the rows are read fresh each time, so a rotated token, an archived credential
// and an archived vault all reach the next dial without a restart.
//
// A credential that matched and cannot be used is an error, never a quiet
// fall-back to an anonymous dial. Downgrading silently would send an
// unauthenticated request the operator believes was authenticated, and the
// server's own refusal would then read as a credential that is wrong rather than
// one that never arrived.
//
// The caller has to tell that error apart from one the lookup itself raised —
// see [vaultresolve.ErrCredentialUnusable].
func (e *Executor) mcpBearer(ctx context.Context, vaultIDs []string, endpoint string) (string, error) {
	return vaultresolve.MCPCredentialFor(ctx, e.pool, e.cipher, vaultIDs, endpoint)
}

// credentialUnusable says the credential itself is at fault, so answering the
// call settles something an operator has to fix. Everything else — a pool that
// blinked, a cipher backend that timed out — says nothing about the credential
// and is worth the retry a faulted work item gets.
func credentialUnusable(err error) bool {
	return errors.Is(err, vaultresolve.ErrCredentialUnusable)
}

// mcpAuthFailure separates the wire's two MCP failures, by cause rather than by
// symptom: `mcp_connection_failed_error` is a server that "could not be reached
// (network error, timeout, or non-authentication HTTP failure)", while
// `mcp_authentication_failed_error` covers the server rejecting the vault's
// credential, requiring one where none matched, or a credential this platform
// could not produce. The first two arrive alike as a 401 or 403 — a server
// answers the same whether a token was sent or not — so one test answers both.
func mcpAuthFailure(err error) bool {
	return errors.Is(err, mcp.ErrUnauthorized) || credentialUnusable(err)
}

// mcpDialReason renders a failed dial, listing or credential lookup into the
// reason a catalog row stores, saying which of the two it was so an operator is
// pointed at the credential rather than at the network.
func mcpDialReason(err error) string {
	if mcpAuthFailure(err) {
		return "authentication failed: " + err.Error()
	}
	return err.Error()
}
