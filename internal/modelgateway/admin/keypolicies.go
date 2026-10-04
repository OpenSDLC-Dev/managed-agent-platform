package admin

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

type keyPolicyView struct {
	Type      string    `json:"type"`
	APIKeyID  string    `json:"api_key_id"`
	Aliases   []string  `json:"aliases"`
	RPM       *int32    `json:"rpm"`
	TPM       *int64    `json:"tpm"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func viewKeyPolicy(k store.KeyPolicy) keyPolicyView {
	return keyPolicyView{Type: "key_policy", APIKeyID: k.APIKeyID, Aliases: k.Aliases, RPM: k.RPM, TPM: k.TPM,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt}
}

// putKeyPolicy writes a key's grant whole: aliases null grants every alias,
// and rpm or tpm null sets no limit. Because the write is whole, each field
// must be named: a body that left one out, as an update elsewhere may, would
// widen the grant without saying so.
func (h *handler) putKeyPolicy(r *http.Request) (any, error) {
	b, err := readBody(r)
	if err != nil {
		return nil, err
	}
	var req struct {
		Aliases []string `json:"aliases"`
		RPM     *int32   `json:"rpm"`
		TPM     *int64   `json:"tpm"`
	}
	if err := strict(b, &req); err != nil {
		return nil, err
	}
	var named patch
	if err := json.Unmarshal(b, &named); err != nil {
		return nil, invalid("Failed to parse request body as JSON: %s", err)
	}
	for _, f := range []struct{ name, null string }{
		{"aliases", "every alias"}, {"rpm", "no limit"}, {"tpm", "no limit"},
	} {
		if _, ok := named[f.name]; !ok {
			return nil, invalid("%s is required: a grant is written whole, so name each field (null for %s)", f.name, f.null)
		}
	}
	if req.Aliases != nil && len(req.Aliases) == 0 {
		return nil, invalid("an empty aliases list grants nothing; DELETE the policy to revoke the grant")
	}
	for i, a := range req.Aliases {
		if slices.Contains(req.Aliases[:i], a) {
			return nil, invalid("aliases names %q twice", a)
		}
	}
	if req.RPM != nil && *req.RPM < 1 {
		return nil, invalid("rpm must be at least 1, or null for no limit")
	}
	if req.TPM != nil && *req.TPM < 1 {
		return nil, invalid("tpm must be at least 1, or null for no limit")
	}
	k, err := h.cfg.Store.PutKeyPolicy(r.Context(), store.KeyPolicy{
		APIKeyID: r.PathValue("api_key_id"), Aliases: req.Aliases, RPM: req.RPM, TPM: req.TPM,
	})
	if err != nil {
		return nil, err
	}
	return viewKeyPolicy(k), nil
}

func (h *handler) listKeyPolicies(r *http.Request) (any, error) {
	ks, err := h.cfg.Store.ListKeyPolicies(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]keyPolicyView, 0, len(ks))
	for _, k := range ks {
		out = append(out, viewKeyPolicy(k))
	}
	return listView{Data: out}, nil
}

func (h *handler) getKeyPolicy(r *http.Request) (any, error) {
	k, err := h.cfg.Store.GetKeyPolicy(r.Context(), r.PathValue("api_key_id"))
	if err != nil {
		return nil, err
	}
	return viewKeyPolicy(k), nil
}

func (h *handler) deleteKeyPolicy(r *http.Request) (any, error) {
	id := r.PathValue("api_key_id")
	if err := h.cfg.Store.DeleteKeyPolicy(r.Context(), id); err != nil {
		return nil, err
	}
	return map[string]string{"type": "key_policy_deleted", "api_key_id": id}, nil
}
