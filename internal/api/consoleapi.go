package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/unknownkey"
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
//
// The reference refuses this very value: its segment is a UUID, and `default`
// is its 400 (2026-09-05 batch2 `rec83.edge6.literal-default-org`). Keeping it
// is a registered divergence — it is the reserved key every console path here
// is built on, and the console sends nothing else.
const reservedOrganization = "default"

// consoleOrganization resolves the {org} segment every console-API route carries,
// shared by both surfaces rather than spelled once each, so the two answer alike
// once #56 makes org a real tenancy key. Every value but `default` names an
// organization this platform does not serve, and is answered as the reference
// answers one it will not serve you: a UUID is its 401 with `{error_visibility}`
// (`rec83.edge6.foreign-org-uuid`), anything else its 400 for a segment that is
// not a UUID, without details (`.literal-default-org`). Both come before
// anything is looked up, so the segment cannot probe environment or key ids.
// Both are worded as recorded (#540); a non-UUID the uuid crate would refuse
// for something other than a stray character never was, and keeps ours.
func consoleOrganization(r *http.Request) error {
	org := r.PathValue("org")
	switch {
	case org == reservedOrganization:
		return nil
	case isUUID(org):
		return withDetails(errAuth("Unable to authenticate session."), userFacingDetails)
	default:
		if why, ok := uuidInvalidCharacter(org); ok {
			return errInvalid("path.organization_uuid: Input should be a valid UUID, %s", why)
		}
		return errInvalid("%q is not an organization id", org)
	}
}

// consoleKeyLimit is the default page size for the key listing, the reference
// console's own (2026-09-05 batch2 `rec83.edge5.list.no-params`). It is no
// maximum: the reference takes `limit=101` and `limit=1000` and echoes them
// (`rec83.edge5.list.limit.101`, `.1000`), and so does parseOffsetPage.
const consoleKeyLimit = 100

// environmentKeyNameMax bounds the operator's label, counted in characters
// rather than bytes — an operator naming a host in Chinese gets the same 128 an
// operator naming it in English does, and it is what the error message and the
// docs say. The dialect's own bound is unobserved, so the number is a local
// choice: long enough for a hostname or a "staging-eu-west-1 runner" phrase,
// short enough that a listing stays readable and a name is not bulk storage.
const environmentKeyNameMax = 128

// environmentKeyName parses an environment key's label: trimmed before it is
// measured and stored, since a name is a label an operator reads back in a
// list and one that is all whitespace names nothing. That rule, like the
// bound, is ours; the management-key surface's recorded rule is apiKeyName's.
func environmentKeyName(obj map[string]json.RawMessage) (string, error) {
	raw, err := consoleKeyName(obj, true)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(*raw)
	if name == "" || utf8.RuneCountInString(name) > environmentKeyNameMax {
		return "", errInvalid("name must be 1-%d characters", environmentKeyNameMax)
	}
	return name, nil
}

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
// console-API environment route addresses, without touching the database. The
// organization is consoleOrganization's: a foreign UUID is the reference's 401
// and any other value but `default` its 400, so it says nothing about which
// environments exist. Neither does an id that cannot be an environment's, the
// reference's 400 too; a well-formed one is the caller's to look up.
func consoleEnvironmentID(r *http.Request) (string, error) {
	if err := consoleOrganization(r); err != nil {
		return "", err
	}
	id := r.PathValue("id")
	if !consoleIDShape(id, domain.PrefixEnvironment) {
		return "", errConsoleEnvironmentMalformed
	}
	return id, nil
}

// consoleIDShape is how this namespace tells a malformed id from an unknown
// one: the prefix, then a non-empty token of bytes Postgres can store. It is
// not domain.Valid, which holds an id to the alphabet this platform
// mints. The reference's ids are not in that alphabet — env_01MQbDnwtRB9MBhtuxWAHq1M
// and apikey_01ABCDEFGHJKMNPQRSTVWXYZ were answered 404, not 400 (2026-09-05
// batch8 idx 22, batch5 idx 10–12; #664) — so the alphabet cannot be what
// makes an id malformed here. The storable-bytes rule is what keeps the id out
// of a query that would fail on it. Session create reads a memory store id by
// it too, for the same recorded reason (parseMemoryResource).
func consoleIDShape(id, prefix string) bool {
	token, ok := strings.CutPrefix(id, prefix+"_")
	return ok && token != "" && storableText(token)
}

