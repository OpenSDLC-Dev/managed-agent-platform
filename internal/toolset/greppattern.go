package toolset

import (
	"regexp"
	"strings"
)

// multiline is the four PCRE patterns a multiline search runs, each built from
// the model's pattern so that its leading start-of-pattern items stay first and
// (?sm) — rg -U --multiline-dotall's meaning: . crosses a newline, ^ and $
// still match at each line — comes after them.
type multiline struct {
	// first is the pattern itself: every match the search makes, in order,
	// the empty ones skipped (grep -o's scan).
	first string
	// anywhere matches where first does and also where rg's ^ matches and
	// PCRE's does not, at the very end of a file after its final newline: the
	// files content mode can print anything for, an empty match at that end
	// printing its before-context.
	anywhere string
	// listed has a match that starts before the end of the file — the files
	// rg -U counts and lists, an empty match at the very end counting for
	// neither.
	listed string
	// scan is first's scan with every empty match made visible: where the
	// pattern's own match is empty, scan consumes the next character instead,
	// which leaves the scan where grep's own step past an empty match would.
	// Matching the pattern twice, once atomic and once as a lookahead, inside
	// a branch reset keeps each copy's capture groups numbered as the
	// model's, so a back-reference means what it meant.
	scan string
	// atEnd matches only where the pattern, its ^ read as rg's, matches at the
	// very end of the file.
	atEnd string
}

// startItem is one PCRE2 start-of-pattern item, which is valid only at the
// very start of a pattern.
var startItem = regexp.MustCompile(`^\(\*(?:UTF|UCP|CRLF|CR|LF|ANYCRLF|ANY|NUL|BSR_ANYCRLF|BSR_UNICODE|NOTEMPTY_ATSTART|NOTEMPTY|NO_AUTO_POSSESS|NO_DOTSTAR_ANCHOR|NO_JIT|NO_START_OPT|LIMIT_(?:DEPTH|HEAP|MATCH|RECURSION)=[0-9]+)\)`)

// multilinePatterns builds a multiline search's patterns, and reports whether
// the search needs them at all. rg -U runs line by line, as if -U were not
// given, when its regex cannot match a newline (grep-searcher's
// multi_line_with_matcher); its count then counts lines rather than matches.
// A pattern that cannot match one is answered the same way here, by the
// line-oriented search.
func multilinePatterns(pattern string) (multiline, bool) {
	var start strings.Builder
	for {
		item := startItem.FindString(pattern)
		if item == "" {
			break
		}
		start.WriteString(item)
		pattern = pattern[len(item):]
	}
	body := escapeNewlines(pattern)
	if !canMatchNewline(body) {
		return multiline{}, false
	}
	s, alt := start.String(), endCaret(body)
	return multiline{
		first:    s + "(?sm)" + body,
		anywhere: s + "(?sm)" + alt,
		listed:   s + `(?sm)(?=[\s\S])(?:` + body + ")",
		scan:     s + "(*NOTEMPTY)(?sm)(?|(?>" + body + ")|(?=" + body + `)[\s\S])`,
		atEnd:    s + `(?sm)\z(?:` + alt + ")",
	}, true
}

