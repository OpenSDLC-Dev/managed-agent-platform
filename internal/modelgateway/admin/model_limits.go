package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/jackc/pgx/v5"
)

// This is a private self-hosting API, not the reference's read-only public
// organization rate-limit API. docs/DIVERGENCES.md records its shape.
type modelLimitView struct {
	Model string `json:"model"`
	RPM   *int64 `json:"requests_per_minute"`
	ITPM  *int64 `json:"input_tokens_per_minute"`
	OTPM  *int64 `json:"output_tokens_per_minute"`
}

func (h *handler) listModelLimits(r *http.Request) (any, error) {
	if cookie, _ := r.Context().Value(cookieKey{}).(bool); !cookie {
		return nil, forbidden("organization rate limits require a console cookie")
	}
	limits, err := h.cfg.Store.ListModelLimits(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]modelLimitView, 0, len(limits))
	for _, limit := range limits {
		out = append(out, modelLimitView{limit.Model, limit.RPM, limit.ITPM, limit.OTPM})
	}
	return listView{Data: out}, nil
}

func (h *handler) putModelLimit(r *http.Request) (any, error) {
	if cookie, _ := r.Context().Value(cookieKey{}).(bool); !cookie {
		return nil, forbidden("organization rate limits require a console cookie")
	}
	b, err := readBody(r)
	if err != nil {
		return nil, err
	}
	limit, err := decodeModelLimit(b)
	if err != nil {
		return nil, err
	}
	if err := checkAliasName(r.PathValue("model")); err != nil {
		return nil, err
	}
	limit.Model = r.PathValue("model")
	limit, err = h.cfg.Store.PutModelLimit(r.Context(), limit, func(tx pgx.Tx) error {
		// Principal-first mutation fence: an admin who loses
		// authority while this waits cannot widen the organization's budget.
		if _, err := tx.Exec(r.Context(), `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		session, err := h.cfg.Login.AuthenticateInTx(r.Context(), tx, r)
		if err != nil {
			return err
		}
		return requireRole(context.WithValue(r.Context(), roleKey{}, session.Role), identity.RoleAdmin)
	})
	if err != nil {
		return nil, err
	}
	return modelLimitView{limit.Model, limit.RPM, limit.ITPM, limit.OTPM}, nil
}

// Budget replacement requires each exact field once. Struct decoding accepts
// case variants and duplicate keys, which can silently turn a budget into null.
func decodeModelLimit(b []byte) (store.ModelLimit, error) {
	var limit store.ModelLimit
	fields := map[string]**int64{
		"requests_per_minute": &limit.RPM, "input_tokens_per_minute": &limit.ITPM, "output_tokens_per_minute": &limit.OTPM,
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return limit, invalid("rate limits must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return limit, invalid("invalid JSON: %s", err)
		}
		name, ok := token.(string)
		if !ok || fields[name] == nil || seen[name] {
			return limit, invalid("unknown or repeated rate-limit field %q", token)
		}
		seen[name] = true
		if err := dec.Decode(fields[name]); err != nil {
			return limit, invalid("%s: %s", name, err)
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return limit, invalid("invalid rate-limit JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return limit, invalid("invalid JSON: data after the value")
	}
	for _, name := range []string{"requests_per_minute", "input_tokens_per_minute", "output_tokens_per_minute"} {
		if !seen[name] {
			return limit, invalid("%s is required; use null for no limit", name)
		}
	}
	return limit, nil
}
