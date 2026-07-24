package egress

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// PlaceholderPrefix marks the opaque tokens the platform injects into a sandbox
// in place of a vault secret. The reference calls the sandbox-visible value an
// "opaque placeholder" and defines no format; this prefix and the random suffix
// are ours (recorded as a deliberate divergence). The token is a valid
// environment-variable value — no spaces or shell metacharacters — so it injects
// cleanly through sandbox.Spec.Env.
const PlaceholderPrefix = "vltph_"

// NewPlaceholder mints a fresh opaque placeholder: the prefix plus 128 bits of
// randomness in hex. crypto/rand never fails on the platforms we target; a read
// error would be a broken system, so it panics rather than mint a guessable or
// empty token.
func NewPlaceholder() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("egress: crypto/rand failed: " + err.Error())
	}
	return PlaceholderPrefix + hex.EncodeToString(b[:])
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
type Credential struct {
	Placeholder string
	Secret      string
	Hosts       *HostSet
	Header      bool
	Body        bool
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
// placeholder. Resolution (a later slice) builds it from the store; the gate
// drives Substitute over each outbound request.
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
// secret, reaches the third party) and the credential is returned in
// unreachable so the caller can emit credential_host_unreachable_error. A
// placeholder whose credential is not enabled for loc is left literal and is
// not unreachable — the documented "a disabled injection_location is neither
// substituted nor stripped". Each unreachable credential is reported once.
func (e *Engine) Substitute(host string, loc Location, s string) (out string, unreachable []*Credential) {
	var pairs []string // placeholder, secret, … for the admitted credentials
	for i := range e.creds {
		c := &e.creds[i]
		if !c.enabled(loc) || !strings.Contains(s, c.Placeholder) {
			continue
		}
		if c.Hosts.Match(host) {
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
