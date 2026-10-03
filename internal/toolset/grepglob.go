package toolset

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// globRule is grep's glob property compiled for find: re is a POSIX ERE over
// the whole path find prints for a file or directory.
type globRule struct {
	re string
	// negate is a leading "!": a path the glob matches is excluded.
	negate bool
	// dirOnly is a trailing "/": the glob matches directories alone.
	dirOnly bool
}

// compileGlob compiles grep's glob property — one rg --glob, the value taken
// whole: a space or a comma outside braces is part of the glob, as it is to rg
// — the way ripgrep 14.1.1 reads it. rg adds the glob as one gitignore line
// (ignore's GitignoreBuilder.add_line) and parses that with globset, separators
// literal and backslash escaping on; this is a port of both. A line that is
// blank, or a comment ("#…"), is no glob at all, and compileGlob returns nil.
//
// rg matches the glob against a path relative to its working directory, the
// workdir here, when the path lies under it, and against the path as walked
// when it does not. anchor is the ERE that stands for that prefix in the paths
// find prints — the quoted workdir and a slash, or "" for a root outside it —
// so an anchored glob is anchored where rg anchors it.
func compileGlob(value, anchor string) (*globRule, error) {
	line := value
	if strings.HasPrefix(line, "#") {
		return nil, nil
	}
	if !strings.HasSuffix(line, `\ `) {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	if line == "" {
		return nil, nil
	}
	r := &globRule{}
	absolute := false
	if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	} else {
		if rest, ok := strings.CutPrefix(line, "!"); ok {
			r.negate, line = true, rest
		}
		if rest, ok := strings.CutPrefix(line, "/"); ok {
			absolute, line = true, rest
		}
	}
	if rest, ok := strings.CutSuffix(line, "/"); ok {
		r.dirOnly, line = true, strings.TrimSuffix(rest, `\`)
	}
	if !absolute && !strings.Contains(line, "/") && !strings.HasPrefix(line, "**/") && line != "**" {
		line = "**/" + line
	}
	if strings.HasSuffix(line, "/**") {
		line += "/*"
	}
	tokens, err := parseGlob(line)
	if err != nil {
		return nil, fmt.Errorf("error parsing glob '%s': %w", value, err)
	}
	r.re = "^" + anchor + globEREBody(tokens) + "$"
	return r, nil
}

// globToken is one token of globset's parse.
type globToken struct {
	kind   globKind
	lit    rune
	neg    bool
	ranges [][2]rune
	alts   [][]globToken
}

type globKind int

const (
	globLiteral globKind = iota
	globAny
	globStar
	globRecursivePrefix
	globRecursiveSuffix
	globRecursiveMiddle
	globClass
	globAlternates
)

// parseGlob is globset's Parser (glob.rs) for a glob with literal_separator
// and backslash_escape set, its errors worded as globset words them.
func parseGlob(glob string) ([]globToken, error) {
	p := globParser{chars: []rune(glob), stack: [][]globToken{nil}}
	for {
		c, ok := p.bump()
		if !ok {
			break
		}
		var err error
		switch c {
		case '?':
			p.push(globToken{kind: globAny})
		case '*':
			p.star()
		case '[':
			err = p.class()
		case '{':
			if len(p.stack) > 1 {
				return nil, fmt.Errorf("nested alternate groups are not allowed")
			}
			p.stack = append(p.stack, nil)
		case '}':
			var alts [][]globToken
			for len(p.stack) >= 2 {
				alts = append(alts, p.stack[len(p.stack)-1])
				p.stack = p.stack[:len(p.stack)-1]
			}
			p.push(globToken{kind: globAlternates, alts: alts})
		case ',':
			if len(p.stack) <= 1 {
				p.push(globToken{kind: globLiteral, lit: ','})
			} else {
				p.stack = append(p.stack, nil)
			}
		case '\\':
			n, ok := p.bump()
			if !ok {
				return nil, fmt.Errorf(`dangling '\'`)
			}
			p.push(globToken{kind: globLiteral, lit: n})
		default:
			p.push(globToken{kind: globLiteral, lit: c})
		}
		if err != nil {
			return nil, err
		}
	}
	if len(p.stack) > 1 {
		return nil, fmt.Errorf("unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)")
	}
	return p.stack[0], nil
}

type globParser struct {
	chars     []rune
	i         int
	prev, cur rune
	stack     [][]globToken
}

