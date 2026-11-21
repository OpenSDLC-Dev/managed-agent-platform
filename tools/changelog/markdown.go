package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	listItemRe = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])[ \t]+`)
	backticks  = regexp.MustCompile("`+")
	// The end anchor keeps wrapped prose such as "[expired]: computed at
	// read time" from being treated as a reference definition.
	refDefTargetRe = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:[ \t]*(\S+)[ \t]*(?:"[^"]*"|'[^']*'|\([^)]*\))?[ \t]*$`)
	linkSchemeRe   = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
)

// codeLines recognizes fenced examples and indented code blocks in changelog
// prose. A missing closing fence is an error because it can hide later releases.
func codeLines(lines []string) ([]bool, error) {
	code := make([]bool, len(lines))
	var marker byte
	var width, openedAt, quoteDepth, listIndent int
	paragraph := false
	for i, raw := range lines {
		limit := -1
		if width != 0 {
			limit = quoteDepth
		}
		line, depth := quoteBody(raw, limit)
		if depth != quoteDepth {
			if width != 0 {
				return nil, fmt.Errorf("unclosed Markdown fence at line %d", openedAt)
			}
			quoteDepth, listIndent, paragraph = depth, 0, false
		}
		trim := strings.TrimLeft(line, " \t")
		indent := 0
		for _, ch := range line[:len(line)-len(trim)] {
			if ch == '\t' {
				indent += 4 - indent%4
			} else {
				indent++
			}
		}
		if width != 0 {
			widthHere, tail := fenceRun(trim)
			code[i] = true
			if indent >= listIndent && indent <= listIndent+3 && widthHere >= width && trim[0] == marker && strings.TrimSpace(tail) == "" {
				width = 0
			}
			continue
		}
		if trim == "" {
			paragraph = false
			continue
		}
		if indent < listIndent {
			listIndent = 0
		}
		if item := listItemRe.FindStringIndex(line); item != nil {
			listIndent = item[1]
			indent, trim = listIndent, line[item[1]:]
		}
		widthHere, fenceTail := fenceRun(trim)
		if indent <= listIndent+3 && widthHere >= 3 && (trim[0] != '`' || !strings.Contains(fenceTail, "`")) {
			marker, width, openedAt = trim[0], widthHere, i+1
			code[i], paragraph = true, false
			continue
		}
		if indent >= listIndent+4 && !paragraph {
			code[i] = true
			continue
		}
		paragraph = !headingRe.MatchString(line)
	}
	if width != 0 {
		return nil, fmt.Errorf("unclosed Markdown fence at line %d", openedAt)
	}
	return code, nil
}

// quoteBody removes the blockquote markers and their optional padding. Inside
// a fence, only the markers from its opener are structural.
func quoteBody(line string, limit int) (string, int) {
	depth := 0
	for depth != limit {
		i := 0
		for i < len(line) && i < 3 && line[i] == ' ' {
			i++
		}
		if i == len(line) || line[i] != '>' {
			break
		}
		depth++
		line = line[i+1:]
		if len(line) != 0 && (line[0] == ' ' || line[0] == '\t') {
			line = line[1:]
		}
	}
	return line, depth
}

func fenceRun(line string) (int, string) {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, line
	}
	i := 1
	for i < len(line) && line[i] == line[0] {
		i++
	}
	return i, line[i:]
}

type textSpan struct{ start, end int }

// inlineCode finds matching runs of backticks in one paragraph. An escaped
// run starts with one literal backtick. Inside a span backslashes are literal,
// so a closing run must match the complete run's length.
func inlineCode(paragraph string) []textSpan {
	runs := backticks.FindAllStringIndex(paragraph, -1)
	next := make(map[int]int)
	closers := make([]int, len(runs))
	escaped := make([]bool, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		escaped[i] = escapedAt(paragraph, run[0])
		width := run[1] - run[0]
		openWidth := width
		if escaped[i] {
			openWidth--
		}
		closers[i] = next[openWidth]
		next[width] = i + 1
	}
	var spans []textSpan
	for i := 0; i < len(runs); i++ {
		if close := closers[i]; close != 0 {
			start := runs[i][0]
			if escaped[i] {
				start++
			}
			i = close - 1
			spans = append(spans, textSpan{start, runs[i][1]})
		}
	}
	return spans
}

func escapedAt(line string, at int) bool {
	backslashes := 0
	for at > 0 && line[at-1] == '\\' {
		at--
		backslashes++
	}
	return backslashes%2 != 0
}

// markdownDestinations finds source ranges so rewrites preserve the bytes
// around the destination. Changelog links and definitions use single-line
// destinations. Inline code can span continuation lines of a paragraph.
func markdownDestinations(lines []string, code []bool, visit func(int, textSpan)) {
	for start := 0; start < len(lines); {
		if code[start] || strings.TrimSpace(lines[start]) == "" {
			start++
			continue
		}
		end := start + 1
		first, depth := quoteBody(lines[start], -1)
		if !headingRe.MatchString(first) {
			for end < len(lines) && !code[end] {
				body, nextDepth := quoteBody(lines[end], -1)
				if depth != nextDepth || strings.TrimSpace(body) == "" || headingRe.MatchString(body) || listItemRe.MatchString(body) {
					break
				}
				end++
			}
		}
		spans := inlineCode(strings.Join(lines[start:end], "\n"))
		offset := 0
		for i := start; i < end; i++ {
			line := lines[i]
			outsideCode := func(at int) bool {
				pos := offset + at
				for len(spans) > 0 && spans[0].end <= pos {
					spans = spans[1:]
				}
				return len(spans) == 0 || spans[0].start > pos
			}
			body, _ := quoteBody(line, depth)
			prefix := len(line) - len(body)
			if ref := refDefTargetRe.FindStringSubmatchIndex(body); ref != nil && outsideCode(prefix) {
				visit(i, textSpan{prefix + ref[2], prefix + ref[3]})
			} else {
				inlineDestinations(line, outsideCode, func(span textSpan) { visit(i, span) })
			}
			offset += len(line) + 1
		}
		start = end
	}
}

func inlineDestinations(line string, outsideCode func(int) bool, visit func(textSpan)) {
	for cursor := 0; cursor < len(line); {
		opener := strings.Index(line[cursor:], "](")
		if opener < 0 {
			return
		}
		opener += cursor
		cursor = opener + 2
		if !outsideCode(opener) || escapedAt(line, opener) {
			continue
		}
		start := skipLinkSpace(line, cursor)
		end, depth := start, 0
		for ; end < len(line); end++ {
			ch := line[end]
			if ch == ' ' || ch == '\t' || ch == '\r' || ch == ')' && depth == 0 {
				break
			}
			if ch == '\\' && end+1 < len(line) && strings.ContainsRune(`\()`, rune(line[end+1])) {
				end++
				continue
			}
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
			}
		}
		visit(textSpan{start, end})
		cursor = end
		if title := skipLinkSpace(line, end); title > end && title < len(line) {
			cursor = skipLinkTitle(line, title, end)
		}
	}
}

func skipLinkSpace(line string, start int) int {
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	return start
}

func skipLinkTitle(line string, start, fallback int) int {
	close := line[start]
	switch close {
	case '(':
		close = ')'
	case '"', '\'':
	default:
		return fallback
	}
	for i := start + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case close:
			end := skipLinkSpace(line, i+1)
			if end < len(line) && line[end] == ')' {
				return end + 1
			}
			return fallback
		}
	}
	return fallback
}

func needsLinkBase(dest string) bool {
	return dest != "" && !strings.HasPrefix(dest, "#") && !linkSchemeRe.MatchString(dest)
}
