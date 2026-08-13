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
const (
	consoleAPIKeysPath = "/api/console/organizations/{org}/workspaces/{workspace}/api_keys"
	consoleAPIKeyPath  = consoleAPIKeysPath + "/{key_id}"
)

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

// consoleWorkspace resolves the {org}/{workspace} pair every route here
// addresses, without touching the database. An unrecognized value on either
// segment answers with the same 404 shape, so the namespace is no better an
// enumeration oracle than /v1 is.
func consoleWorkspace(r *http.Request) error {
	if err := consoleOrganization(r); err != nil {
		return err
	}
	if ws := r.PathValue("workspace"); ws != reservedWorkspace {
		return errNotFound("workspace %s not found", ws)
	}
	return nil
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
	if err := rejectUnknownKeys(obj, "name", "expires_at"); err != nil {
		return nil, err
	}
	name, err := consoleKeyName(obj, true)
	if err != nil {
		return nil, err
	}
	expiresAt, err := apiKeyExpiry(obj)
	if err != nil {
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

// updateAPIKey changes a key's status, its name, or both.
//
// The transaction exists for the check below, not for the write: the row is read
// FOR UPDATE so a concurrent update cannot change what is being decided on
// between the decision and the write.
func (s *server) updateAPIKey(r *http.Request) (any, error) {
	if err := consoleWorkspace(r); err != nil {
		return nil, err
	}
	keyID := r.PathValue("key_id")
	// apikey_ is deliberately outside domain.knownPrefixes, so checkID cannot
	// answer for it — the same reasoning revokeEnvironmentKey states for envkey_.
	// This is its local equivalent, closing the unstorable-byte class before the
	// id binds into a query.
	if !domain.ValidWithPrefix(keyID, domain.PrefixAPIKey) {
		return nil, errNotFound("api key not found")
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "status", "name"); err != nil {
		return nil, err
	}
	status, err := apiKeyStatus(obj)
	if err != nil {
		return nil, err
	}
	name, err := consoleKeyName(obj, false)
	if err != nil {
		return nil, err
	}
	// An empty patch is NOT refused. It used to be — "at least one of status, name
	// is required" — which read like a helpful guard and is not what the reference
	// does: `POST …/{id} {}` there answers 200 with the unchanged resource (#389).
	// The state guards below still apply to it, so an empty patch aimed at an
	// archived or lapsed row is refused for that reason rather than for its shape,
	// which is the ordering the reference's own messages imply.

	ctx := r.Context()
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
		return nil, errNotFound("api key %s not found", keyID)
	}
	if err != nil {
		return nil, err
	}
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
	if current == KeyStatusArchived {
		return nil, errInvalid("api key %s is archived, and an archived key cannot be updated", keyID)
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
		return nil, errInvalid("api key %s has expired; an expired key can only be archived, not renamed or re-activated", keyID)
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

// consoleKeyName parses the operator's label. Bounds are environmentKeyNameMax's,
// reused rather than re-chosen: both are a console label an operator reads back
// in a list, and the dialect's own bound is unobserved on either surface. Reusing
// the other surface's number is a local choice, not evidence about this one, so
// it is registered in docs/DIVERGENCES.md as plan 30 registered its own.
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
	// Trim before measuring and before storing: a name is a label an operator
	// reads back in a list, and one that is all whitespace names nothing.
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > environmentKeyNameMax {
		return nil, errInvalid("name must be 1-%d characters", environmentKeyNameMax)
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
// same meaning — nothing observed forbids it, and refusing a spelling the
// response itself uses would be a gratuitous asymmetry, since a client
// round-tripping a fetched resource back into a create would be rejected for
// echoing what it was given. That step from the response shape to the request
// shape is an extrapolation, not an observation, and is registered INFERRED in
// docs/DIVERGENCES.md.
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
