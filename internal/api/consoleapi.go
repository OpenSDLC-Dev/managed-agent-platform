package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The console API: the off-wire surface the managed-agent-console drives, served
// under /api/ rather than /v1/ so that nothing here can be mistaken for — or
// collide with — the wire-compatible surface a real `ant` CLI and the Anthropic
// SDKs speak. Its paths mirror the reference console's own private API
// path-for-path rather than being invented here, so a second console-facing
// endpoint has a convention to follow instead of a naming argument; the
// divergences from what was observed are declared in docs/DIVERGENCES.md.
//
// Auth needs no dispatcher change, for a reason worth stating precisely, since
// it is the whole argument for omitting an explicit /api/ arm: every non-
// management predicate in dispatchAuth is either a /v1/ prefix or segment test
// (the worker, session-events, skill-read and file-read lanes) or, in the gate's
// single case, exact equality against the fixed path "/internal/v1/gate/config"
// (isGateConfigPath, internal/api/gateauth.go) — which is not under /v1/ and is
// not a prefix test at all. No /api/ path can satisfy either form, so it falls to
// the management lane. A future off-/v1 lane must re-check this rather than
// assume the prefix rule covers everything.
//
// The console's BFF holds that x-api-key server-side; it never reaches a
// browser. There is no cryptographic way to confine the namespace to the console
// with a single management credential — "console-only" means off the wire and
// built for the console, and is documented as exactly that.

// The two console-API path patterns, mirrored segment-for-segment from the
// reference console's private API as observed on 2026-08-10 (docs/plan/30).
// They are constants because the 405 fallbacks must register the same strings
// the handlers do — a fallback registered against a drifted pattern is a 404
// where the house envelope promises a 405, and nothing else would catch it.
const (
	consoleTokensPath = "/api/oauth/organizations/{org}/environments/{id}/tokens"
	consoleRevokePath = consoleTokensPath + "/{token_id}/revoke"
)

// reservedOrganization is the only organization id v1 answers for. The segment
// exists because the reference's does and because org/workspace/project are this
// platform's reserved tenancy keys (principle 5); until they become real
// scoping, any other value names an organization that does not exist.
const reservedOrganization = "default"

// consoleOrganization resolves the {org} segment every console-API route carries,
// shared by both surfaces rather than spelled once each. The point of the 404 is
// that the namespace is no better an enumeration oracle than /v1 is, and that
// argument only holds if the two surfaces answer alike — which a second copy
// cannot guarantee once #56 makes org a real tenancy key and someone updates one
// of them.
func consoleOrganization(r *http.Request) error {
	if org := r.PathValue("org"); org != reservedOrganization {
		return errNotFound("organization %s not found", org)
	}
	return nil
}

// consoleKeyLimit is both the default and the maximum page size for the key
// listing — the reference console's own listing reported limit 100, and a
// deployment issuing more than a hundred keys to one environment can page. It is
// a literal rather than an alias of the wire surface's maxLimit: the two happen
// to agree today, but they are separate observations about separate contracts,
// and page.go's own maxEventLimit is the precedent for a cap differing per
// surface.
const consoleKeyLimit = 100

// environmentKeyNameMax bounds the operator's label, counted in characters
// rather than bytes — an operator naming a host in Chinese gets the same 128 an
// operator naming it in English does, and it is what the error message and the
// docs say. The dialect's own bound is unobserved, so the number is a local
// choice: long enough for a hostname or a "staging-eu-west-1 runner" phrase,
// short enough that a listing stays readable and a name is not bulk storage.
const environmentKeyNameMax = 128

// environmentKeyIssuedJSON is the issuance response: an RFC 6749 token response,
// the shape the reference console's private API returns. It carries no id, name
// or timestamps — a caller that wants the new row re-reads the list, exactly as
// the reference console does — and `access_token` is the only time the secret
// exists outside the database's hash of it.
type environmentKeyIssuedJSON struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// environmentKeyJSON is a key as the listing renders it. expires_at is nullable:
// a key minted before migration 0021 has none and never expires.
type environmentKeyJSON struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// environmentKeyPageJSON mirrors the observed listing envelope — data plus an
// offset-paginated block — rather than this platform's own keyset `next_page`
// envelope, which is the wire surface's and stays there.
type environmentKeyPageJSON struct {
	Data       []environmentKeyJSON `json:"data"`
	Pagination paginationJSON       `json:"pagination"`
}

type paginationJSON struct {
	Total   int  `json:"total"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"has_more"`
}

// consoleEnvironmentID resolves the {organization_id}/{environment_id} pair every
// console-API environment route addresses, without touching the database. An
// unrecognized organization and a malformed environment id answer with the same
// 404 shape an absent environment gets, so the namespace is no better an
// enumeration oracle than /v1 is.
func consoleEnvironmentID(r *http.Request) (string, error) {
	if err := consoleOrganization(r); err != nil {
		return "", err
	}
	id := r.PathValue("id")
	if err := checkID(id, "environment"); err != nil {
		return "", err
	}
	return id, nil
}

