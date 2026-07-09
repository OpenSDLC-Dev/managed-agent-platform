package domain

import "time"

// EnvironmentKind is where sessions in this environment run.
type EnvironmentKind string

const (
	// EnvCloud: the platform provisions and drives the sandbox (Docker/K8s).
	EnvCloud EnvironmentKind = "cloud"
	// EnvSelfHosted: the environment is a work queue; a customer-run worker
	// pulls work and executes tools (BYOC). Same pull protocol as cloud.
	EnvSelfHosted EnvironmentKind = "self_hosted"
)

// NetworkingType controls sandbox egress.
type NetworkingType string

const (
	NetUnrestricted NetworkingType = "unrestricted" // default; all egress except a safety blocklist
	NetLimited      NetworkingType = "limited"      // only AllowedHosts
)

// Networking is the sandbox egress policy. For "limited", AllowedHosts is a
// list of bare hostnames or "*.example.com" wildcards (no scheme/port/path).
type Networking struct {
	Type                 NetworkingType `json:"type"`
	AllowedHosts         []string       `json:"allowed_hosts,omitempty"`
	AllowMCPServers      bool           `json:"allow_mcp_servers,omitempty"`
	AllowPackageManagers bool           `json:"allow_package_managers,omitempty"`
}

// EnvironmentConfig is the sandbox spec. Packages maps a package manager
// ("apt","cargo","gem","go","npm","pip") to a list of packages (optionally
// version-pinned).
type EnvironmentConfig struct {
	Type       EnvironmentKind     `json:"type"`
	Packages   map[string][]string `json:"packages,omitempty"`
	Networking Networking          `json:"networking,omitempty"`
}

// Environment is a sandbox configuration referenced by sessions. It is not
// versioned; it persists until archived/deleted.
type Environment struct {
	Scope

	ID          ID                `json:"id"` // env_…
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Kind        EnvironmentKind   `json:"kind"`
	State       string            `json:"state"`
	Config      EnvironmentConfig `json:"config"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}
