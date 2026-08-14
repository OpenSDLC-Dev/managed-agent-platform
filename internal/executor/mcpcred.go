package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
)

// mcpBearer resolves the bearer token this session's attached vaults register
// for endpoint, or "" when they register none — in which case the dial goes out
// unauthenticated, which is what the reference documents for a server no
// credential matches.
//
// Resolved at every connect, on both dial paths, rather than once per session:
// the rows are read fresh each time, so a rotated token, an archived credential
// and an archived vault all reach the next dial without a restart. That is the
// re-resolution the reference describes, and it costs one indexed read per dial.
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
		return "", fmt.Errorf("the credential registered for this server could not be resolved: %w", err)
	}
	if cred == nil {
		return "", nil
	}
	return cred.Token, nil
}

// mcpDialReason renders a failed dial or listing into the reason a catalog row
// stores, saying which of the wire's two failures it was.
//
// The reference splits them by cause rather than by symptom:
// `mcp_connection_failed_error` is a server that "could not be reached (network
// error, timeout, or non-authentication HTTP failure)", while
// `mcp_authentication_failed_error` covers the server rejecting the vault's
// credential *and* requiring one where none matched. Both of those arrive here
// as a 401 or a 403 — the same status whether a token was sent or not — so one
// test answers both, and an operator reading the row is told to look at the
// credential rather than at the network.
func mcpDialReason(err error) string {
	if errors.Is(err, mcp.ErrUnauthorized) {
		return "authentication failed: " + err.Error()
	}
	return err.Error()
}
