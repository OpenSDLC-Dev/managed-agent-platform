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

// nameHash is how many hex digits of a name's SHA-256 its shortened form ends
// in: 64 bits, so two long names one request carries are vanishingly unlikely
// to shorten alike, and Tools refuses a pair that does.
const nameHash = 16

// FunctionName is the function name a Chat Completions upstream is sent for a
// Messages tool name. A name that fits is sent as it is. A longer one is sent
// as its first 47 bytes, cut back to a rune boundary, then "_" and nameHash
// hex digits of its SHA-256. The model still reads what the tool is from the
// kept head, and the same name always shortens the same way, so a request's
// tools, its tool_choice and the calls its history made agree.
func FunctionName(name string) string {
	if len(name) <= maxFunctionName {
		return name
	}
	cut := maxFunctionName - 1 - nameHash
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	sum := sha256.Sum256([]byte(name))
	return name[:cut] + "_" + hex.EncodeToString(sum[:])[:nameHash]
}

// Names reads an answer's function names back as the tool names a request
// sent them for. It holds the shortened names only, so a nil Names reads
// every name as itself.
type Names map[string]string

// NamesOf is the Names of a request's tool definitions, each name read by its
// exact key, as Tools reads it. Tools has refused tools that share a function
// name before any answer is read.
func NamesOf(tools []json.RawMessage) Names {
	var n Names
	for _, t := range tools {
		var def map[string]json.RawMessage
		if json.Unmarshal(t, &def) != nil {
			continue
		}
		name, _ := text(def, "name")
		if fn := FunctionName(name); fn != name {
			if n == nil {
				n = Names{}
			}
			n[fn] = name
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
