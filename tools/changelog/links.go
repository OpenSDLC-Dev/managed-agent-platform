package main

import (
	"fmt"
	"net/url"
	"strings"
)

// rewriteLinks returns rendered lines and the first bad target. Notes can use
// the rendered length even if the group will be omitted. Other callers reject
// any error before publishing the returned lines.
func rewriteLinks(lines []string, layout []markdownLine, target func(string) (string, error)) ([]string, error) {
	out := append([]string(nil), lines...)
	var firstErr error
	var b strings.Builder
	line, written := -1, 0
	flush := func() {
		if line >= 0 {
			b.WriteString(lines[line][written:])
			out[line] = b.String()
			b.Reset()
		}
	}
	markdownDestinations(lines, layout, func(i int, span textSpan) {
		dest := lines[i][span.start:span.end]
		if !needsLinkBase(dest) {
			return
		}
		replacement, err := target(dest)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%q: %w", strings.TrimSpace(lines[i]), err)
			}
			return
		}
		if replacement == dest {
			return
		}
		if i != line {
			flush()
			line, written = i, 0
		}
		b.WriteString(lines[i][written:span.start])
		b.WriteString(replacement)
		written = span.end
	})
	flush()
	return out, firstErr
}

// rootLink maps the two repo-root spellings, leaving the choice of output
// base to the caller. Archive paths keep distinct prefixes for the inverse.
func rootLink(dest, dotBase, docsBase string) (string, error) {
	var base, rel string
	switch {
	case strings.HasPrefix(dest, "./"):
		base, rel = dotBase, dest[2:]
	case strings.HasPrefix(dest, "docs/"):
		base, rel = docsBase, dest[5:]
	case strings.HasPrefix(dest, "../"):
		return "", fmt.Errorf("parent-relative link target %q cannot be re-based reversibly", dest)
	default:
		return "", fmt.Errorf("unhandled relative link target %q", dest)
	}
	path := unescapeLink(rel)
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", fmt.Errorf("relative link target %q contains a parent segment", dest)
		}
	}
	return base + rel, nil
}

func absolutizeLinks(lines []string, layout []markdownLine, base, version string) ([]string, error) {
	prefix := base + "/blob/v" + version + "/"
	return rewriteLinks(lines, layout, func(dest string) (string, error) {
		result, err := rootLink(dest, prefix, prefix+"docs/")
		if err == nil && base == "" {
			return "", fmt.Errorf("relative link target %q cannot be made absolute without the [Unreleased] link reference", dest)
		}
		return result, err
	})
}

func rebaseLinks(lines []string, layout []markdownLine) ([]string, error) {
	return rewriteLinks(lines, layout, func(dest string) (string, error) {
		return rootLink(dest, "../../", "../")
	})
}

func unrebaseLinks(lines []string, layout []markdownLine) []string {
	out, _ := rewriteLinks(lines, layout, func(dest string) (string, error) {
		if rel, ok := strings.CutPrefix(dest, "../../"); ok {
			return "./" + rel, nil
		}
		if rel, ok := strings.CutPrefix(dest, "../"); ok {
			return "docs/" + rel, nil
		}
		return dest, nil
	})
	return out
}
