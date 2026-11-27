package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	consoleWorkspaceArchivePath   = consoleWorkspacePath + "/archive"
	consoleWorkspaceKeyOwnersPath = consoleWorkspacePath + "/service_accounts"
	consoleWorkspaceMembersPath   = consoleWorkspacePath + "/members"
	consoleWorkspaceMemberPath    = consoleWorkspaceMembersPath + "/{member_id}"
	workspaceColumns              = "public_id, name, created_at, archived_at, display_color"
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

// Recorded archive requests may have no body. Reject unrelated mutation fields.
func emptyWorkspaceMutation(r *http.Request) error {
	if r.ContentLength == 0 {
		return nil
	}
	_, err := managementBody(r)
	return err
}

func (s *server) archiveWorkspace(r *http.Request) (any, error) {
	if err := emptyWorkspaceMutation(r); err != nil {
		return nil, err
	}
	ctx := r.Context()
	space := workspace.ID(ctx)
	tx, err := s.beginWorkspaceArchive(r, space)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE workspace_id=$1 AND archived_at IS NULL AND status<>'terminated' ORDER BY id`, space)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var moves []domain.SessionStatus
	for _, id := range ids {
		children, err := terminateLiveChildren(ctx, tx, s.log, id)
		if err != nil {
			return nil, err
		}
		moves = append(moves, children...)
		sid := domain.ID(id)
		batch, transition, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{Status: domain.SessionTerminated})
		if err != nil {
			return nil, err
		}
		if _, err = s.log.AppendInTx(ctx, tx, sid, batch, events.AppendOptions{SetStatus: transition}); err != nil {
			return nil, err
		}
		if transition != nil {
			moves = append(moves, *transition)
		}
	}
	if len(ids) > 0 {
		if err := events.NotifyReapKick(ctx, tx); err != nil {
			return nil, err
		}
	}
	w, err := scanWorkspace(tx.QueryRow(ctx, `UPDATE workspaces SET archived_at=clock_timestamp()
 WHERE id=$1 AND archived_at IS NULL RETURNING `+workspaceColumns, space))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, status := range moves {
		events.RecordSessionStatus(ctx, status)
	}
	return w, nil
}

// A worker holds its session while acquiring the workspace fence to enqueue its next turn.
// When a session is busy, release all archive locks before waiting for that worker, then
// recheck the caller and enumerate again so newly committed sessions are included.
func (s *server) beginWorkspaceArchive(r *http.Request, space string) (pgx.Tx, error) {
	ctx := r.Context()
	for {
		tx, _, err := s.beginPrincipalMutation(r)
		if err != nil {
			return nil, err
		}
		busy, err := lockArchiveSessions(ctx, tx, space)
		if err == nil && busy == "" {
			return tx, nil
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := s.pool.Exec(ctx, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, busy); err != nil {
			return nil, err
		}
	}
}

func lockArchiveSessions(ctx context.Context, tx pgx.Tx, space string) (string, error) {
	if space == workspace.Default {
		return "", errInvalid("Default workspace cannot be archived")
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, space).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errWorkspaceNotFound
		}
		return "", err
	}
	// Closed sessions can still have a final harvest in flight.
	err := tx.QueryRow(ctx, `WITH targets AS MATERIALIZED (
		SELECT s.id FROM sessions s WHERE s.workspace_id=$1 AND (
			(s.archived_at IS NULL AND s.status<>'terminated') OR EXISTS (
				SELECT FROM work_items w WHERE w.session_id=s.id AND w.state<>'stopped'))
	), locked AS (
		SELECT s.id FROM sessions s JOIN targets t ON t.id=s.id FOR UPDATE OF s SKIP LOCKED
	)
	SELECT id FROM targets EXCEPT SELECT id FROM locked LIMIT 1`, space).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

type workspaceMember struct {
	ID        string `json:"id"`
	Kind      string `json:"type"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Inherited bool   `json:"inherited"`
}

// workspaceKeyOwners lists eligible service accounts without organization directory metadata.
func (s *server) workspaceKeyOwners(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	q, err := managementQuery(r, "page", "limit")
	if err != nil {
		return nil, err
	}
	for key := range q {
		if key != "page" && key != "limit" {
			return nil, errInvalid("unsupported key-owner query parameter %q", key)
		}
	}
	space := workspace.ID(r.Context())
	p, err := parsePageWith(space, q, 20, 100)
	if err != nil {
		return nil, err
	}
	var at *time.Time
	var id string
	if p.cur != nil {
		if p.cur.foreignToTime(cursorServiceAccounts) || p.cur.dir != dirNext || !domain.WellFormedID(p.cur.id, domain.PrefixPrincipal) {
			return nil, errPageCursor
		}
		at, id = &p.cur.t, p.cur.id
	}
	rows, err := s.pool.Query(r.Context(), `SELECT p.id,coalesce(nullif(p.display_name,''),p.subject),p.created_at FROM principals p
 WHERE p.org_id='default' AND p.kind='service_account' AND p.status='active'
 AND workspace_role(p.id,$1) IN ('limited_developer','developer','admin')
 AND ($2::timestamptz IS NULL OR (p.created_at,p.id)<($2,$3)) ORDER BY p.created_at DESC,p.id DESC LIMIT $4`, space, at, id, p.limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	page := struct {
		Data []candidate `json:"data"`
		Next *string     `json:"next_page"`
	}{Data: []candidate{}}
	var lastID string
	var lastTime time.Time
	for rows.Next() {
		if len(page.Data) == p.limit {
			next := encodeTimeCursor(space, cursorServiceAccounts, dirNext, lastTime, lastID)
			page.Next = &next
			break
		}
		var item candidate
		if err := rows.Scan(&lastID, &item.Name, &lastTime); err != nil {
			return nil, err
		}
		item.ID = principal.PublicID(lastID, "service_account")
		page.Data = append(page.Data, item)
	}
	return page, rows.Err()
}

func (s *server) workspaceMembers(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT p.id,p.kind,coalesce(nullif(p.display_name,''),nullif(p.email,''),CASE WHEN p.kind='service_account' THEN p.subject ELSE '' END),
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
		if m.Name == "" {
			m.Name = m.ID
		}
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
	if err := emptyWorkspaceMutation(r); err != nil {
		return nil, err
	}
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
