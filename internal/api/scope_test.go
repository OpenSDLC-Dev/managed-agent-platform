package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// coveredWorkspace builds the scope shape every resolver hands selectWorkspace:
// org and project frozen at 'default', the workspace the only varying member.
func coveredWorkspace(id string) domain.Scope {
	return domain.Scope{OrgID: "default", WorkspaceID: id, ProjectID: "default"}
}

const (
	wsAlpha = "wrkspc_0123456789abcdefghjkmnpq"
	wsBeta  = "wrkspc_rstvwxyz0123456789abcdef"
	// wsForeign is a well-formed id of a workspace the credential does not
	// cover; wsUnknown is well-formed and names nothing at all. The point of
	// the pair is that the answer cannot tell them apart.
	wsForeign = "wrkspc_aaaaaaaaaaaaaaaaaaaaaaaa"
	wsUnknown = "wrkspc_bbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestSelectWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		header      string
		covered     []domain.Scope
		wantID      string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:    "absent header with one covered workspace takes it",
			covered: []domain.Scope{coveredWorkspace("default")},
			wantID:  "default",
		},
		{
			name:        "absent header spanning two workspaces must select one",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha), coveredWorkspace(wsBeta)},
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header is required: this identity spans multiple workspaces and none was selected.",
		},
		{
			name:        "absent header covering nothing takes the same refusal",
			covered:     nil,
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header is required: this identity spans multiple workspaces and none was selected.",
		},
		{
			name:    "the literal default is a valid id here",
			header:  "default",
			covered: []domain.Scope{coveredWorkspace("default")},
			wantID:  "default",
		},
		{
			name:    "a covered wrkspc_ id narrows to that workspace",
			header:  wsBeta,
			covered: []domain.Scope{coveredWorkspace(wsAlpha), coveredWorkspace(wsBeta)},
			wantID:  wsBeta,
		},
		{
			name:        "a bare word is malformed",
			header:      "marketing",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header must be a valid workspace ID.",
		},
		{
			name:        "a token outside the id alphabet is malformed",
			header:      "wrkspc_NOT!AN!ID",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header must be a valid workspace ID.",
		},
		{
			name:        "another resource's prefix is malformed",
			header:      "env_0123456789abcdefghjkmnpq",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header must be a valid workspace ID.",
		},
		{
			name:        "an empty token is malformed",
			header:      "wrkspc_",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusBadRequest,
			wantType:    errTypeInvalidRequest,
			wantMessage: "anthropic-workspace-id header must be a valid workspace ID.",
		},
		{
			name:        "a workspace the credential does not cover is not found",
			header:      wsForeign,
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusNotFound,
			wantType:    errTypeNotFound,
			wantMessage: "Workspace `" + wsForeign + "` not found.",
		},
		{
			name:        "a workspace that does not exist is not found either",
			header:      wsUnknown,
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusNotFound,
			wantType:    errTypeNotFound,
			wantMessage: "Workspace `" + wsUnknown + "` not found.",
		},
		{
			name:        "the literal default is still not found when uncovered",
			header:      "default",
			covered:     []domain.Scope{coveredWorkspace(wsAlpha)},
			wantStatus:  http.StatusNotFound,
			wantType:    errTypeNotFound,
			wantMessage: "Workspace `default` not found.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectWorkspace(scopeRequest(tc.header), tc.covered)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("selectWorkspace: %v", err)
				}
				if got.WorkspaceID != tc.wantID {
					t.Errorf("workspace = %q, want %q", got.WorkspaceID, tc.wantID)
				}
				if got.OrgID != "default" || got.ProjectID != "default" {
					t.Errorf("scope = %+v, want org and project 'default'", got)
				}
				return
			}
			var ae *apiError
			if !errors.As(err, &ae) {
				t.Fatalf("selectWorkspace err = %v, want an apiError", err)
			}
			if ae.status != tc.wantStatus || ae.errType != tc.wantType {
				t.Errorf("error = %d %s, want %d %s", ae.status, ae.errType, tc.wantStatus, tc.wantType)
			}
			if ae.message != tc.wantMessage {
				t.Errorf("message = %q, want %q", ae.message, tc.wantMessage)
			}
			if got != (domain.Scope{}) {
				t.Errorf("scope on refusal = %+v, want the zero scope", got)
			}
		})
	}
}

// Existence must not leak: a real workspace the credential does not cover and
// one that was never created answer the same bytes, because they answer from
// the same code path.
func TestSelectWorkspaceHidesExistenceBehindOneBody(t *testing.T) {
	covered := []domain.Scope{coveredWorkspace(wsAlpha)}
	foreign := renderScopeError(t, wsForeign, covered)
	unknown := renderScopeError(t, wsUnknown, covered)
	// The ids differ, so compare with each echoed id normalised away.
	foreign = replaceOnce(t, foreign, wsForeign)
	unknown = replaceOnce(t, unknown, wsUnknown)
	if foreign != unknown {
		t.Errorf("foreign body %s differs from unknown body %s", foreign, unknown)
	}
}

func TestStampScopeSetsBothTenancyHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	stampScope(w, coveredWorkspace(wsAlpha))
	if got := w.Header().Get("anthropic-organization-id"); got != "default" {
		t.Errorf("anthropic-organization-id = %q, want %q", got, "default")
	}
	if got := w.Header().Get("anthropic-workspace-id"); got != wsAlpha {
		t.Errorf("anthropic-workspace-id = %q, want %q", got, wsAlpha)
	}
	if len(w.Header()) != 2 {
		t.Errorf("stampScope set %d headers (%v), want exactly the two tenancy ones", len(w.Header()), w.Header())
	}
}

// scopeRequest is a request carrying the workspace header, or none when v is
// empty — the absent arm.
func scopeRequest(v string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	if v != "" {
		r.Header.Set("anthropic-workspace-id", v)
	}
	return r
}

// renderScopeError runs a refusal through the package's own error writer, so
// the comparison is over the bytes a client receives rather than over fields.
func renderScopeError(t *testing.T, header string, covered []domain.Scope) string {
	t.Helper()
	r := scopeRequest(header)
	_, err := selectWorkspace(r, covered)
	if err == nil {
		t.Fatalf("selectWorkspace(%q) succeeded, want a refusal", header)
	}
	w := httptest.NewRecorder()
	writeError(w, r, err)
	return w.Body.String()
}

func replaceOnce(t *testing.T, body, id string) string {
	t.Helper()
	before, after, ok := strings.Cut(body, id)
	if !ok {
		t.Fatalf("body %s does not echo %q", body, id)
	}
	return before + "<id>" + after
}