// endCaret rewrites each ^ in a pattern so that it also matches at the very
// end of the subject after a newline, as rg's multi-line ^ does and PCRE's,
// without PCRE2_ALT_CIRCUMFLEX, does not. Only there do the two differ, so a
// search for matches that start before the end needs no rewrite. A ^ that is
// escaped, quoted, in a class or a (?#…) comment, or an option setting such as
// (?^), is left alone.
func endCaret(p string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case quoted:
			if strings.HasPrefix(p[i:], `\E`) {
				quoted = false
				b.WriteByte('\\')
				i++
				c = p[i]
			}
			b.WriteByte(c)
		case c == '\\' && i+1 < len(p):
			quoted = p[i+1] == 'Q'
			b.WriteString(p[i : i+2])
			i++
		case c == '[':
			end, _ := classMatchesNewline(p, i)
			end = min(end, len(p)-1)
			b.WriteString(p[i : end+1])
			i = end
		case strings.HasPrefix(p[i:], "(?#"):
			end := strings.IndexByte(p[i:], ')')
			if end < 0 {
				end = len(p) - i - 1
			}
			b.WriteString(p[i : i+end+1])
			i += end
		case c == '^' && !strings.HasSuffix(p[:i], "(?"):
			b.WriteString(`(?:^|(?<=\n)\z)`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapeNewlines writes each newline character in a pattern as \n: rg -U
// takes a pattern spanning lines, and grep -P refuses one. A newline inside
// \Q…\E closes the quote around its escape; a quote left open is closed, so
// that the groups multilinePatterns wraps the pattern in still close.
func escapeNewlines(p string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case quoted && strings.HasPrefix(p[i:], `\E`):
			quoted = false
			b.WriteString(`\E`)
			i++
		case quoted && c == '\n':
			b.WriteString(`\E\n\Q`)
		case quoted:
			b.WriteByte(c)
		case c == '\\' && i+1 < len(p):
			i++
			switch p[i] {
			case '\n':
				b.WriteString(`\n`)
				continue
			case 'Q':
				quoted = true
			}
			b.WriteByte('\\')
			b.WriteByte(p[i])
		case c == '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		b.WriteString(`\E`)
	}
	return b.String()
}

// canMatchNewline is rg's test for searching a file whole (regex's
// non_matching_bytes, which multi_line_with_matcher reads): whether the
// pattern might match a newline character — which, by a FIXME rg carries, any
// anchor ^, $, \A or \z also counts as doing. It answers conservatively: false
// only for a pattern built from constructs none of which can, and true for
// anything it does not recognise. A wrong true costs a count of matches where
// rg counts lines; a wrong false would lose every match that spans a line.
func canMatchNewline(p string) bool {
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\n', '.', '^', '$':
			return true
		case '[':
			end, nl := classMatchesNewline(p, i)
			if nl {
				return true
			}
			i = end
		case '\\':
			if i+1 == len(p) {
				return true
			}
			i++
			switch e := p[i]; {
			case e == 'Q':
				end := strings.Index(p[i+1:], `\E`)
				if end < 0 {
					return strings.Contains(p[i+1:], "\n")
				}
				if strings.Contains(p[i+1:i+1+end], "\n") {
					return true
				}
				i += end + 2
			case e == 'N':
				if i+1 < len(p) && p[i+1] == '{' {
					return true
				}
			case e == 'p':
				end, ok := safeProperty(p, i)
				if !ok {
					return true
				}
				i = end
			case strings.IndexByte("wdShVbBKEtrfae", e) >= 0, isPunct(e), e >= 0x80:
			default:
				return true
			}
		}
	}
	return false
}

// classMatchesNewline reads the character class opening at p[i] and returns
// the index of its closing ] and whether it might match a newline.
func classMatchesNewline(p string, i int) (int, bool) {
	j := i + 1
	neg := j < len(p) && p[j] == '^'
	if neg {
		j++
	}
	// definite: the class surely holds a newline; maybe: it might.
	definite, maybe := false, false
	first := true
	for ; j < len(p); j++ {
		c := p[j]
		switch {
		case c == ']' && !first:
			if neg {
				return j, !definite
			}
			return j, maybe
		case c == '\n':
			definite, maybe = true, true
		case c == '[' && j+1 < len(p) && p[j+1] == ':':
			end := strings.Index(p[j:], ":]")
			if end < 0 {
				return len(p), true
			}
			switch name := p[j+2 : j+end]; name {
			case "space":
				definite, maybe = true, true
			case "alpha", "digit", "alnum", "upper", "lower", "punct", "xdigit", "word", "blank", "graph", "print":
			default:
				maybe = true
			}
			j += end + 1
		case c == '\\':
			if j+1 == len(p) {
				return len(p), true
			}
			j++
			switch e := p[j]; {
			case e == 'n' || e == 's' || e == 'v':
				definite, maybe = true, true
			case strings.IndexByte("wdShVbtrfae", e) >= 0, isPunct(e), e >= 0x80:
			default:
				maybe = true
			}
		case c == '-' && !first && j+1 < len(p) && p[j+1] != ']':
			// A range: its ends are the bytes either side, unless either is
			// an escape, which this does not read.
			lo, hi := p[j-1], p[j+1]
			if lo == '\\' || hi == '\\' || hi == '[' || (lo <= '\n' && '\n' <= hi) {
				maybe = true
			}
		}
		first = false
	}
	return len(p), true
}

// safeProperty reads the \p property at p[i] (the p) and reports where it
// ends and whether it is a general category that holds no newline — a letter,
// mark, number, punctuation, symbol or space separator.
func safeProperty(p string, i int) (int, bool) {
	if i+1 >= len(p) {
		return i, false
	}
	name, end := p[i+1:i+2], i+1
	if p[i+1] == '{' {
		close := strings.IndexByte(p[i+1:], '}')
		if close < 0 {
			return i, false
		}
		name, end = p[i+2:i+1+close], i+1+close
	}
	switch name {
	case "L", "Lu", "Ll", "Lt", "Lm", "Lo", "L&", "M", "Mn", "Mc", "Me", "N", "Nd", "Nl", "No",
		"P", "Pc", "Pd", "Ps", "Pe", "Pi", "Pf", "Po", "S", "Sm", "Sc", "Sk", "So", "Zs", "Xan", "Xwd":
		return end, true
	}
	return end, false
}

// isPunct is ASCII punctuation, which a backslash makes literal.
func isPunct(c byte) bool {
	return c < 0x80 && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~ ", c) >= 0
}
