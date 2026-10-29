package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

const (
	consoleProxyPath     = "/api/organizations/{org}/proxy/v1/{resource...}"
	consoleProxyRootPath = "/api/organizations/{org}/proxy/v1"
)

// serveConsoleProxy reuses the authenticated dispatcher, not the bare mux. The
// cookie's context may enter its management arm only; it is never a worker key.
func (s *server) serveConsoleProxy(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := requireCookieSession(r); err != nil {
			writeError(w, r, err)
			return
		}
		parts := strings.Split(r.URL.EscapedPath(), "/")
		if !privateCookiePath(r) || len(parts) < 6 {
			writeError(w, r, errNotFound("Not found"))
			return
		}
		for i, want := range [...]string{"", "api", "organizations", r.PathValue("org"), "proxy", "v1"} {
			if value, _ := url.PathUnescape(parts[i]); value != want {
				writeError(w, r, errNotFound("Not found"))
				return
			}
		}
		if err := consoleOrganization(r); err != nil {
			writeError(w, r, err)
			return
		}
		target := "/" + strings.Join(parts[5:], "/")
		if !consoleResourcePath(target) {
			writeError(w, r, errNotFound("Not found"))
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		forwarded := r.Clone(ctx)
		forwarded.URL.Path, _ = url.PathUnescape(target)
		forwarded.URL.RawPath = target
		forwarded.RequestURI = forwarded.URL.RequestURI()
		forwarded.Header.Del("x-api-key")
		forwarded.Header.Del("Authorization")
		if s.verifier != nil && s.verifier.AssertionHeader() != "" {
			forwarded.Header.Del(s.verifier.AssertionHeader())
		}

		// A tick plus a bounded lookup takes at most 10s of the 15s withdrawal
		// budget. Cancellation also aborts an in-flight lookup on request end.
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					session, err := s.login.Authenticate(checkCtx, forwarded)
					stop()
					if ctx.Err() != nil {
						return // request completion, not authority withdrawal
					}
					if err != nil || !session.Role.AtLeast(identity.RoleDeveloper) {
						cancel()
						// Context cancellation alone cannot interrupt a slow client's
						// blocked Write/Flush. Expire this response's write deadline too.
						_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
						return
					}
				}
			}
		}()
		defer func() { cancel(); <-done }()
		next.ServeHTTP(&consoleProxyWriter{ResponseWriter: w, prefix: "/" + strings.Join(parts[1:5], "/"), host: r.Host}, forwarded)
	}
}

// consoleResourcePath bounds both dispatch and redirect rewriting to management
// families. Decode segments individually, as ServeMux does: encoded literals
// work, while encoded separators and cleaning redirects cannot change lanes.
func consoleResourcePath(escaped string) bool {
	parts := strings.Split(escaped, "/")
	if len(parts) < 3 || parts[0] != "" {
		return false
	}
	for i := 1; i < len(parts); i++ {
		value, err := url.PathUnescape(parts[i])
		if err != nil || value == "" || value == "." || value == ".." || strings.Contains(value, "/") {
			return false
		}
		parts[i] = value
	}
	if parts[1] != "v1" || isWorkPath(strings.Join(parts, "/")) {
		return false
	}
	switch parts[2] {
	case "agents", "environments", "sessions", "deployments", "deployment_runs", "vaults", "skills", "files", "memory_stores", "dreams":
		return true
	}
	return false
}

// Headers and bytes otherwise pass through unchanged. Unwrap and Flush retain
// the real connection's streaming/cancellation behavior, without buffering SSE.
type consoleProxyWriter struct {
	http.ResponseWriter
	prefix, host string
	wrote        bool
}

func (w *consoleProxyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *consoleProxyWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if status >= 300 && status < 400 {
		u, err := url.Parse(w.Header().Get("Location"))
		if err == nil && u.User == nil && u.Opaque == "" && consoleResourcePath(u.EscapedPath()) &&
			(u.Host == "" && u.Scheme == "" || u.Host == w.host && (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https")) {
			escaped := w.prefix + u.EscapedPath()
			u.Path, _ = url.PathUnescape(escaped)
			u.RawPath = escaped
			w.Header().Set("Location", u.String())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *consoleProxyWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *consoleProxyWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
