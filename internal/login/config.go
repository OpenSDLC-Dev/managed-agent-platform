package login

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Mode includes the development-only password lane without changing the existing
// identity verifier's modes or its receiver-first deployment behavior.
type Mode string

const (
	ModeDisabled     Mode = "disabled"
	ModePassword     Mode = "password"
	ModeOIDC         Mode = "oidc"
	ModeTrustedProxy Mode = "trusted_proxy"
)

// Config carries native browser-login settings. Only the control plane needs
// OIDCClientSecret; a gateway reads SessionConfigFromEnv instead.
type Config struct {
	Mode             Mode
	PublicOrigin     string
	Development      bool
	OIDCClientID     string
	OIDCClientSecret string `json:"-" yaml:"-" xml:"-"`
}

// String, GoString and LogValue exclude the client secret. Format also covers
// mismatched diagnostic verbs; its comment states fmt's interception limits.
func (c Config) String() string {
	return fmt.Sprintf("login.Config{Mode:%q, PublicOrigin:%q, Development:%t, OIDCClientID:%q}", c.Mode, c.PublicOrigin, c.Development, c.OIDCClientID)
}
func (c Config) GoString() string     { return c.String() }
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Format redacts method-interceptable fmt verbs on values and pointers. fmt
// bypasses methods for %p on a struct value and for unexported nested value
// fields; those paths are not covered. %p on a pointer prints only its address.
func (c Config) Format(state fmt.State, _ rune) { io.WriteString(state, c.String()) }

// ConfigFromEnv parses control-plane login settings. It does not replace
// identity.ConfigFromEnv: that parser still validates the IdP verification lane.
// An absent public origin preserves verifier-only OIDC/trusted-proxy installs.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg, err := SessionConfigFromEnv(getenv)
	if err != nil {
		return Config{}, err
	}
	if cfg.Mode == ModeOIDC && cfg.PublicOrigin != "" {
		cfg.OIDCClientID = strings.TrimSpace(getenv("IDENTITY_OIDC_CLIENT_ID"))
		if cfg.OIDCClientID == "" {
			return Config{}, errors.New("login: native OIDC requires IDENTITY_OIDC_CLIENT_ID")
		}
		if cfg.OIDCClientID != strings.TrimSpace(getenv("IDENTITY_OIDC_AUDIENCE")) {
			return Config{}, errors.New("login: IDENTITY_OIDC_CLIENT_ID must equal IDENTITY_OIDC_AUDIENCE")
		}
		cfg.OIDCClientSecret = getenv("IDENTITY_OIDC_CLIENT_SECRET")
	}
	return cfg, nil
}

// SessionConfigFromEnv reads only settings needed to accept browser sessions.
// It never reads any OIDC client setting, including the optional client secret.
func SessionConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{Mode: Mode(strings.TrimSpace(getenv("IDENTITY_MODE")))}
	switch cfg.Mode {
	case "", ModeDisabled:
		return Config{Mode: ModeDisabled}, nil
	case ModePassword, ModeOIDC, ModeTrustedProxy:
	default:
		return Config{}, errors.New("login: IDENTITY_MODE must be disabled, password, oidc or trusted_proxy")
	}
	switch getenv("DEVELOPMENT_MODE") {
	case "", "false":
	case "true":
		cfg.Development = true
	default:
		return Config{}, errors.New("login: DEVELOPMENT_MODE must be true or false")
	}
	cfg.PublicOrigin = getenv("CONSOLE_PUBLIC_ORIGIN")
	if cfg.PublicOrigin != "" {
		if _, err := parseOrigin(cfg.PublicOrigin, cfg.Development); err != nil {
			return Config{}, err
		}
	}
	if cfg.Mode == ModePassword && (!cfg.Development || cfg.PublicOrigin == "") {
		return Config{}, errors.New("login: password mode requires DEVELOPMENT_MODE=true and CONSOLE_PUBLIC_ORIGIN")
	}
	return cfg, nil
}

// parseOrigin does not resolve a hostname or trust forwarded request headers.
// Errors are constant: url.Parse's error could quote credentials from the input.
func parseOrigin(value string, development bool) (*url.URL, error) {
	invalid := errors.New("login: CONSOLE_PUBLIC_ORIGIN must be an exact HTTPS origin (loopback HTTP requires DEVELOPMENT_MODE=true)")
	u, err := url.Parse(value)
	if err != nil || strings.TrimSpace(value) != value || strings.ContainsAny(value, "?#") ||
		u.User != nil || u.Hostname() == "" || u.Path != "" || u.Opaque != "" || strings.ContainsAny(u.Host, "\\% \t\r\n") {
		return nil, invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, invalid
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	if u.Scheme != "https" && !(development && loopbackHTTP(u)) {
		return nil, invalid
	}
	return u, nil
}

func loopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(u.Hostname())
	return err == nil && addr.Unmap().IsLoopback()
}
