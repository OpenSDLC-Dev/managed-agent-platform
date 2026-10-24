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

// maxGeminiName is the longest function name Gemini takes, one that "Must
// start with a letter or an underscore" (google.golang.org/genai 1.73.0,
// types.go FunctionDeclaration.Name). A Messages tool name may start with a
// digit or a dash, and Gemini refuses the whole request over one such name,
// so GeminiName sends it with a "_" in front (#985).
const maxGeminiName = 128

// nameHash is how many hex digits of a name's SHA-256 its shortened form
// carries: 64 bits, so two long names one request carries are vanishingly
// unlikely to shorten alike, and Tools and geminiTools refuse a pair that does.
const nameHash = 16

// FunctionName is the function name a Chat Completions upstream is sent for a
// Messages tool name. A name that fits is sent as it is. A longer one is
// shortened (shorten).
func FunctionName(name string) string {
	return shorten(name, maxFunctionName)
}

// GeminiName is the function name a Gemini upstream is sent for a Messages
// tool name. A name that starts with an ASCII letter or "_" is sent as it is,
// and any other with a "_" in front. Then a name past maxGeminiName is
// shortened (shorten), which keeps its first byte.
func GeminiName(name string) string {
	if name != "" {
		if c := name[0]; c != '_' && !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			name = "_" + name
		}
	}
	return shorten(name, maxGeminiName)
}

// shorten fits a name into max bytes. A name that fits is kept as it is. A
// longer one becomes its first and last bytes, as many of each as max leaves
// beside "_", nameHash hex digits of its SHA-256 and "_", each end cut back to
// a rune boundary. Both ends stay, because an MCP tool's composed name opens
// with what every tool on its server shares and ends in the tool's own name.
// The same name always shortens the same way, so a request's tools, its
// tool_choice and the calls its history made agree.
func shorten(name string, max int) string {
	if len(name) <= max {
		return name
	}
	end := (max - nameHash - 2) / 2
	head := end
	for head > 0 && !utf8.RuneStart(name[head]) {
		head--
	}
	tail := len(name) - end
	for tail < len(name) && !utf8.RuneStart(name[tail]) {
		tail++
	}
	sum := sha256.Sum256([]byte(name))
	return name[:head] + "_" + hex.EncodeToString(sum[:nameHash/2]) + "_" + name[tail:]
}

// Names reads an answer's function names back as the tool names a request
// sent them for. It holds the names that went out changed only, so a nil
// Names reads every name as itself.
type Names map[string]string

// NamesOf is the Names of a request to a Chat Completions upstream: the names
// its history's calls carried, in order, then its tools' names, each read by
// its exact key, as Messages and Tools read them. A call on a tool the
// request no longer offers still reads back whole. Where two names go out
// alike the later one wins, so the request's tools win over its history, and
// a later call over an earlier one. Tools has refused two tools that share a
// function name before any answer is read.
func NamesOf(tools []json.RawMessage, history []Message) Names {
	return namesOf(tools, history, FunctionName)
}

// GeminiNamesOf is NamesOf for a request to a Gemini upstream, whose names
// GeminiName makes, and whose two tools that share one geminiTools refuses.
func GeminiNamesOf(tools []json.RawMessage, history []Message) Names {
	return namesOf(tools, history, GeminiName)
}

func namesOf(tools []json.RawMessage, history []Message, function func(string) string) Names {
	n := Names{}
	read := func(name string) {
		if fn := function(name); fn != name {
			n[fn] = name
		} else {
			delete(n, name)
		}
	}
	for _, m := range history {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // a string, which holds no call
		}
		for _, b := range blocks {
			if typ, _ := text(b, "type"); typ == "tool_use" {
				name, _ := text(b, "name")
				read(name)
			}
		}
	}
	for _, t := range tools {
		var def map[string]json.RawMessage
		_ = json.Unmarshal(t, &def)
		name, _ := text(def, "name")
		read(name)
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
