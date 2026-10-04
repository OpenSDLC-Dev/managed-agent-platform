package admin

import (
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

type targetJSON struct {
	DeploymentID string `json:"deployment_id"`
	Priority     int    `json:"priority"`
	Weight       *int   `json:"weight"`
}

type targetView struct {
	DeploymentID string `json:"deployment_id"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
}

type aliasView struct {
	Type        string       `json:"type"`
	Name        string       `json:"name"`
	DisplayName string       `json:"display_name"`
	Kind        store.Kind   `json:"kind"`
	Targets     []targetView `json:"targets"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func viewAlias(a store.Alias) aliasView {
	v := aliasView{Type: "alias", Name: a.Name, DisplayName: a.DisplayName, Kind: a.Kind,
		Targets: make([]targetView, 0, len(a.Targets)), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	for _, t := range a.Targets {
		v.Targets = append(v.Targets, targetView(t))
	}
	return v
}

// targets checks each target's shape; the store checks what they name. A
// weight left out is 1.
func targets(in []targetJSON) ([]store.Target, error) {
	if len(in) == 0 {
		return nil, invalid("targets needs at least one target")
	}
	out := make([]store.Target, 0, len(in))
	for i, t := range in {
		if err := checkName("targets.deployment_id", t.DeploymentID); err != nil {
			return nil, err
		}
		if t.Priority < 0 || t.Priority > maxPriority {
			return nil, invalid("targets[%d].priority must be between 0 and %d", i, maxPriority)
		}
		w := 1
		if t.Weight != nil {
			w = *t.Weight
		}
		if err := checkWeight("targets.weight", w); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(out, func(o store.Target) bool { return o.DeploymentID == t.DeploymentID }) {
			return nil, invalid("deployment %q is a target twice", t.DeploymentID)
		}
		out = append(out, store.Target{DeploymentID: t.DeploymentID, Priority: t.Priority, Weight: w})
	}
	return out, nil
}

func (h *handler) createAlias(r *http.Request) (any, error) {
	var req struct {
		Name        string       `json:"name"`
		DisplayName string       `json:"display_name"`
		Targets     []targetJSON `json:"targets"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := checkToken("name", req.Name, maxNameLen); err != nil {
		return nil, err
	}
	if err := checkOptionalName("display_name", req.DisplayName); err != nil {
		return nil, err
	}
	ts, err := targets(req.Targets)
	if err != nil {
		return nil, err
	}
	a, err := h.cfg.Store.CreateAlias(r.Context(), store.Alias{Name: req.Name, DisplayName: req.DisplayName, Targets: ts})
	if err != nil {
		return nil, err
	}
	return viewAlias(a), nil
}

func (h *handler) listAliases(r *http.Request) (any, error) {
	as, err := h.cfg.Store.ListAliases(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]aliasView, 0, len(as))
	for _, a := range as {
		out = append(out, viewAlias(a))
	}
	return listView{Data: out}, nil
}

func (h *handler) getAlias(r *http.Request) (any, error) {
	a, err := h.cfg.Store.GetAlias(r.Context(), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	return viewAlias(a), nil
}

// updateAlias replaces the targets whole when named.
func (h *handler) updateAlias(r *http.Request) (any, error) {
	req, err := decodePatch(r, atCreation("name", "kind"), []string{"display_name", "targets"})
	if err != nil {
		return nil, err
	}
	var (
		name string
		tj   []targetJSON
	)
	hasName, err := req.field("display_name", &name)
	if err != nil {
		return nil, err
	}
	if err := checkOptionalName("display_name", name); err != nil {
		return nil, err
	}
	hasTargets, err := req.field("targets", &tj)
	if err != nil {
		return nil, err
	}
	var ts []store.Target
	if hasTargets {
		if ts, err = targets(tj); err != nil {
			return nil, err
		}
	}
	a, err := h.cfg.Store.UpdateAlias(r.Context(), r.PathValue("name"), func(a *store.Alias) error {
		if hasName {
			a.DisplayName = name
		}
		if hasTargets {
			a.Targets = ts
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return viewAlias(a), nil
}

func (h *handler) deleteAlias(r *http.Request) (any, error) {
	name := r.PathValue("name")
	if err := h.cfg.Store.DeleteAlias(r.Context(), name); err != nil {
		return nil, err
	}
	return map[string]string{"type": "alias_deleted", "name": name}, nil
}
