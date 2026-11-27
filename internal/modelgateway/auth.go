package modelgateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/inferencetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

// caller keeps credential attribution and the principal authorizing inference.
type caller struct {
	workspaceID, workspacePublicID string
	keyID                          string // its api_keys id
	principalID                    string
	sessionID                      string // delegated calls use the claim's session, never a caller header
	delegated                      bool
	playground                     bool
}

// authenticate checks a delegated claim, CLI grant or owned key.
// x-api-key precedes Bearer; repeated fields are ambiguous and refused.
func (h *handler) authenticate(r *http.Request, path string, openAI bool) (caller, *apiError) {
	c, err := h.credential(r, path, openAI)
	if err != nil {
		return caller{}, err
	}
	if !workspace.Matches(r.Header.Values("anthropic-workspace-id"), c.workspacePublicID) {
		return caller{}, forbidden("workspace is not served by this platform")
	}
	r.Header.Del("anthropic-workspace-id")
	return c, nil
}

func (h *handler) credential(r *http.Request, path string, openAI bool) (caller, *apiError) {
	keys := r.Header.Values("x-api-key")
	if len(keys) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return caller{}, unauthenticated("a credential header is repeated")
	}
	key := ""
	if len(keys) == 1 {
		key = keys[0]
	} else if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key = tok
	}
	if key == "" {
		return caller{}, unauthenticated("x-api-key header is required")
	}
	if strings.HasPrefix(key, inferencetoken.TokenPrefix) {
		p, err := inferencetoken.Authenticate(r.Context(), h.cfg.Keys, key)
		if err != nil {
			return caller{}, internal(r, "inference token lookup failed", err)
		}
		if p.WorkID != "" {
			return caller{workspaceID: p.WorkspaceID, workspacePublicID: p.WorkspacePublicID, principalID: p.PrincipalID, sessionID: p.SessionID, delegated: true}, nil
		}
		return caller{}, unauthenticated("invalid inference token")
	}
	if len(keys) == 0 && strings.HasPrefix(key, clioauth.AccessPrefix) {
		return h.authenticateCLI(r, key, path, openAI)
	}
	// Read credential and current owner authority in one database snapshot.
	var c caller
	var role workspace.Role
	err := h.cfg.Keys.QueryRow(r.Context(), `SELECT k.id, p.id, w.id, w.public_id, coalesce(workspace_role(p.id,k.workspace_id),'')
		FROM api_keys k JOIN principals p ON p.id=k.owner_principal_id
		JOIN workspaces w ON (w.org_id,w.id)=(k.org_id,k.workspace_id) AND w.archived_at IS NULL
		WHERE k.owned_key_hash=$1 AND k.status='active' AND k.org_id='default'
		  AND (k.expires_at IS NULL OR k.expires_at > clock_timestamp())
		  AND p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'`,
		apikey.Hash(key)).Scan(&c.keyID, &c.principalID, &c.workspaceID, &c.workspacePublicID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return caller{}, unauthenticated("invalid x-api-key")
	}
	if err != nil {
		return caller{}, internal(r, "api key lookup failed", err)
	}
	if !role.CanUseResources() {
		return caller{}, forbidden("calling models requires the developer role")
	}
	return c, nil
}

func (h *handler) authenticateCLI(r *http.Request, token, path string, openAI bool) (caller, *apiError) {
	a, err := clioauth.Authenticate(r.Context(), h.cfg.Keys, token)
	if errors.Is(err, clioauth.ErrInvalidGrant) {
		return caller{}, unauthenticated("invalid OAuth token")
	}
	if err != nil {
		return caller{}, internal(r, "OAuth token lookup failed", err)
	}
	var refusal string
	switch {
	case openAI:
		return caller{}, forbidden("this OAuth token permits Anthropic inference only")
	case path == "/v1/messages":
		refusal = "OAuth token does not meet scope requirement any_of(org:service_key_inference, user:ccr_inference, user:developer, user:inference, user:voice, workspace:developer, workspace:inference, workspace:messages_create)"
	case path == "/v1/messages/count_tokens":
		refusal = "OAuth token does not meet scope requirement; one of [user:inference, user:developer, user:ccr_inference, org:service_key_inference, workspace:inference, workspace:messages_create, workspace:developer] is required"
	case path == "/v1/models" || strings.HasPrefix(path, "/v1/models/"):
		refusal = "OAuth token does not meet scope requirement any_of(user:ccr_inference, user:developer, user:inference, workspace:developer, workspace:inference)"
	default:
		return caller{}, forbidden("this OAuth token permits Anthropic inference only")
	}
	if !a.Has(clioauth.Inference) && !a.Has(clioauth.Developer) {
		return caller{}, forbidden("%s", refusal)
	}
	if !a.WorkspaceRole.CanUseResources() {
		return caller{}, forbidden("calling models requires the developer role")
	}
	return caller{principalID: a.PrincipalID, workspaceID: a.WorkspaceKey, workspacePublicID: a.WorkspaceID}, nil
}