func (p *globParser) bump() (rune, bool) {
	p.prev = p.cur
	if p.i >= len(p.chars) {
		p.cur = 0
		return 0, false
	}
	p.cur = p.chars[p.i]
	p.i++
	return p.cur, true
}

func (p *globParser) peek() (rune, bool) {
	if p.i >= len(p.chars) {
		return 0, false
	}
	return p.chars[p.i], true
}

func (p *globParser) push(t globToken) {
	p.stack[len(p.stack)-1] = append(p.stack[len(p.stack)-1], t)
}

func (p *globParser) pop() globToken {
	top := p.stack[len(p.stack)-1]
	t := top[len(top)-1]
	p.stack[len(p.stack)-1] = top[:len(top)-1]
	return t
}

// star is globset's parse_star: ** is recursive only as a whole path
// component (or a whole alternative), and two stars otherwise.
func (p *globParser) star() {
	prev := p.prev
	twoStars := func() {
		p.push(globToken{kind: globStar})
		p.push(globToken{kind: globStar})
	}
	if c, ok := p.peek(); !ok || c != '*' {
		p.push(globToken{kind: globStar})
		return
	}
	p.bump()
	if len(p.stack[len(p.stack)-1]) == 0 {
		if c, ok := p.peek(); ok && c != '/' {
			twoStars()
		} else {
			p.push(globToken{kind: globRecursivePrefix})
			p.bump()
		}
		return
	}
	if prev != '/' && (len(p.stack) <= 1 || (prev != ',' && prev != '{')) {
		twoStars()
		return
	}
	suffix := false
	switch c, ok := p.peek(); {
	case !ok:
		p.bump()
		suffix = true
	case (c == ',' || c == '}') && len(p.stack) >= 2:
		suffix = true
	case c == '/':
		p.bump()
	default:
		twoStars()
		return
	}
	switch last := p.pop(); last.kind {
	case globRecursivePrefix, globRecursiveSuffix:
		p.push(last)
	default:
		if suffix {
			p.push(globToken{kind: globRecursiveSuffix})
		} else {
			p.push(globToken{kind: globRecursiveMiddle})
		}
	}
}

// class is globset's parse_class: a backslash is a member like any other, a
// leading ] or - is literal, and a range must not run backwards.
func (p *globParser) class() error {
	t := globToken{kind: globClass}
	if c, ok := p.peek(); ok && (c == '!' || c == '^') {
		p.bump()
		t.neg = true
	}
	first, inRange := true, false
	for {
		c, ok := p.bump()
		if !ok {
			return fmt.Errorf("unclosed character class; missing ']'")
		}
		switch {
		case c == ']' && !first:
			if inRange {
				t.ranges = append(t.ranges, [2]rune{'-', '-'})
			}
			p.push(t)
			return nil
		case c == '-' && !first && !inRange:
			inRange = true
		case inRange:
			last := &t.ranges[len(t.ranges)-1]
			last[1] = c
			if last[1] < last[0] {
				return fmt.Errorf("invalid range; '%c' > '%c'", last[0], last[1])
			}
			inRange = false
		default:
			t.ranges = append(t.ranges, [2]rune{c, c})
		}
		first = false
	}
}

// globEREBody is globset's regex for tokens, written as a POSIX ERE for find's
// -regex under the C locale, where a non-ASCII character is the bytes of its
// UTF-8 and ? or a class matches one byte, as globset's (?-u) regex does.
func globEREBody(tokens []globToken) string {
	if len(tokens) == 1 && tokens[0].kind == globRecursivePrefix {
		return ".*"
	}
	var b strings.Builder
	for _, t := range tokens {
		switch t.kind {
		case globLiteral:
			b.WriteString(regexp.QuoteMeta(string(t.lit)))
		case globAny:
			b.WriteString("[^/]")
		case globStar:
			b.WriteString("[^/]*")
		case globRecursivePrefix:
			b.WriteString("(/?|.*/)")
		case globRecursiveSuffix:
			b.WriteString("/.*")
		case globRecursiveMiddle:
			b.WriteString("(/|/.*/)")
		case globClass:
			b.WriteString(bracket(t.neg, t.ranges))
		case globAlternates:
			var parts []string
			for _, alt := range t.alts {
				if re := globEREBody(alt); re != "" {
					parts = append(parts, re)
				}
			}
			if len(parts) > 0 {
				b.WriteString("(" + strings.Join(parts, "|") + ")")
			}
		}
	}
	return b.String()
}

