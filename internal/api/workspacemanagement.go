package api

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	consoleWorkspaceArchivePath = consoleWorkspacePath + "/archive"
	consoleWorkspaceMembersPath = consoleWorkspacePath + "/members"
	consoleWorkspaceMemberPath  = consoleWorkspaceMembersPath + "/{member_id}"
	workspaceColumns            = "public_id, name, created_at, archived_at, display_color"
)

var displayColor = regexp.MustCompile("^#[0-9a-fA-F]{6}$")

type privateWorkspace struct {
	auditWorkspace
	InferenceDataRetention bool `json:"inference_data_retention"`
}

func scanWorkspace(row pgx.Row) (privateWorkspace, error) {
	w := privateWorkspace{auditWorkspace: auditWorkspace{Type: "workspace", Tags: map[string]string{}}}
	err := row.Scan(&w.ID, &w.Name, &w.CreatedAt, &w.ArchivedAt, &w.DisplayColor)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, errWorkspaceNotFound
	}
	return w, err
}

func (s *server) listWorkspaces(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	session, err := requireCookieSession(r)
	if err != nil {
		return nil, err
	}
	q, err := managementQuery(r, "include_archived")
	if err != nil {
		return nil, err
	}
	archived, err := parseBoolParam(q, "include_archived")
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+workspaceColumns+` FROM workspaces
 WHERE org_id='default' AND id<>'default' AND ($2 OR archived_at IS NULL)
 AND ($3 OR workspace_role($1,id)='admin') ORDER BY lower(name),public_id`, session.PrincipalID, archived, session.Role == identity.RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (privateWorkspace, error) { return scanWorkspace(row) })
}

func (s *server) getWorkspace(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	return scanWorkspace(s.pool.QueryRow(r.Context(), `SELECT `+workspaceColumns+` FROM workspaces
 WHERE id=$1 AND id<>'default' AND archived_at IS NULL`, workspace.ID(r.Context())))
}

func workspaceFields(r *http.Request, create bool) (*string, *string, error) {
	obj, err := managementBody(r, "name", "display_color")
	if err != nil {
		return nil, nil, err
	}
	var name, color *string
	n, set, null, err := stringField(obj, "name")
	if err != nil {
		return nil, nil, err
	}
	if set || create {
		n = strings.TrimSpace(n)
		if null || n == "" || utf8.RuneCountInString(n) > 128 {
			return nil, nil, errInvalid("name must contain 1 to 128 characters")
		}
		name = &n
	}
	c, set, null, err := stringField(obj, "display_color")
	if err != nil {
		return nil, nil, err
	}
	if set {
		if null || !displayColor.MatchString(c) {
			return nil, nil, errInvalid("display_color must be a six-digit hex color")
		}
		color = &c
	}
	if name == nil && color == nil {
		return nil, nil, errInvalid("specify name or display_color")
	}
	return name, color, nil
}

func (s *server) createWorkspace(r *http.Request) (any, error) {
	name, color, err := workspaceFields(r, true)
	if err != nil {
		return nil, err
	}
	tx, _, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	var raw [16]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := "wrkspc_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	w, err := scanWorkspace(tx.QueryRow(r.Context(), `INSERT INTO workspaces(id,public_id,name,display_color)
 VALUES($1,$1,$2,coalesce($3,'#6C5BB9')) RETURNING `+workspaceColumns, id, name, color))
	if err != nil {
		return nil, err
	}
	return w, tx.Commit(r.Context())
}

func (s *server) editWorkspace(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	ctx := r.Context()
	space := workspace.ID(ctx)
	if space == workspace.Default {
		return nil, errInvalid("Default workspace settings are protected")
	}
	name, color, err := workspaceFields(r, false)
	if err != nil {
		return nil, err
	}
	tx, _, err := s.beginWorkspaceCredentialMutation(r, identity.RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	w, err := scanWorkspace(tx.QueryRow(ctx, `UPDATE workspaces SET name=coalesce($2,name),display_color=coalesce($3,display_color)
 WHERE id=$1 AND archived_at IS NULL RETURNING `+workspaceColumns, space, name, color))
	if err != nil {
		return nil, err
	}
	return w, tx.Commit(ctx)
}

func (s *server) archiveWorkspace(r *http.Request) (any, error) {
	tx, _, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	defer tx.Rollback(ctx)
	space := workspace.ID(ctx)
	if space == workspace.Default {
		return nil, errInvalid("Default workspace cannot be archived")
	}
	w, err := scanWorkspace(tx.QueryRow(ctx, `UPDATE workspaces SET archived_at=clock_timestamp()
 WHERE id=$1 AND archived_at IS NULL RETURNING `+workspaceColumns, space))
	if err != nil {
		return nil, err
	}
	return w, tx.Commit(ctx)
}

type workspaceMember struct {
	ID        string `json:"id"`
	Kind      string `json:"type"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Inherited bool   `json:"inherited"`
}

func (s *server) workspaceMembers(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT p.id,p.kind,coalesce(nullif(p.display_name,''),nullif(p.email,''),p.subject),
 workspace_role(p.id,$1),p.role='admin' OR (p.role='billing' AND workspace_role(p.id,$1)='billing')
 FROM principals p WHERE p.org_id='default' AND workspace_role(p.id,$1) IS NOT NULL ORDER BY 3,p.id`, workspace.ID(r.Context()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (workspaceMember, error) {
		var m workspaceMember
		err := row.Scan(&m.ID, &m.Kind, &m.Name, &m.Role, &m.Inherited)
		m.ID = principal.PublicID(m.ID, m.Kind)
		return m, err
	})
}

func (s *server) grantWorkspaceMember(r *http.Request) (any, error) {
	obj, err := managementBody(r, "principal_id", "role")
	if err != nil {
		return nil, err
	}
	alias, err := requiredString(obj, "principal_id")
	if err != nil {
		return nil, err
	}
	role, err := requiredString(obj, "role")
	if err != nil {
		return nil, err
	}
	if role != string(workspace.User) && !workspace.Role(role).CanUseResources() {
		return nil, errInvalid("unsupported workspace role")
	}
	return s.changeWorkspaceMember(r, alias, &role)
}

func (s *server) removeWorkspaceMember(r *http.Request) (any, error) {
	return s.changeWorkspaceMember(r, r.PathValue("member_id"), nil)
}

func (s *server) changeWorkspaceMember(r *http.Request, alias string, role *string) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	id, kind, ok := principal.ParsePublicID(alias)
	if !ok {
		return nil, errNotFound("Not found")
	}
	ctx := r.Context()
	space := workspace.ID(ctx)
	tx, _, err := s.beginWorkspaceCredentialMutation(r, identity.RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var orgRole string
	err = tx.QueryRow(ctx, `SELECT role FROM principals WHERE id=$1 AND kind=$2 AND org_id='default' AND status='active'`, id, kind).Scan(&orgRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("Not found")
	}
	if err != nil {
		return nil, err
	}
	if err := setWorkspaceGrant(ctx, tx, space, id, orgRole, role); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, tx.Commit(ctx)
}
