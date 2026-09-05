package domain

import (
	"strings"
	"time"
)

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
// version-pinned). It carries no slot for the wire's packages.type
// discriminator (#382): that key is a render-time-only concern the API
// layer stamps on and off (api.packagesTypeEcho) — this map must never
// store it, or internal/executor's and the gate config endpoint's decode of
// this same field breaks.
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

// ValidPackageEntry reports whether entry may be handed to its package manager
// as one argument. Two shapes are refused, and only two: the empty string,
// which no manager can install, and one beginning with '-', which every
// manager reads as an option rather than a package — `--index-url=…` passed to
// `pip install` would redirect the whole install, and no quoting can stop it,
// because the entry is a single argument either way. Everything else (a pin,
// `@`, `:`, whitespace) is the manager's own syntax and passes through verbatim.
// A NUL is not this predicate's concern: the API's rejectNULBody (wire.go)
// refuses it anywhere in the request body before this runs, and jsonb could not
// store it, so no stored row ever carries one to the executor.
// The API applies this at create and update; the executor applies it again
// before building a command, so a row stored before the rule is refused at
// install rather than passed (docs/plan/40_environment-packages.md decision 6).
func ValidPackageEntry(entry string) bool {
	return entry != "" && !strings.HasPrefix(entry, "-")
}
