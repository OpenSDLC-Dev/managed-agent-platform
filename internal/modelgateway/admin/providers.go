package admin

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

type profileView struct {
	Type string `json:"type"`
	profile.Profile
}

func viewProfile(p profile.Profile) profileView {
	if p.Hosts == nil {
		p.Hosts = []profile.Host{}
	}
	return profileView{Type: "profile", Profile: p}
}

func (h *handler) listProfiles(*http.Request) (any, error) {
	var out []profileView
	for _, p := range profile.All() {
		out = append(out, viewProfile(p))
	}
	return listView{Data: out}, nil
}

func (h *handler) getProfile(r *http.Request) (any, error) {
	p, ok := profile.Lookup(r.PathValue("name"))
	if !ok {
		return nil, notFound("profile %q does not exist", r.PathValue("name"))
	}
	return viewProfile(p), nil
}

type providerView struct {
	Type           string                      `json:"type"`
	ID             string                      `json:"id"`
	Name           string                      `json:"name"`
	Profile        string                      `json:"profile"`
	Endpoints      map[profile.Protocol]string `json:"endpoints"`
	Headers        map[string]string           `json:"headers"`
	StallTimeoutMS *int64                      `json:"stall_timeout_ms"`
	Enabled        bool                        `json:"enabled"`
	CreatedAt      time.Time                   `json:"created_at"`
	UpdatedAt      time.Time                   `json:"updated_at"`
}

