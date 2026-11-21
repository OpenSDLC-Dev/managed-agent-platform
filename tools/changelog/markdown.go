package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	listItemRe = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])([ \t]+)`)
	backticks  = regexp.MustCompile("`+")
	// The end anchor keeps wrapped prose such as "[expired]: computed at
	// read time" from being treated as a reference definition.
	refDefTargetRe = regexp.MustCompile(`^\[[^\]]+\]:[ \t]*(\S+)[ \t]*(?:"[^"]*"|'[^']*'|\([^)]*\))?[ \t]*$`)
	linkSchemeRe   = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
	linkEscapeRe   = regexp.MustCompile(`\\([[:punct:]])`)
)

// codeLines recognizes fenced examples and indented code blocks in changelog
// prose. A missing closing fence is an error because it can hide later releases.
func codeLines(lines []string) ([]bool, error) {
	code := make([]bool, len(lines))
	var marker byte
	var width, openedAt, quoteDepth, listIndent, fenceListQuotes int
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
		indent, trim := markdownIndent(line)
		if width != 0 {
			if fenceListQuotes != 0 {
				body, depth := quoteBody(trim, fenceListQuotes)
				if indent < listIndent || indent > listIndent+3 || depth != fenceListQuotes {
					return nil, fmt.Errorf("unclosed Markdown fence at line %d", openedAt)
				}
				indent, trim = markdownIndent(body)
				indent += listIndent
			}
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
		innerQuotes := 0
		if item := listContentStart(line); item > 0 {
			listIndent = item
			var content string
			content, innerQuotes = quoteBody(line[item:], -1)
			indent, trim = markdownIndent(content)
			indent += listIndent
			paragraph = false
		}
		widthHere, fenceTail := fenceRun(trim)
		if indent <= listIndent+3 && widthHere >= 3 && (trim[0] != '`' || !strings.Contains(fenceTail, "`")) {
			marker, width, openedAt, fenceListQuotes = trim[0], widthHere, i+1, innerQuotes
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

func markdownIndent(line string) (int, string) {
	trim := strings.TrimLeft(line, " \t")
	indent := 0
	for _, ch := range line[:len(line)-len(trim)] {
		if ch == '\t' {
			indent += 4 - indent%4
		} else {
			indent++
		}
	}
	return indent, trim
}

func listContentStart(line string) int {
	if item := listItemRe.FindStringSubmatchIndex(line); item != nil {
		if item[3]-item[2] > 4 {
			// Four or more spaces after the list's padding can start code.
			return item[2] + 1
		}
		return item[1]
	}
	return 0
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

// inlineCodeEnds pairs each possible opener with its nearest matching run.
// The scanner uses a pair only if it reaches the opener in prose. Backticks
// inside a destination or title are skipped along with the rest of the link.
func inlineCodeEnds(paragraph string) map[int]int {
	runs := backticks.FindAllStringIndex(paragraph, -1)
	next, ends := make(map[int]int), make(map[int]int)
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		width := run[1] - run[0]
		openWidth := width
		if escapedAt(paragraph, run[0]) {
			// Only the first backtick is escaped in prose. Inside a code
			// span, backslashes are literal and the full run can close it.
			openWidth--
		}
		if close := next[openWidth]; close != 0 {
			ends[run[0]] = close
		}
		next[width] = run[1]
	}
	return ends
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
		codeEnds := inlineCodeEnds(strings.Join(lines[start:end], "\n"))
		offset, cursor := 0, 0
		for i := start; i < end; i++ {
			line := lines[i]
			body, _ := quoteBody(line, depth)
			if item := listContentStart(body); item > 0 {
				body = body[item:]
				body, _ = quoteBody(body, -1)
			}
			body = strings.TrimLeft(body, " \t")
			prefix := len(line) - len(body)
			if ref := refDefTargetRe.FindStringSubmatchIndex(body); ref != nil && cursor <= offset+prefix {
				visit(i, textSpan{prefix + ref[2], prefix + ref[3]})
			} else {
				cursor = inlineDestinations(line, max(cursor-offset, 0), offset, codeEnds, func(span textSpan) { visit(i, span) })
			}
			offset += len(line) + 1
		}
		start = end
	}
}

func inlineDestinations(line string, cursor, offset int, codeEnds map[int]int, visit func(textSpan)) int {
	for cursor < len(line) {
		if line[cursor] == '`' {
			if end := codeEnds[offset+cursor]; end != 0 {
				cursor = end - offset
			} else {
				for cursor < len(line) && line[cursor] == '`' {
					cursor++
				}
			}
			continue
		}
		if !strings.HasPrefix(line[cursor:], "](") || escapedAt(line, cursor) {
			cursor++
			continue
		}
		start := skipLinkSpace(line, cursor+2)
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
	return offset + cursor
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

func unescapeLink(dest string) string {
	return linkEscapeRe.ReplaceAllString(dest, "${1}")
}
