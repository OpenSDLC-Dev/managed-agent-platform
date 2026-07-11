// Package api implements the wire-compatible control-plane REST surface:
// Anthropic Managed Agents resource CRUD (agents / environments / sessions)
// with the reference paths, JSON shapes, ID prefixes, pagination envelope,
// error envelope, and x-api-key management auth. The `?beta=true` query and
// anthropic-version / anthropic-beta headers are accepted and ignored.
package api

import (
	"context"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type server struct {
	pool   *pgxpool.Pool
	log    *events.Log
	broker *events.Broker
	queue  *queue.Queue
}

// NewHandler assembles the control-plane HTTP surface over the given pool.
func NewHandler(pool *pgxpool.Pool) http.Handler {
	s := &server{pool: pool, log: events.NewLog(pool), broker: events.NewBroker(pool), queue: queue.New(pool)}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/agents", s.handle(s.createAgent))
	mux.HandleFunc("GET /v1/agents", s.handle(s.listAgents))
	mux.HandleFunc("GET /v1/agents/{id}", s.handle(s.getAgent))
	mux.HandleFunc("POST /v1/agents/{id}", s.handle(s.updateAgent)) // update is POST on the wire, not PATCH
	mux.HandleFunc("GET /v1/agents/{id}/versions", s.handle(s.listAgentVersions))
	mux.HandleFunc("POST /v1/agents/{id}/archive", s.handle(s.archiveAgent))

	mux.HandleFunc("POST /v1/environments", s.handle(s.createEnvironment))
	mux.HandleFunc("GET /v1/environments", s.handle(s.listEnvironments))
	mux.HandleFunc("GET /v1/environments/{id}", s.handle(s.getEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}", s.handle(s.updateEnvironment))
	mux.HandleFunc("DELETE /v1/environments/{id}", s.handle(s.deleteEnvironment))
	mux.HandleFunc("POST /v1/environments/{id}/archive", s.handle(s.archiveEnvironment))

	mux.HandleFunc("POST /v1/sessions", s.handle(s.createSession))
	mux.HandleFunc("GET /v1/sessions", s.handle(s.listSessions))
	mux.HandleFunc("GET /v1/sessions/{id}", s.handle(s.getSession))
	mux.HandleFunc("POST /v1/sessions/{id}", s.handle(s.updateSession))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handle(s.deleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.handle(s.archiveSession))

	mux.HandleFunc("POST /v1/sessions/{id}/events", s.handle(s.sendSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handle(s.listSessionEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamSessionEvents)

	// The mux's built-in 404/405 write plain text; clients expect the wire
	// error envelope, so register explicit fallbacks: "/" for unknown paths
	// and a method-less pattern per route for unsupported methods.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound("no such endpoint: %s", r.URL.Path))
	})
	for _, pattern := range []string{
		"/v1/agents", "/v1/agents/{id}", "/v1/agents/{id}/versions", "/v1/agents/{id}/archive",
		"/v1/environments", "/v1/environments/{id}", "/v1/environments/{id}/archive",
		"/v1/sessions", "/v1/sessions/{id}", "/v1/sessions/{id}/archive",
		"/v1/sessions/{id}/events", "/v1/sessions/{id}/events/stream",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, methodNotAllowed(r))
		})
	}

	// The work API is a separate auth domain: BYOC workers authenticate with an
	// Authorization: Bearer environment key, not the management x-api-key. It
	// lives on its own mux so the two auth middlewares never overlap, and the
	// top router hands the /work subtree to it and everything else to the
	// management mux.
	work := http.NewServeMux()
	work.HandleFunc("GET /v1/environments/{id}/work/poll", s.handle(s.pollWork))
	work.HandleFunc("/v1/environments/{id}/work/poll", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, methodNotAllowed(r))
	})
	work.HandleFunc("/v1/environments/{id}/work/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound("no such endpoint: %s", r.URL.Path))
	})

	top := http.NewServeMux()
	top.Handle("/v1/environments/{id}/work/", requireEnvironmentKey(pool, work))
	top.Handle("/", requireAPIKey(pool, mux))

	return withRequestID(withTracing(top))
}

// methodNotAllowed is the wire 405 for a known path reached with an
// unsupported method.
func methodNotAllowed(r *http.Request) *apiError {
	return &apiError{http.StatusMethodNotAllowed, errTypeInvalidRequest,
		"method " + r.Method + " is not allowed on " + r.URL.Path}
}

// handle adapts a typed handler to http.HandlerFunc: JSON out, error envelope
// on failure. The reference returns 200 for every successful call, including
// creates.
func (s *server) handle(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// withRequestID stamps every response (success and error) with a request-id
// header and threads the ID into the context for error envelopes.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := domain.NewID("req").String()
		w.Header().Set("request-id", rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, rid)))
	})
}

// withTracing continues the caller's W3C trace context and opens one server
// span per request (CLAUDE.md: every cross-process call propagates OTel
// context). With no tracer provider installed this is a no-op passthrough.
func withTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		carrier := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				carrier[k] = v[0]
			}
		}
		ctx := telemetry.Extract(r.Context(), carrier)
		ctx, span := otel.GetTracerProvider().
			Tracer("github.com/OpenSDLC-Dev/managed-agent-platform/internal/api").
			Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
