package admin

import (
	"context"
	"encoding/json"
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
	var req struct {
		RPM  *int64 `json:"requests_per_minute"`
		ITPM *int64 `json:"input_tokens_per_minute"`
		OTPM *int64 `json:"output_tokens_per_minute"`
	}
	if err := strict(b, &req); err != nil {
		return nil, err
	}
	var named patch
	if err := json.Unmarshal(b, &named); err != nil {
		return nil, invalid("invalid JSON: %s", err)
	}
	for _, field := range []string{"requests_per_minute", "input_tokens_per_minute", "output_tokens_per_minute"} {
		if _, ok := named[field]; !ok {
			return nil, invalid("%s is required; use null for no limit", field)
		}
	}
	if err := checkAliasName(r.PathValue("model")); err != nil {
		return nil, err
	}
	limit, err := h.cfg.Store.PutModelLimit(r.Context(), store.ModelLimit{
		Model: r.PathValue("model"), RPM: req.RPM, ITPM: req.ITPM, OTPM: req.OTPM,
	}, func(tx pgx.Tx) error {
		// Same principal-first mutation fence as key grants. An admin who loses
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
