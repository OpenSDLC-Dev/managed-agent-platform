package modelgateway

import (
	"errors"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
)

const (
	playgroundModelsPath   = "/api/organizations/default/proxy/v1/models"
	playgroundMessagesPath = "/api/organizations/default/proxy/v1/messages"
)

func (h *handler) servePlayground(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	m := h.cfg.Login
	if m == nil || !m.NativeEnabled() || len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("x-api-key")) != 0 {
		writeError(w, r, unauthenticated("a browser session cookie is required"))
		return
	}
	s, err := m.Authenticate(r.Context(), r)
	switch {
	case errors.Is(err, login.ErrWorkspaceSelector):
		writeError(w, r, forbidden("this organization or workspace is not served"))
		return
	case errors.Is(err, login.ErrUnauthenticated):
		w.Header().Set("x-platform-signed-out", "1")
		writeError(w, r, unauthenticated("invalid or expired browser session"))
		return
	case errors.Is(err, principal.ErrDeactivated):
		w.Header().Set("x-platform-signed-out", "1")
		writeError(w, r, forbidden("this account has been removed from the organization"))
		return
	case err != nil:
		writeError(w, r, internal(r, "Playground session lookup failed", err))
		return
	}
	if m.CheckCSRF(r, s) != nil {
		writeError(w, r, forbidden("invalid browser origin or CSRF token"))
		return
	}
	if !s.PlaygroundAccess {
		writeError(w, r, forbidden("Playground access requires administrator approval"))
		return
	}
	if values := r.Header.Values("x-organization-uuid"); len(values) > 1 || len(values) == 1 && values[0] != "default" {
		writeError(w, r, forbidden("this organization or workspace is not served"))
		return
	}
	if s.WorkspaceChoices > 1 && r.Header.Get("x-workspace-id") == "" {
		writeError(w, r, forbidden("select a workspace"))
		return
	}
	r = r.WithContext(workspace.WithID(r.Context(), s.WorkspaceKey))
	c := caller{workspaceID: s.WorkspaceKey, principalID: s.PrincipalID, playground: true}
	if r.URL.Path == playgroundModelsPath {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, r, notAllowed(r.Method, http.MethodGet))
			return
		}
		h.listModels(w, r, c)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, notAllowed(r.Method, http.MethodPost))
		return
	}
	// Only the model protocol headers belong on the vendor request.
	ctx, stop := m.WatchAuthority(w, r, func(session login.Session) bool { return session.PlaygroundAccess })
	defer stop()
	request := r.Clone(ctx)
	request.Header = http.Header{
		"Anthropic-Version": r.Header.Values("Anthropic-Version"),
		"Anthropic-Beta":    r.Header.Values("Anthropic-Beta"),
	}
	h.inference(w, request, c, "/v1/messages")
}
