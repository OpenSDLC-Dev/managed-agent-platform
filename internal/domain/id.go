// Package domain holds the Anthropic-native core types that are the single
// source of truth for the platform. Nothing in this package may depend on
// adk-go, genai, or any provider SDK — the wire schema of Anthropic Managed
// Agents is authoritative here.
package domain

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// ID is an opaque, prefixed identifier, e.g. "agent_9m3k…". The prefix is
// wire-compatible with Anthropic Managed Agents so the real `ant` CLI and
// Anthropic SDKs recognize our resources. Clients must treat the part after
// the prefix as opaque.
type ID string

// Resource ID prefixes, matching the Anthropic Managed Agents wire format.
const (
	PrefixAgent         = "agent"
	PrefixEnvironment   = "env"
	PrefixSession       = "sesn"
	PrefixEvent         = "sevt"
	PrefixWork          = "work"
	PrefixVault         = "vlt"
	PrefixCredential    = "vcrd"
	PrefixResource      = "sesrsc"
	PrefixDeployment    = "depl"
	PrefixDeploymentRun = "drun"
	PrefixFile          = "file"
	PrefixSkill         = "skill"
	PrefixSkillVersion  = "skillver"
	PrefixOutcome       = "outc"
	PrefixSessionThread = "sthr"
	// PrefixEnvironmentKey names an issued worker credential's row. It is
	// internal-only — never on the /v1 wire, and the reference identifies its
	// own environment keys by bare UUID on its console's private API — so it
	// stays out of knownPrefixes, following the apikey_/gtk_ precedent. That
	// set is what every /v1 path accepts as an id shape, and widening it for a
	// private identifier would widen all of them (checkID validates shape, not
	// which resource the prefix names).
	PrefixEnvironmentKey = "envkey"
	// PrefixPrincipal names an authenticated human's identity-bookkeeping row
	// (#56, plan 31). It joins the same private family for the same reason:
	// a principal id never appears on a /v1 path — it is written to
	// sessions.created_by for audit and never rendered — so admitting it to
	// knownPrefixes would widen the id shape every wire path accepts in order
	// to validate something no wire path ever receives.
	PrefixPrincipal = "principal"
	// PrefixAPIKey names a management credential's row. The reference uses the
	// same spelling — its console addressed a probe key as
	// `apikey_013EepdgX96Ux6op9hfWqjqJ` (#378) — but on its console's private API,
	// not on the wire, so this joins the private family too. It was a bare string
	// literal in internal/api until plan 32 gave the console a route that has to
	// validate one on a path; the two spellings are the same constant now.
	PrefixAPIKey = "apikey"
)

// altSessionPrefix is accepted on input for wire compatibility: some Anthropic
// surfaces use "session_" instead of "sesn_". We normalize to PrefixSession on
// generation but recognize both.
const altSessionPrefix = "session"

// idAlphabet is Crockford base32 (lowercased): the digits and lowercase letters
// minus i, l, o, u, no padding. It is both what NewID emits and what Valid
// accepts in a token, so a stored id and an accepted one cannot drift. 15 random
// bytes encode to exactly 24 characters (120 bits / 5).
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

// knownPrefixes is the resource-id prefix set the API accepts on a path or an
// id-shaped query parameter. altSessionPrefix is included so the session_ wire
// spelling validates alongside sesn_.
var knownPrefixes = map[string]bool{
	PrefixAgent: true, PrefixEnvironment: true, PrefixSession: true, PrefixEvent: true,
	PrefixWork: true, PrefixVault: true, PrefixCredential: true, PrefixResource: true,
	PrefixDeployment: true, PrefixDeploymentRun: true, PrefixFile: true,
	PrefixSkillVersion: true, PrefixSkill: true, PrefixOutcome: true,
	PrefixSessionThread: true, altSessionPrefix: true,
}

// PrimaryThreadID is the id of a session's primary thread: sthr_ plus the
// session id's own token (plan 35 decision 1) — deterministic, so any
// component can name it without a lookup, backfillable in SQL, valid in the
// id alphabet, and unique because session ids are. Clients treat it as
// opaque; the reference's derivation is unrecorded. The session_ wire
// spelling derives to the same id as its sesn_ form.
func PrimaryThreadID(sessionID ID) ID {
	_, token, _ := strings.Cut(string(sessionID), "_")
	return ID(PrefixSessionThread + "_" + token)
}

const idRandomBytes = 15

// NewID returns a fresh ID with the given prefix (use the Prefix* constants).
// It panics only if the system CSPRNG fails, which is not a recoverable
// condition for a server that must mint identifiers.
func NewID(prefix string) ID {
	b := make([]byte, idRandomBytes)
	if _, err := rand.Read(b); err != nil {
		panic("domain: crypto/rand failed: " + err.Error())
	}
	return ID(prefix + "_" + idEncoding.EncodeToString(b))
}

// Prefix returns the portion before the first underscore, or "" if there is none.
func (id ID) Prefix() string {
	if i := strings.IndexByte(string(id), '_'); i >= 0 {
		return string(id)[:i]
	}
	return ""
}

// HasPrefix reports whether id carries the given resource prefix. The Session
// prefix additionally accepts the alternate "session_" form for wire compat.
func (id ID) HasPrefix(prefix string) bool {
	p := id.Prefix()
	if p == prefix {
		return true
	}
	return prefix == PrefixSession && p == altSessionPrefix
}

// Valid reports whether id is a well-formed resource identifier: a known
// prefix, an underscore, and a non-empty token drawn only from idAlphabet — the
// exact shape NewID emits, plus the session_ wire spelling. Clients only ever
// hold ids the server minted, so a value failing this cannot name a stored row.
// The API rejects such an id on shape (a 404 on a path, a 400 on a query
// filter) before it reaches a bind parameter, where an unstorable byte (U+0000,
// invalid UTF-8) — or any non-alphabet byte — would otherwise fail as a 500
// (Postgres SQLSTATE 22021) rather than the status the wire expects.
func (id ID) Valid() bool {
	prefix, token, ok := strings.Cut(string(id), "_")
	return ok && knownPrefixes[prefix] && validToken(token)
}

// ValidWithPrefix is Valid for an identifier this platform mints but never puts
// on the /v1 wire — envkey_ and its kin, which stay out of knownPrefixes so that
// admitting one cannot widen the id shape every wire path accepts. It answers
// the same question against a prefix the caller names: the exact shape NewID
// emits. Callers off the wire use it to reject a malformed id before it binds
// into a query, which is the whole reason Valid exists.
func ValidWithPrefix(id, prefix string) bool {
	p, token, ok := strings.Cut(id, "_")
	return ok && p == prefix && validToken(token)
}

// validToken holds the rule both spellings share: a non-empty token drawn only
// from idAlphabet. One copy, so the two entry points cannot drift on what an
// acceptable id body is.
func validToken(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		if strings.IndexByte(idAlphabet, token[i]) < 0 {
			return false
		}
	}
	return true
}

// IsZero reports whether the ID is empty.
func (id ID) IsZero() bool { return id == "" }

func (id ID) String() string { return string(id) }
