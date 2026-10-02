package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The management-key console routes, mirrored segment-for-segment from the
// reference console's private API as recorded live on 2026-08-13 (#378).
//
// The prefix is `/api/console/`, not plan 30's `/api/oauth/`: the reference uses
// both, and each surface keeps the one it was observed under. Constants for the
// same reason consoleTokensPath is one — the 405 fallbacks must register the
// string the handlers do, and a drifted pattern is a 404 where the house
// envelope promises a 405.
//
// A key is updated at two paths. consoleOrgAPIKeyPath is the reference's own,
// with no workspace segment (2026-09-05 batch5 `rec86.keys.update.*`);
// consoleAPIKeyPath, under the workspace, is the one this platform served
// first and keeps as an alias, which the console still drives.
const (
	consoleWorkspacePath = "/api/console/organizations/{org}/workspaces/{workspace}"
	consoleAPIKeysPath   = consoleWorkspacePath + "/api_keys"
	consoleAPIKeyPath    = consoleAPIKeysPath + "/{key_id}"
	consoleOrgAPIKeyPath = "/api/console/organizations/{org}/api_keys/{key_id}"
)

// apiKeyNameMax bounds a management key's name, in characters: the bound the
// reference was recorded enforcing (2026-09-05 batch5 `rec86.create.name.500`
// passes, `.501` and `.1000` are refused).
const apiKeyNameMax = 500

// reservedWorkspace is the only workspace id this platform answers for, beside
// reservedOrganization. The segment is carried because the reference carries it
// and because `workspace_id` is already a reserved tenancy column (principle 5):
// a seam, not an implementation. Until it becomes real scoping, any other value
// names a workspace that does not exist.
const reservedWorkspace = "default"

// actorJSON renders the reference's `{id, type}` actor. Its own vocabulary for
// type is `user`; ours is `principal` or `api_key`, because we have no `user_`
// id to give — a divergence, registered in docs/DIVERGENCES.md.
type actorJSON struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// apiKeyJSON is a management key as both the listing and the update render it,
// field-for-field from the recorded resource, in its order.
//
// Three fields are deliberately not what the reference's public schema shows.
// `workspace_id` and `principal` are emitted null — which is mirroring rather
// than reserving-by-guess, because on the reference's own single-tenant account
// both came back null (#378), so #56 can populate them without a shape change.
// `can_manage`, which the listing carries and the created resource does not, is
// omitted entirely: these routes are admin-only, so the field would be a
// constant true — a per-row authorization hint for a permission split we have
// not built. Both registered in docs/DIVERGENCES.md.
type apiKeyJSON struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	Name           string     `json:"name"`
	WorkspaceID    *string    `json:"workspace_id"`
	CreatedAt      time.Time  `json:"created_at"`
	CreatedBy      *actorJSON `json:"created_by"`
	PartialKeyHint string     `json:"partial_key_hint"`
	Status         string     `json:"status"`
	ExpiresAt      *time.Time `json:"expires_at"`
	Principal      *actorJSON `json:"principal"`
}

// apiKeyIssuedJSON is the create response: the whole resource with one extra
// field. That is the recorded shape, and it is **not** the shape plan 30 gave
// environment-key issuance (`{access_token, expires_in}`, RFC 6749) — the
// reference runs two dialects on two surfaces and we mirror each where it
// belongs. Embedding rather than repeating the fields keeps the two renderings
// from drifting, and puts raw_key last, where the recording has it.
type apiKeyIssuedJSON struct {
	apiKeyJSON
	RawKey string `json:"raw_key"`
}

// apiKeyResourceType is the resource discriminator the reference emits.
const apiKeyResourceType = "api_key"

// The actor vocabulary, which is a SEPARATE wire concept from the resource
// discriminator above even though one of its values is spelled the same. The
// reference's actor union has a single member, `user`; ours has two, because we
// have no `user_` id to give. Binding actorMachine to apiKeyResourceType would
// mean that matching the resource discriminator to a future recording silently
// rewrote every machine issuer's actor type in the same edit.
const (
	actorMachine = "api_key"
	actorHuman   = "principal"
)

// actorFor renders an id as an actor, or nil for the absent issuer a key seeded
// from CONTROLPLANE_API_KEY carries. The type is read from the id's own prefix,
// so the two can never disagree. An empty string is treated as absent for
// safety, though IssueManagementKey refuses to write one.
func actorFor(id *string) *actorJSON {
	if id == nil || *id == "" {
		return nil
	}
	kind := actorMachine
	if strings.HasPrefix(*id, domain.PrefixPrincipal+"_") {
		kind = actorHuman
	}
	return &actorJSON{ID: *id, Type: kind}
}