// isUUID reports whether s is a UUID in one of the four forms the reference's
// parser takes. Its refusal of a key id — "invalid character: expected an
// optional prefix of `urn:uuid:` followed by [0-9a-fA-F-]" (2026-09-05 batch2
// `rec83.edge4.revoke.malformed-id`) — is the Rust uuid crate's, whose parser
// accepts exactly 32 hex digits bare, 36 grouped 8-4-4-4-12 by hyphens, that
// grouping in braces (38), or it after a lowercase `urn:uuid:` (45); hex in
// either case (uuid 1.8.0 and 1.10.0, src/parser.rs try_parse). That reading
// is an inference from the error string, registered in docs/DIVERGENCES.md.
// github.com/google/uuid's Parse is not used: it is only an indirect dependency
// here, and it is looser on two counts — any case for the prefix, and any
// first and last byte in place of the braces.
func isUUID(s string) bool {
	switch {
	case len(s) == 32:
		return isHex(s)
	case len(s) == 38 && s[0] == '{' && s[37] == '}':
		s = s[1:37]
	case len(s) == 45 && s[:9] == "urn:uuid:":
		s = s[9:]
	case len(s) != 36:
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' &&
		isHex(s[:8]+s[9:13]+s[14:18]+s[19:23]+s[24:])
}

// uuidInvalidCharacter is the Rust uuid crate's refusal of a value holding a
// character no UUID can, which the reference's pydantic layer quotes after
// "Input should be a valid UUID, " (2026-09-05 batch2
// `rec83.edge4.revoke.malformed-id`, `rec83.edge6.literal-default-org`): the
// first character that is neither a hyphen nor a hex digit, and its 1-based
// position in the value as sent, counting a stripped `{` or `urn:uuid:`
// (uuid 1.10.0, src/error.rs InvalidUuid::into_err). ok is false for a value
// the crate refuses on another ground — invalid UTF-8, a wrong length or
// grouping — whose wording no recording holds.
func uuidInvalidCharacter(s string) (why string, ok bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	body, offset := s, 0
	switch {
	case len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}':
		body, offset = s[1:len(s)-1], 1
	case strings.HasPrefix(s, "urn:uuid:"):
		body, offset = s[len("urn:uuid:"):], len("urn:uuid:")
	}
	for i, c := range body {
		if c != '-' && (c >= utf8.RuneSelf || !isHex(string(c))) {
			return fmt.Sprintf("invalid character: expected an optional prefix of `urn:uuid:` "+
				"followed by [0-9a-fA-F-], found `%c` at %d", c, i+offset+1), true
		}
	}
	return "", false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// errConsoleEnvironmentMalformed is the 400 the reference answers an id without
// the env_ shape with, details and words included (2026-09-05 batch2
// `rec83.edge3.issue.malformed-env`; #664, #540). The id it names is not
// echoed, as the reference's is not.
var errConsoleEnvironmentMalformed = withDetails(errInvalid("Invalid request: Environment id must have `env_` prefix."),
	errorDetails{ErrorVisibility: visibilityUserFacing, ErrorCode: "invalid_request"})

// errConsoleEnvironmentNotFound is this namespace's 404 for an environment it
// will not answer for, with the details the reference attaches to it here
// (2026-09-05 batch2 `rec83.edge3.issue.unknown-env`; #664) and the words every
// environment 404 has (errEnvironmentNotFound). The /v1 routes' 404 for the
// same environment stays without details, as the reference's does (2026-09-02
// batch2 `env.archive.with-deployment`).
func errConsoleEnvironmentNotFound(id string) error {
	return withDetails(errEnvironmentNotFound(id),
		errorDetails{ErrorVisibility: visibilityUserFacing, ErrorCode: "environment_not_found"})
}

// errEnvironmentKeyNotFound is revocation's one not-found branch, an unknown key
// and another environment's alike, with the details and the words the
// reference answers both with (2026-09-05 batch2
// `rec83.edge4.revoke.unknown-uuid` and `.cross-environment`; #664, #540).
func errEnvironmentKeyNotFound() error {
	return withDetails(errNotFound("Token not found"), userFacingDetails)
}

// userFacingDetails is what the reference attaches to most of this namespace's
// refusals: a foreign organization (consoleOrganization) and an unknown
// environment key (above); and on the management-key surface
// (consoleapikeys.go), an unknown workspace on its key routes and on its own
// read, an unknown API key, an id without the apikey_ prefix, and a
// principal_id that is not a user_ or svac_ id — 2026-09-05 batch2
// `rec83.edge6.foreign-org-uuid` and `rec83.edge4.revoke.unknown-uuid`, batch5
// `rec86.keys.update.wellformed-id.*`, `.bogus-id.*` and
// `rec86.create.principal_id.bogus-string`, batch8
// `item6.after-archive.workspaceB.api_keys` and `.get`; #664, #820. The 404 a
// well-formed principal_id gets carries it too, by inference: no recording
// names one (apiKeyPrincipal).
var userFacingDetails = errorDetails{ErrorVisibility: visibilityUserFacing}

// consoleEnvironment is consoleEnvironmentID plus the existence check the two
// read-mostly routes need. Neither consults kind nor archive state: an operator
// must be able to see, and revoke, keys already issued to an environment whatever
// has happened to it since. Issuance reads the row under a lock instead — see
// createEnvironmentKey.
//
// notInternal because off the wire is not off the rule (plan 41 §4.4): the dream
// runner's own environment is hidden from every public surface, and a console
// listing that answered for it would confirm the row the /v1 routes refuse to.
func (s *server) consoleEnvironment(r *http.Request) (string, error) {
	id, err := consoleEnvironmentID(r)
	if err != nil {
		return "", err
	}
	var exists bool
	err = s.pool.QueryRow(r.Context(),
		`SELECT true FROM environments WHERE id = $1`+notInternal, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errConsoleEnvironmentNotFound(id)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// createEnvironmentKey issues a worker credential and returns it once.
//
// Any environment gets one, a `cloud` or an archived one included, as the
// reference issues on both (2026-09-05 batch2 `rec83.edge1.issue.on-cloud-env`
// and `rec83.edge2.issue.on-archived-env`). A cloud environment's key cannot
// take that environment's work: the platform executor runs it in-process, and
// the work API refuses such a key's poll and listing as the reference does
// (pollWork, listWork) beneath a queue whose poll serves self_hosted items
// alone (queue.Poll). An archived self_hosted environment's key polls on, so
// a worker can drain what the archive left queued.
//
// The row read carries notInternal for the reason consoleEnvironment's does:
// the dream runner's hidden environment is refused as absent.
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
	// A pydantic surface on the reference, whose unknown-key sentence no
	// recording holds, so it is not rejectUnknownKeys' strict-decoder one.
	if key, ok := unknownkey.Least(obj, "name"); ok {
		return nil, errInvalid("unknown field %q", key)
	}
	name, err := environmentKeyName(obj)
	if err != nil {
		return nil, err
	}

	// One transaction around check → insert, the idiom session create already
	// uses (internal/api/sessions.go): FOR SHARE on the environment row blocks a
	// concurrent delete from slipping in between the two, which would turn the
	// insert's foreign key into a 500 where this route's own not-found branch is
	// the right answer.
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM environments WHERE id = $1`+notInternal+` FOR SHARE`, envID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errConsoleEnvironmentNotFound(envID)
	}
	if err != nil {
		return nil, err
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
	if err := consoleOrganization(r); err != nil {
		return err
	}
	keyID := r.PathValue("token_id")
	// envkey_ is deliberately outside domain.knownPrefixes, so checkID cannot
	// answer for it: checkID validates shape without asking which resource a
	// prefix names, and admitting a private identifier there would widen the id
	// shape every /v1 path accepts. This is its local equivalent — the same
	// unstorable-byte class closed before the id binds into a query. A key id is
	// one of ours or one of the reference's, which are UUIDs; one that is
	// neither gets the reference's 400, which carries no details, and the
	// recorded UUID that names nothing its 404 (2026-09-05 batch2
	// `rec83.edge4.revoke.malformed-id` and `.unknown-uuid`; #664).
	//
	// It runs before the environment is resolved. The reference refuses a
	// malformed key id as path validation — pydantic's `path.token_uuid`, with
	// no details — and a malformed environment id in its handler, with details
	// (`rec83.edge3.issue.malformed-env`), so the key id's shape is judged first,
	// and a malformed one costs no query.
	if !consoleIDShape(keyID, domain.PrefixEnvironmentKey) && !isUUID(keyID) {
		if why, ok := uuidInvalidCharacter(keyID); ok {
			return errInvalid("path.token_uuid: Input should be a valid UUID, %s", why)
		}
		return errInvalid("%q is not an environment key id", keyID)
	}
	envID, err := s.consoleEnvironment(r)
	if err != nil {
		return err
	}
	found, err := RevokeEnvironmentKey(r.Context(), s.pool, envID, keyID)
	if err != nil {
		return err
	}
	if !found {
		return errEnvironmentKeyNotFound()
	}
	return nil
}

// noStore forbids caching the response of a route that hands back a
// credential — the console's key issuance, and the work poll whose `secret`
// carries a sessions token. RFC 6749 §5.1 requires it of a token response,
// and this body is the plaintext's only appearance anywhere — a console BFF
// or reverse proxy with response retention on must not be the thing that
// keeps a second copy.
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
//
// Its refusals are pydantic's, as recorded (2026-09-05 batch2
// `rec83.edge5.list.limit.abc`, `.limit.0`, `.offset.-1`; #540); a non-integer
// offset, never recorded, takes the non-integer limit's sentence by analogy.
func parseOffsetPage(q url.Values) (limit, offset int, err error) {
	limit, offset = consoleKeyLimit, 0
	if s := q.Get("limit"); s != "" {
		if limit, err = pydanticInt("limit", s, 1); err != nil {
			return 0, 0, err
		}
	}
	if s := q.Get("offset"); s != "" {
		if offset, err = pydanticInt("offset", s, 0); err != nil {
			return 0, 0, err
		}
	}
	return limit, offset, nil
}

// pydanticInt parses one integer query parameter with a lower bound, refusing
// it in pydantic's words. Its lax reading of a string is inferred, not
// recorded: surrounding whitespace is stripped, as Python's int() strips it —
// which also reads a query's `+5`, decoded to " 5", as 5 — and an integer
// past any machine integer is still an integer, its bound checked like any
// other: one over the minimum saturates at math.MaxInt, for the caller to cap,
// and one under it is the minimum's sentence. A float or underscore spelling
// ("5.0", "1_000") is not taken, with no evidence that pydantic takes it here.
func pydanticInt(field, s string, min int) (int, error) {
	n, ok, overflow := parseDecimalInt(strings.TrimSpace(s))
	switch {
	case !ok:
		return 0, errPydanticInt(field)
	case overflow && n > 0, n > math.MaxInt:
		return math.MaxInt, nil
	case overflow, n < int64(min):
		return 0, errPydanticMin(field, min)
	}
	return int(n), nil
}

// errPydanticInt and errPydanticMin are pydanticInt's two sentences.
func errPydanticInt(field string) error {
	return errInvalid("%s: Input should be a valid integer, unable to parse string as an integer", field)
}

func errPydanticMin(field string, min int) error {
	return errInvalid("%s: Input should be greater than or equal to %d", field, min)
}