func viewProvider(p store.Provider) providerView {
	v := providerView{Type: "provider", ID: p.ID, Name: p.Name, Profile: p.Profile, Endpoints: p.Endpoints,
		Headers: p.Headers, Enabled: p.Enabled, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	if v.Headers == nil {
		v.Headers = map[string]string{}
	}
	if p.StallTimeout > 0 {
		ms := p.StallTimeout.Milliseconds()
		v.StallTimeoutMS = &ms
	}
	return v
}

// stallTimeout turns stall_timeout_ms into a duration; nil is the default.
func stallTimeout(ms *int64) (time.Duration, error) {
	if ms == nil {
		return 0, nil
	}
	if *ms < 1 || *ms > math.MaxInt32 {
		return 0, invalid("stall_timeout_ms must be between 1 and %d", math.MaxInt32)
	}
	return time.Duration(*ms) * time.Millisecond, nil
}

func (h *handler) createProvider(r *http.Request) (any, error) {
	var req struct {
		Name           string                      `json:"name"`
		Profile        string                      `json:"profile"`
		Endpoints      map[profile.Protocol]string `json:"endpoints"`
		Headers        map[string]string           `json:"headers"`
		StallTimeoutMS *int64                      `json:"stall_timeout_ms"`
		Enabled        *bool                       `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := checkName("name", req.Name); err != nil {
		return nil, err
	}
	prof, ok := profile.Lookup(req.Profile)
	if !ok {
		return nil, invalid("profile %q does not exist; GET /admin/v1/profiles lists them", req.Profile)
	}
	if len(req.Endpoints) == 0 {
		return nil, invalid("endpoints needs at least one endpoint, keyed by protocol")
	}
	endpoints := make(map[profile.Protocol]string, len(req.Endpoints))
	for proto, raw := range req.Endpoints {
		if proto != profile.Anthropic && proto != profile.OpenAI {
			return nil, invalid("endpoints: unknown protocol %q; the protocols are anthropic and openai", proto)
		}
		if !prof.Supports(proto) {
			return nil, invalid("profile %q does not speak %s", prof.Name, proto)
		}
		u, err := checkEndpoint(string(proto), raw)
		if err != nil {
			return nil, err
		}
		endpoints[proto] = u
	}
	if err := checkHeaders(req.Headers); err != nil {
		return nil, err
	}
	stall, err := stallTimeout(req.StallTimeoutMS)
	if err != nil {
		return nil, err
	}
	p, err := h.cfg.Store.CreateProvider(r.Context(), store.Provider{
		Name: req.Name, Profile: prof.Name, Endpoints: endpoints, Headers: req.Headers,
		StallTimeout: stall, Enabled: req.Enabled == nil || *req.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return viewProvider(p), nil
}

func (h *handler) listProviders(r *http.Request) (any, error) {
	ps, err := h.cfg.Store.ListProviders(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]providerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, viewProvider(p))
	}
	return listView{Data: out}, nil
}

func (h *handler) getProvider(r *http.Request) (any, error) {
	p, err := h.cfg.Store.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return viewProvider(p), nil
}

func (h *handler) updateProvider(r *http.Request) (any, error) {
	req, err := decodePatch(r, atCreation("profile", "endpoints"), []string{"name", "headers", "stall_timeout_ms", "enabled"})
	if err != nil {
		return nil, err
	}
	var (
		name    string
		headers map[string]string
		stallMS *int64
		enabled bool
	)
	hasName, err := req.field("name", &name)
	if err != nil {
		return nil, err
	}
	if hasName {
		if err := checkName("name", name); err != nil {
			return nil, err
		}
	}
	hasHeaders, err := req.field("headers", &headers)
	if err != nil {
		return nil, err
	}
	if err := checkHeaders(headers); err != nil {
		return nil, err
	}
	hasStall, err := req.field("stall_timeout_ms", &stallMS)
	if err != nil {
		return nil, err
	}
	stall, err := stallTimeout(stallMS)
	if err != nil {
		return nil, err
	}
	hasEnabled, err := req.field("enabled", &enabled)
	if err != nil {
		return nil, err
	}
	p, err := h.cfg.Store.UpdateProvider(r.Context(), r.PathValue("id"), func(p *store.Provider) error {
		if hasName {
			p.Name = name
		}
		if hasHeaders {
			p.Headers = headers
		}
		if hasStall {
			p.StallTimeout = stall
		}
		if hasEnabled {
			p.Enabled = enabled
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return viewProvider(p), nil
}

func (h *handler) deleteProvider(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if err := h.cfg.Store.DeleteProvider(r.Context(), id); err != nil {
		return nil, err
	}
	return map[string]string{"type": "provider_deleted", "id": id}, nil
}

type credentialView struct {
	Type       string             `json:"type"`
	ID         string             `json:"id"`
	ProviderID string             `json:"provider_id"`
	Kind       string             `json:"kind"`
	LastFour   string             `json:"last_four"`
	Protocols  []profile.Protocol `json:"protocols"`
	Weight     int                `json:"weight"`
	Enabled    bool               `json:"enabled"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// viewCredential shows everything but the key.
func viewCredential(c store.Credential) credentialView {
	return credentialView{Type: "credential", ID: c.ID, ProviderID: c.ProviderID, Kind: c.Kind, LastFour: c.LastFour,
		Protocols: c.Protocols, Weight: c.Weight, Enabled: c.Enabled, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// maxKeyLen bounds a vendor key, which travels in one header.
const maxKeyLen = 4096

// lastFour is the tail the console shows of a key — none of a key short
// enough that four characters would be a large share of it.
func lastFour(key string) string {
	r := []rune(key)
	if len(r) < 12 {
		return ""
	}
	return string(r[len(r)-4:])
}

func checkProtocols(ps []profile.Protocol) error {
	for i, p := range ps {
		if p != profile.Anthropic && p != profile.OpenAI {
			return invalid("protocols: unknown protocol %q; the protocols are anthropic and openai", p)
		}
		if slices.Contains(ps[:i], p) {
			return invalid("protocols names %s twice", p)
		}
	}
	return nil
}

func (h *handler) createCredential(r *http.Request) (any, error) {
	var req struct {
		Key       string             `json:"key"`
		Protocols []profile.Protocol `json:"protocols"`
		Weight    *int               `json:"weight"`
		Enabled   *bool              `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := checkToken("key", req.Key, maxKeyLen); err != nil {
		return nil, err
	}
	if err := checkProtocols(req.Protocols); err != nil {
		return nil, err
	}
	weight := 1
	if req.Weight != nil {
		weight = *req.Weight
	}
	if err := checkWeight("weight", weight); err != nil {
		return nil, err
	}
	providerID := r.PathValue("id")
	if _, err := h.cfg.Store.GetProvider(r.Context(), providerID); err != nil {
		return nil, err
	}
	ciphertext, keyID, err := h.cfg.Cipher.Encrypt(r.Context(), []byte(req.Key))
	if errors.Is(err, secrets.ErrPlaintextTooLarge) {
		return nil, invalid("key cannot be sealed: %s", err)
	}
	if err != nil {
		return nil, fmt.Errorf("seal credential key: %w", err)
	}
	c, err := h.cfg.Store.CreateCredential(r.Context(), store.Credential{
		ProviderID: providerID, Ciphertext: ciphertext, KeyID: keyID, LastFour: lastFour(req.Key),
		Protocols: req.Protocols, Weight: weight, Enabled: req.Enabled == nil || *req.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return viewCredential(c), nil
}

func (h *handler) listCredentials(r *http.Request) (any, error) {
	providerID := r.PathValue("id")
	if _, err := h.cfg.Store.GetProvider(r.Context(), providerID); err != nil {
		return nil, err
	}
	cs, err := h.cfg.Store.ListCredentials(r.Context(), providerID)
	if err != nil {
		return nil, err
	}
	out := make([]credentialView, 0, len(cs))
	for _, c := range cs {
		out = append(out, viewCredential(c))
	}
	return listView{Data: out}, nil
}

func (h *handler) getCredential(r *http.Request) (any, error) {
	c, err := h.cfg.Store.GetCredential(r.Context(), r.PathValue("id"), r.PathValue("cid"))
	if err != nil {
		return nil, err
	}
	return viewCredential(c), nil
}

func (h *handler) updateCredential(r *http.Request) (any, error) {
	req, err := decodePatch(r,
		fixedFields{"key": "a credential's key is fixed; to rotate it, add a new credential and delete this one"},
		[]string{"protocols", "weight", "enabled"})
	if err != nil {
		return nil, err
	}
	var (
		protos  []profile.Protocol
		weight  int
		enabled bool
	)
	hasProtos, err := req.field("protocols", &protos)
	if err != nil {
		return nil, err
	}
	if err := checkProtocols(protos); err != nil {
		return nil, err
	}
	hasWeight, err := req.field("weight", &weight)
	if err != nil {
		return nil, err
	}
	if hasWeight {
		if err := checkWeight("weight", weight); err != nil {
			return nil, err
		}
	}
	hasEnabled, err := req.field("enabled", &enabled)
	if err != nil {
		return nil, err
	}
	c, err := h.cfg.Store.UpdateCredential(r.Context(), r.PathValue("id"), r.PathValue("cid"), func(c *store.Credential) error {
		if hasProtos {
			c.Protocols = protos
		}
		if hasWeight {
			c.Weight = weight
		}
		if hasEnabled {
			c.Enabled = enabled
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return viewCredential(c), nil
}

func (h *handler) deleteCredential(r *http.Request) (any, error) {
	id := r.PathValue("cid")
	if err := h.cfg.Store.DeleteCredential(r.Context(), r.PathValue("id"), id); err != nil {
		return nil, err
	}
	return map[string]string{"type": "credential_deleted", "id": id}, nil
}
