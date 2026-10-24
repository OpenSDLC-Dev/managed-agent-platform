package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"
)

// maxFunctionName is the longest function name Chat Completions takes, "a
// maximum length of 64" (checked against github.com/openai/openai-go/v3
// v3.73.0 — shared/shared.go FunctionDefinition.Name), where a Messages tool
// name runs to 128 (checked against anthropic-sdk-go v1.70.1 — spec
// components.schemas.Tool.properties.name). OpenAI refuses the whole request
// over one longer name, so a longer one is shortened on the way out and read
// back on the way in (#981).
const maxFunctionName = 64

// nameHash is how many hex digits of a name's SHA-256 its shortened form
// carries: 64 bits, so two long names one request carries are vanishingly
// unlikely to shorten alike, and Tools refuses a pair that does.
const nameHash = 16

// nameEnd is how many bytes a shortened name keeps from each end of the name.
const nameEnd = (maxFunctionName - nameHash - 2) / 2

// FunctionName is the function name a Chat Completions upstream is sent for a
// Messages tool name. A name that fits is sent as it is. A longer one is sent
// as its first and last nameEnd bytes, each cut back to a rune boundary, with
// "_", nameHash hex digits of its SHA-256 and "_" between them. Both ends
// stay, because an MCP tool's composed name opens with what every tool on its
// server shares and ends in the tool's own name. The same name always
// shortens the same way, so a request's tools, its tool_choice and the calls
// its history made agree.
func FunctionName(name string) string {
	if len(name) <= maxFunctionName {
		return name
	}
	head := nameEnd
	for head > 0 && !utf8.RuneStart(name[head]) {
		head--
	}
	tail := len(name) - nameEnd
	for tail < len(name) && !utf8.RuneStart(name[tail]) {
		tail++
	}
	sum := sha256.Sum256([]byte(name))
	return name[:head] + "_" + hex.EncodeToString(sum[:nameHash/2]) + "_" + name[tail:]
}

// Names reads an answer's function names back as the tool names a request
// sent them for. It holds the shortened names only, so a nil Names reads
// every name as itself.
type Names map[string]string

// NamesOf is the Names of a request: the names its history's calls carried,
// and its tools' names, each read by its exact key, as Messages and Tools
// read them. A call on a tool the request no longer offers still reads back
// whole. The tools win where the two meet: a name a tool goes out under reads
// back as that tool. Tools has refused two tools that share a function name
// before any answer is read.
func NamesOf(tools []json.RawMessage, history []Message) Names {
	n := Names{}
	for _, m := range history {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // a string, which holds no call
		}
		for _, b := range blocks {
			if typ, _ := text(b, "type"); typ == "tool_use" {
				name, _ := text(b, "name")
				if fn := FunctionName(name); fn != name {
					n[fn] = name
				}
			}
		}
	}
	for _, t := range tools {
		var def map[string]json.RawMessage
		_ = json.Unmarshal(t, &def)
		name, _ := text(def, "name")
		if fn := FunctionName(name); fn != name {
			n[fn] = name
		} else {
			delete(n, name)
		}
	}
	return n
}

// Tool is the tool name an answer's function name stands for.
func (n Names) Tool(function string) string {
	if name, ok := n[function]; ok {
		return name
	}
	return function
}
