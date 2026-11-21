package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	listItemRe = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])([ \t]+)`)
	backticks  = regexp.MustCompile("`+")
	blockEndRe = regexp.MustCompile(`^ {0,3}(?:=+|-+|(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})[ \t]*$`)
	// The end anchor keeps wrapped prose such as "[expired]: computed at
	// read time" from being treated as a reference definition.
	refDefTargetRe = regexp.MustCompile(`^\[[^\]]+\]:[ \t]*(\S+)[ \t]*(?:"[^"]*"|'[^']*'|\([^)]*\))?[ \t]*$`)
	linkSchemeRe   = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
	linkEscapeRe   = regexp.MustCompile(`\\([[:punct:]])`)
)

type markdownLine uint8

const (
	proseLine markdownLine = iota
	codeLine
	boundaryLine
	listItemLine
	blankLine
)

// markdownLines classifies block boundaries and code using indentation within
// the current container. A missing closing fence can hide later releases.
func markdownLines(lines []string) ([]markdownLine, error) {
	layout := make([]markdownLine, len(lines))
	var marker byte
	var width, openedAt, quoteDepth, listIndent, fenceListQuotes int
	var listIndents []int
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
			listIndents = listIndents[:0]
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
			layout[i] = codeLine
			if indent >= listIndent && indent <= listIndent+3 && widthHere >= width && trim[0] == marker && strings.TrimSpace(tail) == "" {
				width = 0
			}
			continue
		}
		if trim == "" {
			layout[i] = blankLine
			paragraph = false
			continue
		}
		for len(listIndents) != 0 && indent < listIndents[len(listIndents)-1] {
			listIndents = listIndents[:len(listIndents)-1]
		}
		listIndent = 0
		if len(listIndents) != 0 {
			listIndent = listIndents[len(listIndents)-1]
		}
		innerQuotes := 0
		if item := listContentStart(trim); item > 0 && indent <= listIndent+3 && !blockEndRe.MatchString(trim) {
			listIndent = indent + item
			listIndents = append(listIndents, listIndent)
			var content string
			content, innerQuotes = quoteBody(trim[item:], -1)
			indent, trim = markdownIndent(content)
			indent += listIndent
			layout[i] = listItemLine
			paragraph = false
		}
		widthHere, fenceTail := fenceRun(trim)
		if indent <= listIndent+3 && widthHere >= 3 && (trim[0] != '`' || !strings.Contains(fenceTail, "`")) {
			marker, width, openedAt, fenceListQuotes = trim[0], widthHere, i+1, innerQuotes
			layout[i], paragraph = codeLine, false
			continue
		}
		if indent >= listIndent+4 && !paragraph {
			layout[i] = codeLine
			continue
		}
		paragraph = indent > listIndent+3 || !markdownBlockBoundary(trim)
		if !paragraph {
			layout[i] = boundaryLine
		}
	}
	if width != 0 {
		return nil, fmt.Errorf("unclosed Markdown fence at line %d", openedAt)
	}
	return layout, nil
}

func markdownBlockBoundary(line string) bool {
	return headingRe.MatchString(line) || blockEndRe.MatchString(line)
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
func markdownDestinations(lines []string, layout []markdownLine, visit func(int, textSpan)) {
	for start := 0; start < len(lines); {
		if layout[start] == codeLine || layout[start] == blankLine {
			start++
			continue
		}
		end := start + 1
		_, depth := quoteBody(lines[start], -1)
		if layout[start] != boundaryLine {
			for end < len(lines) && layout[end] == proseLine {
				_, nextDepth := quoteBody(lines[end], -1)
				if depth != nextDepth {
					break
				}
				end++
			}
		}
		codeEnds := inlineCodeEnds(strings.Join(lines[start:end], "\n"))
		offset, cursor := 0, 0
		definitions := true
		for i := start; i < end; i++ {
			line := lines[i]
			body, _ := quoteBody(line, depth)
			body = strings.TrimLeft(body, " \t")
			if item := listContentStart(body); item > 0 {
				body = body[item:]
				body, _ = quoteBody(body, -1)
			}
			body = strings.TrimLeft(body, " \t")
			prefix := len(line) - len(body)
			if ref := refDefTargetRe.FindStringSubmatchIndex(body); definitions && ref != nil && cursor <= offset+prefix {
				visit(i, textSpan{prefix + ref[2], prefix + ref[3]})
			} else {
				definitions = false
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
