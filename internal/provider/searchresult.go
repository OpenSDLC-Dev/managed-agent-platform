package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SearchResultText renders one search_result content block (a web_search hit)
// as plain text: the title and source URL on a header line, then each of the
// block's own text blocks on its own newline-terminated line, so consecutive
// results stay separated after a caller's plain join. The structure
// (citations config, block boundaries) is dropped — a documented lossy
// conversion. Shared by provider/openai, whose Chat Completions tool message
// is text-only by wire shape, and provider/anthropic's opt-in
// flatten_search_results route flag (an endpoint gap, not a wire limitation —
// see docs/DIVERGENCES.md), so the two adapters render a search_result block
// identically. The wire union says the inner content is text blocks only, so
// anything else is an upstream bug and fails loudly.
func SearchResultText(title string, source, content json.RawMessage) (string, error) {
	var src string
	if len(bytes.TrimSpace(source)) > 0 {
		if err := json.Unmarshal(source, &src); err != nil {
			return "", fmt.Errorf("search_result source must be a URL string: %w", err)
		}
	}
	var inner []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(bytes.TrimSpace(content)) > 0 {
		if err := json.Unmarshal(content, &inner); err != nil {
			return "", err
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s)\n", title, src)
	for _, c := range inner {
		if c.Type != "text" {
			return "", fmt.Errorf("unsupported block %q inside search_result content (text only)", c.Type)
		}
		sb.WriteString(c.Text)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
