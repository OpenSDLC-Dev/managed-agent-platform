// Package profile holds the model gateway's compiled-in vendor profiles
// (docs/plan/59_model-gateway.md, "Vendor profiles"): for each vendor the
// protocols it speaks and the hosts it publishes for each, in each region it
// runs a console for. A provider names one profile and takes each endpoint
// from the profile's hosts or from the operator; the generic profiles publish
// no host, so a provider on one always names its own.
//
// The hosts are the vendors' own (Ground truth, "Vendors", verified
// 2026-10-04): DeepSeek's API docs (api-docs.deepseek.com, guides/anthropic_api),
// MiniMax's CN and international API references, Zhipu's BigModel and Z.ai
// docs, and Kimi's platform docs (platform.kimi.ai, api/messages). A host here
// is a base URL: the Anthropic SDKs append /v1/messages to one, the OpenAI
// SDKs /chat/completions.
package profile

// Protocol is an upstream wire protocol.
type Protocol string

const (
	Anthropic Protocol = "anthropic" // Anthropic Messages
	OpenAI    Protocol = "openai"    // OpenAI-compatible APIs
)

// Region is the console a host belongs to. A vendor's CN and international
// sites issue their own keys, so a provider, one account, uses one region's
// hosts.
type Region string

const (
	RegionCN            Region = "cn"
	RegionInternational Region = "international"
)

// Host is one published endpoint. Region is empty for a vendor with one host.
type Host struct {
	Protocol Protocol `json:"protocol"`
	Region   Region   `json:"region,omitempty"`
	BaseURL  string   `json:"base_url"`
}

// Profile is one vendor's data.
type Profile struct {
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name"`
	Protocols   []Protocol `json:"protocols"`
	Hosts       []Host     `json:"hosts"`
}

// Supports reports whether a provider of this profile may have an endpoint on
// proto.
func (p Profile) Supports(proto Protocol) bool {
	for _, have := range p.Protocols {
		if have == proto {
			return true
		}
	}
	return false
}

var both = []Protocol{Anthropic, OpenAI}

var profiles = []Profile{
	{Name: "deepseek", DisplayName: "DeepSeek", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, BaseURL: "https://api.deepseek.com/anthropic"},
		{Protocol: OpenAI, BaseURL: "https://api.deepseek.com"},
	}},
	{Name: "minimax", DisplayName: "MiniMax", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://api.minimax.cn/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.minimax.io/anthropic"},
		{Protocol: OpenAI, Region: RegionCN, BaseURL: "https://api.minimax.cn/v1"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.minimax.io/v1"},
	}},
	{Name: "zhipu", DisplayName: "Zhipu (BigModel · Z.ai)", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://open.bigmodel.cn/api/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.z.ai/api/anthropic"},
		{Protocol: OpenAI, Region: RegionCN, BaseURL: "https://open.bigmodel.cn/api/paas/v4"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.z.ai/api/paas/v4"},
	}},
	// Kimi documents its OpenAI-compatible host on the international site only.
	{Name: "moonshot", DisplayName: "Moonshot (Kimi)", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://api.moonshot.cn/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.moonshot.ai/anthropic"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.moonshot.ai/v1"},
	}},
	{Name: "anthropic-generic", DisplayName: "Any Anthropic Messages endpoint", Protocols: []Protocol{Anthropic}},
	{Name: "openai-generic", DisplayName: "Any OpenAI-compatible endpoint", Protocols: []Protocol{OpenAI}},
}

// All returns every profile, as copies the caller may keep and change.
func All() []Profile {
	out := make([]Profile, len(profiles))
	for i, p := range profiles {
		out[i] = clone(p)
	}
	return out
}

// Lookup returns the profile named name.
func Lookup(name string) (Profile, bool) {
	for _, p := range profiles {
		if p.Name == name {
			return clone(p), true
		}
	}
	return Profile{}, false
}

func clone(p Profile) Profile {
	p.Protocols = append([]Protocol(nil), p.Protocols...)
	p.Hosts = append([]Host(nil), p.Hosts...)
	return p
}
