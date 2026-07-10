package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Wire error types (shared.ErrorType in the reference SDK). The reference has
// no dedicated conflict type; optimistic-version mismatches surface as
// invalid_request_error with HTTP 409.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeAuthentication = "authentication_error"
	errTypeNotFound       = "not_found_error"
	errTypeAPI            = "api_error"
)

// apiError is an error that maps onto the Anthropic wire error envelope.
type apiError struct {
	status  int
	errType string
	message string
}

func (e *apiError) Error() string { return e.message }

func errInvalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, errTypeInvalidRequest, fmt.Sprintf(format, args...)}
}

func errNotFound(format string, args ...any) *apiError {
	return &apiError{http.StatusNotFound, errTypeNotFound, fmt.Sprintf(format, args...)}
}

func errConflict(format string, args ...any) *apiError {
	return &apiError{http.StatusConflict, errTypeInvalidRequest, fmt.Sprintf(format, args...)}
}

func errAuth(message string) *apiError {
	return &apiError{http.StatusUnauthorized, errTypeAuthentication, message}
}

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// writeError renders any error as the wire envelope
// {"type":"error","request_id":…,"error":{"type":…,"message":…}}.
// Non-apiError values are internal faults: logged, reported as api_error
// without leaking internals.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae, ok := err.(*apiError)
	if !ok {
		slog.Error("internal error", "method", r.Method, "path", r.URL.Path,
			"request_id", requestIDFrom(r.Context()), "err", err)
		ae = &apiError{http.StatusInternalServerError, errTypeAPI, "internal server error"}
	}
	writeJSON(w, ae.status, map[string]any{
		"type":       "error",
		"request_id": requestIDFrom(r.Context()),
		"error":      map[string]string{"type": ae.errType, "message": ae.message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
