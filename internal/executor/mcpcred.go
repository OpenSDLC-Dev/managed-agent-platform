package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
)

// errCredentialUnusable marks a credential that matched this server and could
// not be turned into a token to send — an unopenable ciphertext, a cipher-less
// deployment. The dial never happens, so no server ever refuses it, but it is
// an authentication failure all the same and the wire has no fourth type for it.
var errCredentialUnusable = errors.New("the credential could not be resolved")

// mcpBearer resolves the bearer token this session's attached vaults register
// for endpoint, or "" when they register none — in which case the dial goes out
// unauthenticated, which is what the reference documents for a server no
// credential matches.
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
func (e *Executor) mcpBearer(ctx context.Context, vaultIDs []string, endpoint string) (string, error) {
	cred, err := vaultresolve.MCPCredentialFor(ctx, e.pool, e.cipher, vaultIDs, endpoint)
	if err != nil {
		// vaultresolve's errors name credential ids and never secrets or
		// ciphertext, so this one is safe to store and log as it stands.
		return "", fmt.Errorf("%w: %w", errCredentialUnusable, err)
	}
	if cred == nil {
		return "", nil
	}
	return cred.Token, nil
}

// mcpAuthFailure separates the wire's two MCP failures, by cause rather than by
// symptom: `mcp_connection_failed_error` is a server that "could not be reached
// (network error, timeout, or non-authentication HTTP failure)", while
// `mcp_authentication_failed_error` covers the server rejecting the vault's
// credential, requiring one where none matched, or a credential this platform
// could not produce. The first two arrive alike as a 401 or 403 — a server
// answers the same whether a token was sent or not — so one test answers both.
func mcpAuthFailure(err error) bool {
	return errors.Is(err, mcp.ErrUnauthorized) || errors.Is(err, errCredentialUnusable)
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