func renderAPIKey(k ManagementKey) apiKeyJSON {
	return apiKeyJSON{
		ID:             k.ID,
		Type:           apiKeyResourceType,
		Name:           k.Name,
		WorkspaceID:    nil,
		CreatedAt:      k.CreatedAt.UTC(),
		CreatedBy:      actorFor(k.CreatedBy),
		PartialKeyHint: k.PartialKeyHint,
		Status:         k.Status,
		ExpiresAt:      utcPtr(k.ExpiresAt),
		Principal:      nil,
	}
}

// consoleWorkspace resolves the {org}/{workspace} pair the workspace-scoped
// routes here address, without touching the database. The organization is
// consoleOrganization's; an unrecognized workspace is a 404 carrying the
// reference's `{error_visibility}` (2026-09-05 batch8
// `item6.after-archive.workspaceB.api_keys`), so the namespace is no better an
// enumeration oracle than /v1 is.
func consoleWorkspace(r *http.Request) error {
	if err := consoleOrganization(r); err != nil {
		return err
	}
	if ws := r.PathValue("workspace"); ws != reservedWorkspace {
		return errWorkspaceNotFound(ws)
	}
	return nil
}

// errWorkspaceNotFound is the reference's recorded 404 for a workspace, words
// included (2026-09-05 batch8 `item6.after-archive.workspaceB.get` and
// `.api_keys`; #540): it does not name the workspace.
func errWorkspaceNotFound(string) error {
	return withDetails(errNotFound("Not found"), userFacingDetails)
}

// getWorkspace answers GET …/workspaces/{workspace} with the 404 the reference
// was recorded answering an archived workspace with (2026-09-05 batch8
// `item6.after-archive.workspaceB.get`), for every id, `default` included.
// This platform holds no workspace to render: the recorded workspace object is
// a `wrkspc_` row whose display color, data residency and compartment id have
// no value here, and the reserved `default` is no such row — nor is the
// reference's own Default workspace a row of its workspace listing (batch9
// `item5.workspaces.list.include_archived`).
func (s *server) getWorkspace(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	return nil, errWorkspaceNotFound(r.PathValue("workspace"))
}

// createAPIKey issues a management credential and returns it once.
func (s *server) createAPIKey(r *http.Request) (any, error) {
	if err := consoleWorkspace(r); err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	// A pydantic surface on the reference, whose unknown-key sentence no
	// recording holds, so it is not rejectUnknownKeys' strict-decoder one.
	if key, ok := firstUnknownKey(obj, "name", "expires_at", "principal_id"); ok {
		return nil, errInvalid("unknown field %q", key)
	}
	name, err := apiKeyCreateName(obj)
	if err != nil {
		return nil, err
	}
	expiresAt, err := apiKeyExpiry(obj)
	if err != nil {
		return nil, err
	}
	if err := apiKeyPrincipal(obj); err != nil {
		return nil, err
	}
	// The issuer, from whichever lane authenticated: a `principal_` id for a human
	// over SSO, the machine key's own `apikey_` row id otherwise. This route is
	// authenticated on both lanes, so it is never empty — and it must not be, since
	// a NULL issuer is what marks a row env-var-managed.
	key, row, err := IssueManagementKey(r.Context(), s.pool, *name, expiresAt, principalFrom(r.Context()))
	if err != nil {
		return nil, err
	}
	return apiKeyIssuedJSON{apiKeyJSON: renderAPIKey(row), RawKey: key}, nil
}

// listAPIKeys renders every management key as a bare JSON array — no envelope,
// no paging, which is what the reference's console list returns. The wire
// surface's `{data, next_page}` envelope stays on the wire surface, and the
// public Admin API's `{data, first_id, has_more, last_id}` is a third shape for
// the same resource that we do not serve at all.
func (s *server) listAPIKeys(r *http.Request) (any, error) {
	if err := consoleWorkspace(r); err != nil {
		return nil, err
	}
	keys, err := ListManagementKeys(r.Context(), s.pool)
	if err != nil {
		return nil, err
	}
	out := make([]apiKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, renderAPIKey(k))
	}
	return out, nil
}

// apiKeyPatch decodes and validates an update body: the status and name it
// sets, either nil when the body leaves it alone.
func apiKeyPatch(raw json.RawMessage) (status, name *string, err error) {
	obj, err := decodeBodyObject(raw)
	if err != nil {
		return nil, nil, err
	}
	if key, ok := firstUnknownKey(obj, "status", "name"); ok {
		return nil, nil, errInvalid("unknown field %q", key)
	}
	if status, err = apiKeyStatus(obj); err != nil {
		return nil, nil, err
	}
	if name, err = apiKeyName(obj, false); err != nil {
		return nil, nil, err
	}
	return status, name, nil
}

