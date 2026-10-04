package admin

import (
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

// putKeyPolicy writes a key's grant whole: aliases absent or null grants
// every alias, and rpm or tpm absent or null sets no limit.
func (h *handler) putKeyPolicy(r *http.Request) (any, error) {
	var req struct {
		Aliases []string `json:"aliases"`
		RPM     *int32   `json:"rpm"`
		TPM     *int64   `json:"tpm"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
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
