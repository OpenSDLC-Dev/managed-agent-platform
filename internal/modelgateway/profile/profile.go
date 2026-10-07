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
//
// A profile also says what the gateway changes on its way to the vendor's
// Anthropic endpoint, under the plan's edit policy: pass through by default,
// edit only what the platform's own traffic needs, refuse only what the
// vendor documents it ignores where the result depends on it. Each edit
// cites its evidence where it is set.
package profile

import (
	"encoding/json"
	"regexp"
)

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

// Profile is one vendor's data. What the gateway changes on the way to the
// vendor is its own business, outside the admin API's view of a profile.
type Profile struct {
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name"`
	Protocols   []Protocol `json:"protocols"`
	Hosts       []Host     `json:"hosts"`

	// BearerAuth sends the provider's key to its Anthropic endpoint as
	// Authorization: Bearer rather than x-api-key, Anthropic's own header.
	BearerAuth bool `json:"-"`
	// FlattenSearchResults sends each search_result block in a tool_result
	// as text, for a vendor that refuses the block, in the rendering the
	// brain's flatten_search_results uses (internal/provider/anthropic).
	FlattenSearchResults bool `json:"-"`
	// Ignores names what in a request to model, a deployment's upstream
	// model id, the vendor documents it ignores although the answer depends
	// on it, or "" for nothing; no such deployment serves the request.
	Ignores func(model string, request map[string]json.RawMessage) string `json:"-"`
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
	// DeepSeek's Anthropic API guide lists x-api-key alone among the headers,
	// "Fully Supported". The endpoint refuses a search_result block with a
	// 422, ``unknown variant `search_result` `` (probed 2026-10-07).
	{Name: "deepseek", DisplayName: "DeepSeek", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, BaseURL: "https://api.deepseek.com/anthropic"},
		{Protocol: OpenAI, BaseURL: "https://api.deepseek.com"},
	}, FlattenSearchResults: true, Ignores: deepseekIgnores},
	// MiniMax's Messages API reference takes either header and says
	// "Authorization: Bearer <API_KEY> is recommended". It refuses a
	// search_result block in a tool_result with a 400, "invalid tool_result
	// content (2013)" (#565; probed 2026-10-07).
	{Name: "minimax", DisplayName: "MiniMax", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://api.minimax.cn/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.minimax.io/anthropic"},
		{Protocol: OpenAI, Region: RegionCN, BaseURL: "https://api.minimax.cn/v1"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.minimax.io/v1"},
	}, BearerAuth: true, FlattenSearchResults: true, Ignores: minimaxIgnores},
	// BigModel's Claude API compatibility guide sends the key as x-api-key.
	{Name: "zhipu", DisplayName: "Zhipu (BigModel · Z.ai)", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://open.bigmodel.cn/api/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.z.ai/api/anthropic"},
		{Protocol: OpenAI, Region: RegionCN, BaseURL: "https://open.bigmodel.cn/api/paas/v4"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.z.ai/api/paas/v4"},
	}},
	// Kimi documents its OpenAI-compatible host on the international site
	// only, and a Bearer token as the one way to send a key: its Claude Code
	// guide says to remove ANTHROPIC_API_KEY, which sends x-api-key.
	{Name: "moonshot", DisplayName: "Moonshot (Kimi)", Protocols: both, Hosts: []Host{
		{Protocol: Anthropic, Region: RegionCN, BaseURL: "https://api.moonshot.cn/anthropic"},
		{Protocol: Anthropic, Region: RegionInternational, BaseURL: "https://api.moonshot.ai/anthropic"},
		{Protocol: OpenAI, Region: RegionInternational, BaseURL: "https://api.moonshot.ai/v1"},
	}, BearerAuth: true},
	{Name: "anthropic-generic", DisplayName: "Any Anthropic Messages endpoint", Protocols: []Protocol{Anthropic}},
	{Name: "openai-generic", DisplayName: "Any OpenAI-compatible endpoint", Protocols: []Protocol{OpenAI}},
}

// The Ignores hooks refuse only what bounds the answer itself, which a
// caller's code may rely on. A vendor's documented ignoring of a sampling
// knob (top_k), of what shapes the context rather than the answer
// (context_management, cache_control), or of a server-side feature the
// model then answers without (mcp_servers, container) passes through, as
// does DeepSeek's of tool_result's is_error: a failed tool's result says so
// in its text, the brain's included. Fields are read by their exact keys,
// as the vendors read them.
//
// deepseekIgnores: DeepSeek's Anthropic API guide has tool_choice auto, any
// and tool "Supported (disable_parallel_tool_use is ignored)", and a model
// free to call tools in parallel may answer with several calls where the
// caller allowed one.
func deepseekIgnores(_ string, req map[string]json.RawMessage) string {
	var choice map[string]json.RawMessage
	var ban bool
	if json.Unmarshal(req["tool_choice"], &choice) == nil &&
		json.Unmarshal(choice["disable_parallel_tool_use"], &ban) == nil && ban {
		return "tool_choice.disable_parallel_tool_use"
	}
	return ""
}

// minimaxIgnores: MiniMax's Anthropic SDK guide (text-anthropic-api) has
// stop_sequences "This parameter will be ignored", so an answer runs past
// the sequence the caller stops at; and for its M2.x models thinking
// disabled is "Accepted but ignored; thinking remains on", so an answer
// spends the caller's max_tokens on thinking it turned off. MiniMax-M3
// honors disabled, and M3.1-Flash-Preview refuses it with a 400 of its own.
func minimaxIgnores(model string, req map[string]json.RawMessage) string {
	var stops []json.RawMessage
	if json.Unmarshal(req["stop_sequences"], &stops) == nil && len(stops) > 0 {
		return "stop_sequences"
	}
	var thinking map[string]json.RawMessage
	var typ string
	if minimaxM2.MatchString(model) && json.Unmarshal(req["thinking"], &thinking) == nil &&
		json.Unmarshal(thinking["type"], &typ) == nil && typ == "disabled" {
		return "thinking.type"
	}
	return ""
}

// minimaxM2 matches MiniMax's M2.x model ids, MiniMax-M2 and MiniMax-M2.7
// alike, in any case.
var minimaxM2 = regexp.MustCompile(`(?i)^minimax-m2([.-]|$)`)

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