// updateAPIKey is the update on the reference's own route, which names the
// organization and no workspace.
func (s *server) updateAPIKey(r *http.Request) (any, error) {
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	return s.updateAPIKeyIn(r)
}

// updateWorkspaceAPIKey is the same update on the workspace-scoped alias.
func (s *server) updateWorkspaceAPIKey(r *http.Request) (any, error) {
	if err := consoleWorkspace(r); err != nil {
		return nil, err
	}
	return s.updateAPIKeyIn(r)
}

// updateAPIKeyIn changes a key's status, its name, or both, once the caller
// has resolved the route's scope.
//
// The transaction exists for the check below, not for the write: the row is read
// FOR UPDATE so a concurrent update cannot change what is being decided on
// between the decision and the write.
func (s *server) updateAPIKeyIn(r *http.Request) (any, error) {
	keyID := r.PathValue("key_id")
	// apikey_ is deliberately outside domain.knownPrefixes, so checkID cannot
	// answer for it — the same reasoning revokeEnvironmentKey states for envkey_.
	// This is its local equivalent, closing the unstorable-byte class before the
	// id binds into a query, and answering the reference's 400. It runs before
	// the body is read, as the reference refuses the id whatever the body says.
	if !consoleIDShape(keyID, domain.PrefixAPIKey) {
		return nil, withDetails(errInvalid("API Key id must have `apikey_` prefix."), userFacingDetails)
	}
	// The body is read and judged before any row is locked, so neither a slow
	// upload nor an invalid body holds the lock. Its refusal is reported only
	// once the key is known to exist, because the reference answers a key that
	// does not exist with its 404 whatever the body says (2026-09-05 batch5
	// `rec86.keys.update.wellformed-id.bad-field`; #664) — save a body too large
	// to read, which readBody refuses first.
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	notFound := withDetails(errNotFound("API Key `%s` not found.", keyID), userFacingDetails)
	ctx := r.Context()
	status, name, bodyErr := apiKeyPatch(raw)
	if bodyErr != nil {
		var exists bool
		err := s.pool.QueryRow(ctx, `SELECT true FROM api_keys WHERE id = $1`, keyID).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound
		}
		if err != nil {
			return nil, err
		}
		return nil, bodyErr
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createdBy *string
	var current string
	var lapsed bool
	err = tx.QueryRow(ctx,
		`SELECT created_by, status, (expires_at IS NOT NULL AND expires_at <= now())
		 FROM api_keys WHERE id = $1 FOR UPDATE`, keyID).Scan(&createdBy, &current, &lapsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	// An empty patch is NOT refused. It used to be — "at least one of status, name
	// is required" — which read like a helpful guard and is not what the reference
	// does: `POST …/{id} {}` there answers 200 with the unchanged resource (#389).
	// The state guards below still apply to it, so an empty patch aimed at an
	// archived or lapsed row is refused for that reason rather than for its shape,
	// which is the ordering the reference's own messages imply.

	// The env-var-managed refusal comes FIRST, before the archived one, because a
	// rotated deployment holds archived rows with no issuer — EnsureAPIKey archives
	// the incumbent on every rotation — and for those the environment variable is
	// both the more fundamental fact and the only actionable one. Ordered the other
	// way, an admin patching such a row is told "archived is permanent, use
	// inactive", follows that advice, is refused again, and is never told the thing
	// they could act on.
	//
	// The rule itself: a key nobody issued belongs to CONTROLPLANE_API_KEY and this
	// route does not get to touch it. Its lifecycle already has an owner —
	// rotation-by-restart — so a console disable would be silently undone by the
	// next boot. And renaming it would break that rotation outright: EnsureAPIKey
	// archives the incumbent by *name*, so a bootstrap row renamed out from under it
	// would survive the next rotation and leave two live credentials, which is
	// exactly the race api_keys_one_live_unissued exists to prevent. The row is
	// still listed — hiding it would be a worse lie than refusing to mutate it.
	if createdBy == nil {
		return nil, errInvalid("api key %s is managed by CONTROLPLANE_API_KEY; rotate it by restarting the control plane with a new value", keyID)
	}
	// Archived is terminal, and NOTHING may be patched onto an archived row — the
	// repeated archive included. The reference refuses all five shapes with one
	// message, "Archived API keys cannot be updated." (#389, measured on two
	// independent keys). An earlier round of #388 made the repeated archive a
	// succeeding no-op, reasoning that a retried Delete should not error; the
	// reasoning is sound in the abstract and simply not what this API does.
	//
	// Terminality is also what keeps `archived` and `inactive` from meaning the
	// same thing: `inactive` exists to be undone, and if `archived` could be undone
	// an operator who retired a key would have no way to rely on it while its
	// plaintext may sit in a leaked backup one request from working. Migration 0024
	// states the same rule, mapping every `revoked_at` row to archived because
	// "revocation was one-way, and archived is the one-way state".
	//
	// Terminal *on this surface*. EnsureAPIKey still adopts an archived row whose
	// plaintext is configured as CONTROLPLANE_API_KEY, which is deliberate and
	// logged — see the warning it emits. That is the environment variable claiming
	// a value, not the console undoing an archive, and it needs the deployment
	// access that could equally configure a fresh key.
	//
	// Both refusals are worded as the #389 probe measured them (docs/HISTORY.md),
	// since #540 — they used to name the key and say "archived" for the
	// reference's "deleted", on the argument that an operator could act on it.
	if current == KeyStatusArchived {
		return nil, errInvalid("Archived API keys cannot be updated.")
	}
	// A lapsed key admits exactly one operation: archiving it. The reference states
	// the rule in the refusal itself — "Expired API keys can only be deleted, not
	// renamed or reactivated" — where "deleted" is `status: archived`, since it
	// serves no DELETE verb (that route answers 405). Re-activating, disabling and
	// renaming are all 400 there; this platform used to permit the last two, on the
	// argument that cleanup must not depend on the clock. Only the archive does.
	//
	// `lapsed` is read from expires_at ALONE, not from the derived status. Anding in
	// a status — which the rendering does, since archived outranks expired there —
	// left an earlier version of this guard reachable around: disable a key, let it
	// lapse, re-enable it, and the flag was false because the row was `inactive` at
	// the moment it was read. The row is what expired; which state it sits in while
	// that happened does not change the answer here.
	if lapsed && !(name == nil && status != nil && *status == KeyStatusArchived) {
		return nil, errInvalid("Expired API keys can only be deleted, not renamed or reactivated.")
	}
	row, err := updateManagementKey(ctx, tx, keyID, status, name)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderAPIKey(row), nil
}

// apiKeyName parses a management key's name as the reference was recorded
// judging it: 1 to apiKeyNameMax characters, as sent. Nothing is trimmed — a
// whitespace-only name passes there and is stored and echoed as sent
// (2026-09-05 batch5 `rec86.create.name.whitespace`, `.tab-nl`; 2026-09-24
// `api-key`). The refusal's message is the recorded one. A rename is held to
// the same rule, which no recording shows.
//
// Returns nil when the field is absent, which only the update path allows.
func apiKeyName(obj map[string]json.RawMessage, required bool) (*string, error) {
	name, err := consoleKeyName(obj, required)
	if err != nil || name == nil {
		return name, err
	}
	if utf8.RuneCountInString(*name) > apiKeyNameMax {
		return nil, errInvalid("name: String should have at most %d characters", apiKeyNameMax)
	}
	return name, nil
}

// apiKeyCreateName is apiKeyName for a create, refusing a missing, non-string
// or empty name in pydantic's words, as the reference was recorded refusing
// each (2026-09-05 batch5 `rec86.create.name.missing`, `.wrong-type`,
// `.empty`; #540). A null name keeps ours, and so does a rename's refusal:
// neither was recorded.
func apiKeyCreateName(obj map[string]json.RawMessage) (*string, error) {
	raw, ok := obj["name"]
	switch {
	case !ok:
		return nil, errInvalid("name: Field required")
	case isNull(raw):
		return apiKeyName(obj, true)
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return nil, errInvalid("name: Input should be a valid string")
	}
	if name == "" {
		return nil, errInvalid("name: String should have at least 1 character")
	}
	return apiKeyName(obj, true)
}

// apiKeyPrincipal judges create's optional `principal_id`, the reference's way
// to link a key to an identity, as far as the recordings reach. Its shape is
// recorded: a `user_` or `svac_` id, anything else a 400 with
// `{error_visibility}` (2026-09-05 batch5 `rec86.create.principal_id.bogus-string`).
// What a well-formed one does is not, and needs an identity this platform
// does not have — no users and no service accounts, its humans being
// `principal_` rows that shape refuses — so a well-formed id names nothing
// here and is the namespace's 404 for an id that names nothing. An absent or
// null field is no principal, the only kind of key this platform issues.
func apiKeyPrincipal(obj map[string]json.RawMessage) error {
	raw, ok := obj["principal_id"]
	if !ok || isNull(raw) {
		return nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return errInvalid("principal_id must be a string")
	}
	if !consoleIDShape(id, "user") && !consoleIDShape(id, "svac") {
		return withDetails(errInvalid("principal_id must be a user_... (user) or svac_... (service account) ID."), userFacingDetails)
	}
	return withDetails(errNotFound("principal %s not found", id), userFacingDetails)
}

// consoleKeyName reads an operator's label on either console surface: present,
// a string, and not empty. What else bounds it is each surface's own —
// environmentKeyName's rule and apiKeyName's.
//
// Returns nil when the field is absent, which only the update path allows.
func consoleKeyName(obj map[string]json.RawMessage, required bool) (*string, error) {
	if raw, ok := obj["name"]; !ok {
		if !required {
			return nil, nil
		}
	} else if isNull(raw) && !required {
		// On a PATCH, absent means "leave it alone", so an explicit null is a
		// distinct thing the caller said and cannot mean the same — and a name
		// cannot be cleared, since every key carries one the listing renders.
		//
		// On a create the two DO coincide: a null name is a missing name, and
		// `requiredString` below already says so in the words the environment-key
		// surface has always used. Keeping that branch untouched is deliberate —
		// sharing this helper with createEnvironmentKey must not quietly reword an
		// error message on a surface plan 30 already shipped.
		return nil, errInvalid("name cannot be null")
	}
	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	return &name, nil
}

// apiKeyStatus parses the settable status, or nil when absent.
//
// `expired` gets its own message rather than falling into the generic one,
// because an operator reaching for it has a coherent intention — retire this
// key — and the useful answer names the state that does it. The reference
// rejects `expired` too, its server having answered a `deleted` probe with
// "status: Input should be 'active', 'inactive' or 'archived'"; we reject the
// same set and write our own text, since that message is framework-generated
// validation output and ours can say more.
func apiKeyStatus(obj map[string]json.RawMessage) (*string, error) {
	raw, ok := obj["status"]
	if !ok {
		return nil, nil
	}
	// An explicit null is its own answer, not a missing field. wire.go keeps
	// absent/null/value distinct because the distinction is semantic on a patch,
	// and `requiredString` folds all three into "status is required" — which would
	// tell a caller who supplied the field that they had not. There is nothing to
	// clear here (a key always has a status), so null is invalid; the message just
	// has to say which of the two things went wrong.
	if isNull(raw) {
		return nil, errInvalid("status cannot be null; omit it to leave the status unchanged")
	}
	status, err := requiredString(obj, "status")
	if err != nil {
		return nil, err
	}
	if status == KeyStatusExpired {
		return nil, errInvalid("status %q is derived from expires_at and cannot be set; use %q to retire a key",
			KeyStatusExpired, KeyStatusArchived)
	}
	settable := []string{KeyStatusActive, KeyStatusInactive, KeyStatusArchived}
	if !slices.Contains(settable, status) {
		return nil, errInvalid("status must be one of %s", strings.Join(settable, ", "))
	}
	return &status, nil
}

// apiKeyExpiry parses the optional absolute instant a key expires at.
//
// Absent means never, which is the recorded contract from both ends: the
// console's "Never" **omits the field** rather than sending null, and the
// response then reports expires_at: null. An explicit null is accepted for the
// same meaning, and that is no longer the extrapolation this comment used to
// call it: #389 posted `"expires_at": null` to the reference and got 200 with
// `expires_at: null` back, so the step from the response shape to the request
// shape is now an observation. The INFERRED entry that once registered it is
// gone from docs/DIVERGENCES.md along with the other four — there is nothing
// left to infer here, and nothing left to diverge on.
//
// There is no duration vocabulary, deliberately. The reference's dialog offers
// "3 hours / 30 days / Custom N units", and every one of those is client-side
// sugar resolved to an absolute instant before it crosses the wire.
//
// A past instant is accepted, and mints a key already reporting `expired`. This
// platform refused it until #389 measured the reference, which answers 200 — the
// argument for refusing (a validation message beats debugging an authentication
// failure) lost to the one that governs here: the reference's behaviour is the
// source of truth, and a key born expired is refused by the credential path from
// its first request, so nothing unsafe is minted.
func apiKeyExpiry(obj map[string]json.RawMessage) (*time.Time, error) {
	raw, ok := obj["expires_at"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	s, err := requiredString(obj, "expires_at")
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errInvalid("expires_at must be an RFC 3339 timestamp")
	}
	return &t, nil
}
