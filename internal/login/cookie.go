package login

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

const sessionCookieName = "map_session"

func (m *Manager) SetCookie(w http.ResponseWriter, issued IssuedSession) {
	cookie := m.cookie(issued.Token)
	cookie.Expires = issued.ExpiresAt
	cookie.MaxAge = int(sessionLifetime / time.Second)
	http.SetCookie(w, cookie)
}

func (m *Manager) ClearCookie(w http.ResponseWriter) {
	cookie := m.cookie("")
	cookie.Expires = time.Unix(1, 0).UTC()
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

func (m *Manager) cookie(value string) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: value, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode}
}

// sessionToken examines raw pairs instead of Request.Cookie: that parser skips
// malformed cookies, which could hide an offered duplicate from authentication.
func sessionToken(r *http.Request) (string, error) {
	var token string
	count := 0
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			part = strings.TrimLeft(part, " \t")
			name, value, found := strings.Cut(part, "=")
			if strings.TrimSpace(name) != sessionCookieName {
				continue
			}
			count++
			if !found || name != sessionCookieName {
				return "", ErrUnauthenticated
			}
			token = value
		}
	}
	if count != 1 || len(token) != 43 {
		return "", ErrUnauthenticated
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return "", ErrUnauthenticated
	}
	return token, nil
}

// CheckOrigin requires the configured origin verbatim and rejects cross-site
// Fetch Metadata. Forwarded host/protocol headers cannot grant browser access.
func (m *Manager) CheckOrigin(r *http.Request) error {
	origins := r.Header.Values("Origin")
	if m.origin == "" || len(origins) != 1 || origins[0] != m.origin {
		return ErrCSRF
	}
	sites := r.Header.Values("Sec-Fetch-Site")
	if len(sites) > 1 {
		return ErrCSRF
	}
	if len(sites) == 1 {
		switch sites[0] {
		case "same-origin", "same-site", "none":
		default:
			return ErrCSRF
		}
	}
	return nil
}

// CheckCSRF protects cookie-authenticated unsafe methods. Anonymous password
// login/setup instead use CheckOrigin plus a JSON content-type admission check.
func (m *Manager) CheckCSRF(r *http.Request, session Session) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if err := m.CheckOrigin(r); err != nil {
		return err
	}
	tokens := r.Header.Values("X-CSRF-Token")
	if len(tokens) != 1 || len(session.CSRFToken) != 43 || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(session.CSRFToken)) != 1 {
		return ErrCSRF
	}
	return nil
}
