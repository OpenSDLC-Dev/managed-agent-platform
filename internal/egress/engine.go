package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PlaceholderPrefix marks the opaque tokens the platform injects into a sandbox
// in place of a vault secret. The reference calls the sandbox-visible value an
// "opaque placeholder" and defines no format; this prefix and the derived suffix
// are ours (recorded as a deliberate divergence). The token is a valid
// environment-variable value — no spaces or shell metacharacters — so it injects
// cleanly through sandbox.Spec.Env.
const PlaceholderPrefix = "vltph_"

// Placeholder derives the opaque token the sandbox sees in place of the vault
// secret named secretName for the given session: the prefix plus 128 bits from
// SHA-256 of (sessionID, secretName), in hex.
//
// It is deterministic on purpose. The sandbox binds its environment at container
// create and keeps it across the idempotent re-provisions of a session
// (sandbox.Spec.Env is "fixed at create"), so every executor pass — and later
// the egress gate resolving live secret values — must derive the exact token
// already in the sandbox, not mint a fresh one that would no longer match.
// Stability is per (session, secret_name): a rotated credential (same
// secret_name, new secret) keeps its placeholder and the gate resolves the new
// value under it. The token is opaque and not itself a secret — the per-session
// gate only substitutes a session's own placeholders, and only for a host the
// credential's allowed_hosts admit, so correctness needs a stable derivation,
// not an unguessable one. The NUL separator keeps ("a","bc") from colliding with
// ("ab","c").
func Placeholder(sessionID, secretName string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + secretName))
	return PlaceholderPrefix + hex.EncodeToString(sum[:16])
}

// Location is where in an outbound request a placeholder was found — the two
// injection_location arms a credential can enable.
type Location int

const (
	LocationHeader Location = iota
	LocationBody
)

// Credential is one resolved environment-variable vault credential: its
// sandbox-visible Placeholder, the Secret it stands for, the hosts it may be
// used against, and which injection locations it is enabled for. Secrets live
// here only for the substitution call path — never logged, never stored.
//
// Unrestricted is the credential's own networking arm: when set, the secret may
// be substituted for any request host and Hosts is ignored (the credential was
// created with networking "unrestricted"). Otherwise Hosts is the limited
// allow-list — a nil/empty set matches nothing (fail-closed).
type Credential struct {
	Placeholder  string
	Secret       string
	Hosts        *HostSet
	Unrestricted bool
	Header       bool
	Body         bool
}

func (c *Credential) enabled(loc Location) bool {
	switch loc {
	case LocationHeader:
		return c.Header
	case LocationBody:
		return c.Body
	default:
		return false
	}
}

// Engine holds the resolved credentials for one substitution pass, keyed by
// placeholder. internal/vaultresolve reads them from the store and the control
// plane hands them to the gate (api/gateconfig.go), which builds an engine per
// session and drives Substitute over each outbound request.
type Engine struct {
	creds []Credential
}

// NewEngine builds an engine over a set of resolved credentials. The slice is
// small (a session's attached env-var credentials), so Substitute scans it
// directly rather than pre-indexing.
func NewEngine(creds []Credential) *Engine {
	return &Engine{creds: creds}
}

// Substitute rewrites s for a request bound to host, in injection location loc.
// A credential enabled for loc whose placeholder appears in s is substituted
// with its secret when host is admitted by the credential's allowed_hosts;
// when it is not, the placeholder is left literal (the opaque token, never the
// secret, reaches the third party — the reference's documented behavior, not an
// error) and the credential is returned in unreachable as a diagnostic; the
// wire-visible credential_host_unreachable_error is a config conflict the
// controlplane emits at gate-config render, never a per-request report. A
// placeholder whose credential is not enabled for loc is left literal and is
// not unreachable — the documented "a disabled injection_location is neither
// substituted nor stripped". Each unreachable credential is reported once.
//
// This path is uninstrumented: no span, no metric, no decision log. Plan 12
// intended a per-substitution span and it was never built, so an operator
// asking "why was my credential not substituted?" has only the gate's
// egress_request span, which carries the method and server address and no
// verdict. Said here rather than in that plan alone, because the absence is
// the answer to a question this function invites.
func (e *Engine) Substitute(host string, loc Location, s string) (out string, unreachable []*Credential) {
	var pairs []string // placeholder, secret, … for the admitted credentials
	for i := range e.creds {
		c := &e.creds[i]
		if !c.enabled(loc) || !strings.Contains(s, c.Placeholder) {
			continue
		}
		if c.Unrestricted || c.Hosts.Match(host) {
			pairs = append(pairs, c.Placeholder, c.Secret)
			continue
		}
		unreachable = append(unreachable, c)
	}
	if len(pairs) == 0 {
		return s, unreachable
	}
	// One left-to-right pass that never re-scans its own output, so a secret
	// that happens to contain another credential's placeholder is not itself
	// rewritten — the result is independent of credential order.
	return strings.NewReplacer(pairs...).Replace(s), unreachable
}