// consoleEnvironment is consoleEnvironmentID plus the existence check the two
// read-mostly routes need. Neither consults kind nor archive state: an operator
// must be able to see, and revoke, keys already issued to an environment whatever
// has happened to it since. Issuance does consult both, and reads them under a
// row lock instead — see createEnvironmentKey.
func (s *server) consoleEnvironment(r *http.Request) (string, error) {
	id, err := consoleEnvironmentID(r)
	if err != nil {
		return "", err
	}
	var exists bool
	err = s.pool.QueryRow(r.Context(),
		`SELECT true FROM environments WHERE id = $1`, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound("environment %s not found", id)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// createEnvironmentKey issues a worker credential and returns it once.
//
// Only a self_hosted environment gets one: a cloud environment's work is run by
// this platform's own executor, which consumes the queue in-process and holds no
// environment key, so issuing one there would hand an operator a credential
// nothing can use. The reference offers no key UI for its cloud environments
// either.
func (s *server) createEnvironmentKey(r *http.Request) (any, error) {
	envID, err := consoleEnvironmentID(r)
	if err != nil {
		return nil, err
	}
	// The body is read and validated before the transaction opens, so a slow or
	// hostile client cannot hold a row lock open for the length of its upload.
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "name"); err != nil {
		return nil, err
	}
	// Shared with the management-key surface (consoleapikeys.go): both are an
	// operator's label on a credential, read back in a console listing, and both
	// bound on environmentKeyNameMax. Two copies of the trim-and-measure would let
	// the two contracts drift the first time one of them gained a character-class
	// rule or a different bound.
	namePtr, err := consoleKeyName(obj, true)
	if err != nil {
		return nil, err
	}
	name := *namePtr

	// One transaction around check → insert, the idiom session create already
	// uses (internal/api/sessions.go): FOR SHARE on the environment row blocks a
	// concurrent archive or delete from slipping in between the two. Without it
	// an archive landing in that window mints a live key on an archived
	// environment — the 400 below, silently not enforced — and a delete turns the
	// insert's foreign key into a 500 where this route's own not-found branch is
	// the right answer.
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	var archivedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT kind, archived_at FROM environments WHERE id = $1 FOR SHARE`, envID).Scan(&kind, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("environment %s not found", envID)
	}
	if err != nil {
		return nil, err
	}
	if kind != string(domain.EnvSelfHosted) {
		return nil, errInvalid("environment %s is a %s environment; only a self_hosted environment runs a worker that authenticates with an environment key", envID, kind)
	}
	if archivedAt != nil {
		return nil, errInvalid("environment %s is archived", envID)
	}
	key, err := issueEnvironmentKey(ctx, tx, envID, name)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return environmentKeyIssuedJSON{
		AccessToken: key,
		ExpiresIn:   int(EnvironmentKeyTTL / time.Second),
	}, nil
}

// listEnvironmentKeys renders an environment's live keys.
func (s *server) listEnvironmentKeys(r *http.Request) (any, error) {
	envID, err := s.consoleEnvironment(r)
	if err != nil {
		return nil, err
	}
	limit, offset, err := parseOffsetPage(r.URL.Query())
	if err != nil {
		return nil, err
	}
	keys, total, err := ListEnvironmentKeys(r.Context(), s.pool, envID, limit, offset)
	if err != nil {
		return nil, err
	}
	data := make([]environmentKeyJSON, 0, len(keys))
	for _, k := range keys {
		data = append(data, environmentKeyJSON{
			ID:        k.ID,
			Name:      k.Name,
			CreatedAt: k.CreatedAt.UTC(),
			ExpiresAt: utcPtr(k.ExpiresAt),
		})
	}
	return environmentKeyPageJSON{
		Data: data,
		Pagination: paginationJSON{
			Total:   total,
			Limit:   limit,
			Offset:  offset,
			HasMore: offset+len(data) < total,
		},
	}, nil
}

// revokeEnvironmentKey retires one key. A key id belonging to another
// environment takes the same branch as one that never existed, so revocation can
// neither reach across environments nor confirm that an id exists elsewhere.
func (s *server) revokeEnvironmentKey(r *http.Request) error {
	envID, err := s.consoleEnvironment(r)
	if err != nil {
		return err
	}
	keyID := r.PathValue("token_id")
	// envkey_ is deliberately outside domain.knownPrefixes, so checkID cannot
	// answer for it: checkID validates shape without asking which resource a
	// prefix names, and admitting a private identifier there would widen the id
	// shape every /v1 path accepts. This is its local equivalent — the same
	// unstorable-byte class closed before the id binds into a query.
	if !domain.ValidWithPrefix(keyID, domain.PrefixEnvironmentKey) {
		return errNotFound("environment key not found")
	}
	found, err := RevokeEnvironmentKey(r.Context(), s.pool, envID, keyID)
	if err != nil {
		return err
	}
	if !found {
		return errNotFound("environment key not found")
	}
	return nil
}

// noStore forbids caching the response of the one route that hands back a
// credential. RFC 6749 §5.1 requires it of a token response, and this body is
// the plaintext's only appearance anywhere — a console BFF or reverse proxy
// with response retention on must not be the thing that keeps a second copy.
// The headers go on before the handler runs, so they are present whatever the
// outcome: a rejected request carries no secret, but a header set only on the
// success path is a header someone eventually gets wrong.
func noStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next(w, r)
	}
}

// parseOffsetPage reads the offset paging the mirrored dialect uses, in place of
// the wire surface's keyset cursors. Offset paging can repeat or skip a row
// across concurrent writes, which a keyset cursor cannot; it is what the
// reference console's listing does, and an operator's per-host key list is small
// enough that the difference never surfaces.
func parseOffsetPage(q url.Values) (limit, offset int, err error) {
	limit, offset = consoleKeyLimit, 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > consoleKeyLimit {
			return 0, 0, errInvalid("limit must be an integer between 1 and %d", consoleKeyLimit)
		}
		limit = n
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, 0, errInvalid("offset must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, nil
}
