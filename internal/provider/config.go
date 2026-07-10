package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// routeFile is one entry of the model_providers config file (CLAUDE.md
// principle 4: providers are constructed from configuration). `model` is the
// agent-facing model string ("*" = default route); `upstream_model` is what
// the endpoint receives (empty = pass the agent's string through). Exactly
// one of `api_key` / `api_key_env` supplies the credential — the env
// indirection keeps secrets out of config files.
type routeFile struct {
	Model         string            `json:"model"`
	Protocol      string            `json:"protocol"`
	BaseURL       string            `json:"base_url"`
	UpstreamModel string            `json:"upstream_model"`
	APIKey        string            `json:"api_key"`
	APIKeyEnv     string            `json:"api_key_env"`
	Headers       map[string]string `json:"headers"`
}

// LoadRoutes reads a model_providers JSON file into registry routes.
// Structural validation happens here (unknown keys are config typos, not
// extensions); route-level validation stays in NewRegistry.
func LoadRoutes(path string) ([]Route, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("model providers config: %w", err)
	}
	var items []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("model providers config %s: must be a JSON array: %w", path, err)
	}

	routes := make([]Route, 0, len(items))
	for i, item := range items {
		var rf routeFile
		d := json.NewDecoder(bytes.NewReader(item))
		d.DisallowUnknownFields()
		if err := d.Decode(&rf); err != nil {
			return nil, fmt.Errorf("model providers config %s, entry %d: %w", path, i, err)
		}
		key := rf.APIKey
		switch {
		case rf.APIKey != "" && rf.APIKeyEnv != "":
			return nil, fmt.Errorf("model providers config %s, entry %d: api_key and api_key_env are mutually exclusive", path, i)
		case rf.APIKeyEnv != "":
			key = os.Getenv(rf.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("model providers config %s, entry %d: environment variable %s is not set", path, i, rf.APIKeyEnv)
			}
		}
		routes = append(routes, Route{
			Model: rf.Model,
			Config: Config{
				Protocol: rf.Protocol,
				Model:    rf.UpstreamModel,
				BaseURL:  rf.BaseURL,
				APIKey:   key,
				Headers:  rf.Headers,
			},
		})
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("model providers config %s: no routes defined", path)
	}
	return routes, nil
}
