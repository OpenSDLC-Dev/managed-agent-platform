package admin

import (
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

type pricesJSON struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheWrite *float64 `json:"cache_write"`
	CacheRead  *float64 `json:"cache_read"`
}

type deploymentView struct {
	Type          string             `json:"type"`
	ID            string             `json:"id"`
	ProviderID    string             `json:"provider_id"`
	UpstreamModel string             `json:"upstream_model"`
	Kind          store.Kind         `json:"kind"`
	DisplayName   string             `json:"display_name"`
	Capabilities  store.Capabilities `json:"capabilities"`
	Prices        pricesJSON         `json:"prices"`
	Enabled       bool               `json:"enabled"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

func viewDeployment(d store.Deployment) deploymentView {
	return deploymentView{Type: "deployment", ID: d.ID, ProviderID: d.ProviderID, UpstreamModel: d.UpstreamModel,
		Kind: d.Kind, DisplayName: d.DisplayName, Capabilities: d.Capabilities,
		Prices:  pricesJSON{Input: d.Prices.Input, Output: d.Prices.Output, CacheWrite: d.Prices.CacheWrite, CacheRead: d.Prices.CacheRead},
		Enabled: d.Enabled, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}

func checkKind(k store.Kind) error {
	switch k {
	case store.KindChat, store.KindEmbedding, store.KindRerank:
		return nil
	case "":
		return invalid("kind is required: chat, embedding or rerank")
	}
	return invalid("kind %q is not chat, embedding or rerank", k)
}

func checkCapabilities(c store.Capabilities) error {
	if c.MaxInputTokens < 0 {
		return invalid("capabilities.max_input_tokens must not be negative")
	}
	if c.MaxTokens < 0 {
		return invalid("capabilities.max_tokens must not be negative")
	}
	return nil
}

// The bounds on a nonzero price per million tokens, each orders of
// magnitude past any real price in any currency (a $600 price in Iranian
// rials is near 2e9). Within them every cost the ledger computes, and every
// day's sum of them, reads back as a double precision number, since the
// gateway bounds a row's token counts too.
const (
	minPrice = 1e-12
	maxPrice = 1e15
)

// checkPrices returns prices in the store's shape, each 0 or within the
// bounds; JSON carries no NaN or infinity.
func checkPrices(p pricesJSON) (store.Prices, error) {
	for name, v := range map[string]*float64{"input": p.Input, "output": p.Output, "cache_write": p.CacheWrite, "cache_read": p.CacheRead} {
		switch {
		case v == nil || *v == 0:
		case *v < 0:
			return store.Prices{}, invalid("prices.%s must not be negative", name)
		case *v < minPrice || *v > maxPrice:
			return store.Prices{}, invalid("prices.%s must be 0 or between %g and %g", name, minPrice, maxPrice)
		}
	}
	return store.Prices{Input: p.Input, Output: p.Output, CacheWrite: p.CacheWrite, CacheRead: p.CacheRead}, nil
}

func (h *handler) createDeployment(r *http.Request) (any, error) {
	var req struct {
		ProviderID    string             `json:"provider_id"`
		UpstreamModel string             `json:"upstream_model"`
		Kind          store.Kind         `json:"kind"`
		DisplayName   string             `json:"display_name"`
		Capabilities  store.Capabilities `json:"capabilities"`
		Prices        pricesJSON         `json:"prices"`
		Enabled       *bool              `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := checkName("provider_id", req.ProviderID); err != nil {
		return nil, err
	}
	if err := checkName("upstream_model", req.UpstreamModel); err != nil {
		return nil, err
	}
	if err := checkKind(req.Kind); err != nil {
		return nil, err
	}
	if err := checkOptionalName("display_name", req.DisplayName); err != nil {
		return nil, err
	}
	if err := checkCapabilities(req.Capabilities); err != nil {
		return nil, err
	}
	prices, err := checkPrices(req.Prices)
	if err != nil {
		return nil, err
	}
	d, err := h.cfg.Store.CreateDeployment(r.Context(), store.Deployment{
		ProviderID: req.ProviderID, UpstreamModel: req.UpstreamModel, Kind: req.Kind, DisplayName: req.DisplayName,
		Capabilities: req.Capabilities, Prices: prices, Enabled: req.Enabled == nil || *req.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return viewDeployment(d), nil
}

func (h *handler) listDeployments(r *http.Request) (any, error) {
	ds, err := h.cfg.Store.ListDeployments(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]deploymentView, 0, len(ds))
	for _, d := range ds {
		out = append(out, viewDeployment(d))
	}
	return listView{Data: out}, nil
}

func (h *handler) getDeployment(r *http.Request) (any, error) {
	d, err := h.cfg.Store.GetDeployment(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return viewDeployment(d), nil
}

// updateDeployment replaces capabilities and prices whole when named.
func (h *handler) updateDeployment(r *http.Request) (any, error) {
	req, err := decodePatch(r, atCreation("provider_id", "upstream_model", "kind"),
		[]string{"display_name", "capabilities", "prices", "enabled"})
	if err != nil {
		return nil, err
	}
	var (
		name    string
		caps    store.Capabilities
		pj      pricesJSON
		enabled bool
	)
	hasName, err := req.field("display_name", &name)
	if err != nil {
		return nil, err
	}
	if err := checkOptionalName("display_name", name); err != nil {
		return nil, err
	}
	hasCaps, err := req.field("capabilities", &caps)
	if err != nil {
		return nil, err
	}
	if err := checkCapabilities(caps); err != nil {
		return nil, err
	}
	hasPrices, err := req.field("prices", &pj)
	if err != nil {
		return nil, err
	}
	prices, err := checkPrices(pj)
	if err != nil {
		return nil, err
	}
	hasEnabled, err := req.field("enabled", &enabled)
	if err != nil {
		return nil, err
	}
	d, err := h.cfg.Store.UpdateDeployment(r.Context(), r.PathValue("id"), func(d *store.Deployment) error {
		if hasName {
			d.DisplayName = name
		}
		if hasCaps {
			d.Capabilities = caps
		}
		if hasPrices {
			d.Prices = prices
		}
		if hasEnabled {
			d.Enabled = enabled
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return viewDeployment(d), nil
}

func (h *handler) deleteDeployment(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if err := h.cfg.Store.DeleteDeployment(r.Context(), id); err != nil {
		return nil, err
	}
	return map[string]string{"type": "deployment_deleted", "id": id}, nil
}
