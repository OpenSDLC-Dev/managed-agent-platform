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
	PrefixResource      = "sesrsc"
	PrefixDeployment    = "depl"
	PrefixDeploymentRun = "drun"
	PrefixFile          = "file"
	PrefixSkill         = "skill"
)

// altSessionPrefix is accepted on input for wire compatibility: some Anthropic
// surfaces use "session_" instead of "sesn_". We normalize to PrefixSession on
// generation but recognize both.
const altSessionPrefix = "session"

// idAlphabet is Crockford base32 (lowercased), no padding. 15 random bytes
// encode to exactly 24 characters (120 bits / 5).
var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

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

// IsZero reports whether the ID is empty.
func (id ID) IsZero() bool { return id == "" }

func (id ID) String() string { return string(id) }
