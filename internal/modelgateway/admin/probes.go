package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// Prober makes the vendor calls of the check and test routes, from the
// gateway's own request code (internal/modelgateway), which this package
// cannot import. A check sends the headers and the key an attempt sends; a
// test is an attempt, its profile's edits and conversion included, so what
// it reports is what an agent's call would meet. Neither route stores what
// it learns; the answer is the record.
type Prober interface {
	// CheckCredential lists the vendor's models with c's key, once on each
	// protocol c may be used on, without a retry.
	CheckCredential(ctx context.Context, p store.Provider, c store.Credential) []CredentialCheck
	// TestDeployment sends d one minimal request with each of cs, once and
	// without a retry or a fallback, on the protocol a Messages request (an
	// embeddings or rerank one, for those kinds) would reach p on, and writes
	// each to the ledger as a test.
	TestDeployment(ctx context.Context, d store.Deployment, p store.Provider, cs []store.Credential) []DeploymentTest
}

// CredentialCheck is one protocol's result of a credential check.
type CredentialCheck struct {
	Protocol  profile.Protocol `json:"protocol"`
	OK        bool             `json:"ok"`
	Status    *int             `json:"status"`     // the vendor's HTTP status; null when it answered none
	Error     *ProbeError      `json:"error"`      // null when ok
	LatencyMS *int64           `json:"latency_ms"` // null when no request was sent
	Models    []string         `json:"models"`     // the ids the vendor listed; null unless ok
}

// DeploymentTest is one credential's result of a deployment test.
type DeploymentTest struct {
	CredentialID string            `json:"credential_id"`
	Protocol     *profile.Protocol `json:"protocol"` // null when the credential reaches the deployment on none
	OK           bool              `json:"ok"`
	Status       *int              `json:"status"` // the vendor's HTTP status; null when it answered none
	Error        *ProbeError       `json:"error"`  // null when ok
	LatencyMS    *int64            `json:"latency_ms"`
	Usage        *TestUsage        `json:"usage"` // null when the vendor reported none
}

// ProbeError is why a check or a test failed: the vendor's error type and
// message, its credentials removed, or the gateway's own.
type ProbeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// TestUsage is the tokens a test's answer reported, in the ledger's terms.
type TestUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
}

type credentialCheckView struct {
	Type         string            `json:"type"`
	ProviderID   string            `json:"provider_id"`
	CredentialID string            `json:"credential_id"`
	Results      []CredentialCheck `json:"results"`
}

type deploymentTestView struct {
	Type         string           `json:"type"`
	DeploymentID string           `json:"deployment_id"`
	Results      []DeploymentTest `json:"results"`
}

// optional reads a body a request may leave out into dst; an empty body
// leaves dst as it is.
func optional(r *http.Request, dst any) error {
	b, err := readBody(r)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return err
	}
	return strict(b, dst)
}

// checkCredential checks a credential, disabled or not, on its provider's
// endpoints. Its body, if any, is an empty object.
func (h *handler) checkCredential(r *http.Request) (any, error) {
	if err := optional(r, &struct{}{}); err != nil {
		return nil, err
	}
	p, err := h.cfg.Store.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	c, err := h.cfg.Store.GetCredential(r.Context(), p.ID, r.PathValue("cid"))
	if err != nil {
		return nil, err
	}
	return credentialCheckView{Type: "credential_check", ProviderID: p.ID, CredentialID: c.ID,
		Results: h.cfg.Prober.CheckCredential(r.Context(), p, c)}, nil
}

// testDeployment tests a deployment with the credential its body names, or
// with each enabled credential of its provider. A disabled deployment,
// provider or named credential is tested all the same: a test is how an
// operator learns whether to enable one.
func (h *handler) testDeployment(r *http.Request) (any, error) {
	var req struct {
		CredentialID *string `json:"credential_id"`
	}
	if err := optional(r, &req); err != nil {
		return nil, err
	}
	d, err := h.cfg.Store.GetDeployment(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	p, err := h.cfg.Store.GetProvider(r.Context(), d.ProviderID)
	if err != nil {
		return nil, err
	}
	var cs []store.Credential
	if req.CredentialID != nil {
		c, err := h.cfg.Store.GetCredential(r.Context(), p.ID, *req.CredentialID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, invalid("credential_id: provider %q has no credential %q", p.ID, *req.CredentialID)
		}
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
	} else {
		all, err := h.cfg.Store.ListCredentials(r.Context(), p.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			if c.Enabled {
				cs = append(cs, c)
			}
		}
		if len(cs) == 0 {
			return nil, invalid("provider %q has no enabled credential to test with; name one in credential_id", p.ID)
		}
	}
	return deploymentTestView{Type: "deployment_test", DeploymentID: d.ID,
		Results: h.cfg.Prober.TestDeployment(r.Context(), d, p, cs)}, nil
}