// bracket writes a class as a POSIX bracket expression. Like globset's, the
// class is a class of bytes — its (?-u) regex spells a non-ASCII member as the
// bytes of its UTF-8, so a member contributes each of its bytes and a range
// runs from the last byte of one end to the first of the other — so it is
// worked out as a set of bytes first. A bracket has no escape, so the
// characters that are syntax inside one are placed where they are members: a ]
// first, then the rest, then [, ^ and - last.
func bracket(neg bool, ranges [][2]rune) string {
	var set [256]bool
	for _, r := range ranges {
		lo, hi := string(r[0]), string(r[1])
		if r[0] == r[1] {
			for i := 0; i < len(lo); i++ {
				set[lo[i]] = true
			}
			continue
		}
		for i := 0; i < len(lo)-1; i++ {
			set[lo[i]] = true
		}
		for b := int(lo[len(lo)-1]); b <= int(hi[0]); b++ {
			set[b] = true
		}
		for i := 1; i < len(hi); i++ {
			set[hi[i]] = true
		}
	}
	// No path holds a NUL, so a class that admits only it matches nothing,
	// and one that excludes only it matches any byte.
	set[0] = false
	special := map[byte]bool{']': set[']'], '[': set['['], '^': set['^'], '-': set['-']}
	var body strings.Builder
	for b := 1; b < 256; {
		if !set[b] || special[byte(b)] {
			b++
			continue
		}
		e := b
		for e+1 < 256 && set[e+1] && !special[byte(e+1)] {
			e++
		}
		body.WriteByte(byte(b))
		switch {
		case e == b+1:
			body.WriteByte(byte(e))
		case e > b+1:
			body.WriteByte('-')
			body.WriteByte(byte(e))
		}
		b = e + 1
	}
	members := body.String()
	if special[']'] {
		members = "]" + members
	}
	if special['['] {
		members += "["
	}
	if special['^'] {
		// First, a ^ would negate the class; only a - can lead in its place.
		if members == "" && !neg {
			if special['-'] {
				return "[-^]"
			}
			return `\^`
		}
		members += "^"
	}
	if special['-'] {
		members += "-"
	}
	switch {
	case members == "" && neg:
		return "[\x01-\xff]"
	case members == "":
		return "[^\x01-\xff]"
	case neg:
		return "[^" + members + "]"
	}
	return "[" + members + "]"
}

// selection builds find's file test and directory-prune test from the glob and
// a type's globs, with ripgrep's precedence (ignore's Ignore.matched and
// Override.matched): the glob decides first — a path it matches is in, or out
// when it is negated, and while a positive glob exists a file it does not match
// is out; a file the glob leaves undecided falls to the type, which matches its
// base name; and one neither decides is in unless hidden, its name starting
// with a dot. A directory below the root is pruned when a negated glob matches
// it, and otherwise, unless a positive glob matches it, when it is hidden or is
// node_modules — the stand-in for the .gitignore files rg reads and this does
// not (docs/DIVERGENCES.md). A directory-only glob decides directories alone.
func selection(types []string, rule *globRule) (sel, prune []string) {
	var base []string
	switch {
	case rule != nil && !rule.negate:
		base = []string{"-false"}
	case len(types) > 0:
		for i, t := range types {
			if i > 0 {
				base = append(base, "-o")
			}
			base = append(base, "-name", t)
		}
	default:
		base = []string{"!", "-name", ".*"}
	}
	hidden := []string{"-name", ".*", "-o", "-name", "node_modules"}
	switch {
	case rule == nil:
		return base, hidden
	case rule.negate:
		prune = wrap([]string{"-regex", rule.re, "-o"}, hidden)
	default:
		prune = wrap([]string{"!", "-regex", rule.re}, hidden)
	}
	switch {
	case rule.dirOnly:
		sel = base
	case rule.negate:
		sel = wrap([]string{"!", "-regex", rule.re}, base)
	default:
		sel = []string{"-regex", rule.re}
	}
	return sel, prune
}

// wrap appends a parenthesized find expression to a test that leads into it.
func wrap(lead, expr []string) []string {
	out := append(append([]string(nil), lead...), "(")
	out = append(out, expr...)
	return append(out, ")")
}
