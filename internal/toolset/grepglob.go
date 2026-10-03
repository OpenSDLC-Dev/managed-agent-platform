package toolset

import (
	"errors"
	"fmt"
	"strings"
)

// globRule is one glob of grep's glob property, compiled for find: re is a
// POSIX ERE over the whole path find prints for a file or directory.
type globRule struct {
	re string
	// negate is a leading "!": a path it matches is excluded.
	negate bool
	// dirOnly is a trailing "/": the glob matches directories alone.
	dirOnly bool
}

// compileGlobs compiles grep's glob property into rules for find, which
// searches under root. The property maps to rg --glob, whose globs are
// gitignore lines: one with no slash but a trailing one matches a name at any
// depth, and any other is anchored — here at the search root, since that is
// where find starts, where rg anchors at its own working directory
// (docs/DIVERGENCES.md). The value may hold several globs, split at whitespace
// and at commas outside braces, so "*.ts,*.tsx" means what a model sending it
// means rather than matching nothing.
func compileGlobs(root, value string) ([]globRule, error) {
	var rules []globRule
	prefix := ereQuote(strings.TrimSuffix(root, "/")) + "/"
	for _, g := range splitGlobs(value) {
		r := globRule{}
		if rest, ok := strings.CutPrefix(g, "!"); ok {
			r.negate, g = true, rest
		}
		if rest, ok := strings.CutSuffix(g, "/"); ok {
			r.dirOnly, g = true, rest
		}
		anchored := strings.Contains(g, "/")
		g = strings.TrimPrefix(g, "/")
		if g == "" {
			return nil, fmt.Errorf("glob %q matches no path", value)
		}
		body, err := globERE(g)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", g, err)
		}
		if !anchored {
			body = "(.*/)?" + body
		}
		r.re = "^" + prefix + body + "$"
		rules = append(rules, r)
	}
	return rules, nil
}

// splitGlobs splits the glob property into its globs: at whitespace, and at
// commas that are not inside a {…} alternation.
func splitGlobs(value string) []string {
	var out []string
	for _, field := range strings.Fields(value) {
		depth, start := 0, 0
		for i, c := range field {
			switch {
			case c == '{':
				depth++
			case c == '}' && depth > 0:
				depth--
			case c == ',' && depth == 0:
				if i > start {
					out = append(out, field[start:i])
				}
				start = i + 1
			}
		}
		if start < len(field) {
			out = append(out, field[start:])
		}
	}
	return out
}

// globERE translates one glob into an ERE with globset's literal-separator
// meaning: * and ? stop at a slash, ** spans directories where it is a whole
// path segment (a ** inside a segment is a *), [...] is a class that never
// matches a slash, {a,b} is an alternation, and \ quotes the next character.
func globERE(g string) (string, error) {
	var b strings.Builder
	rs := []rune(g)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '\\':
			if i+1 == len(rs) {
				return "", errors.New("ends in a lone backslash")
			}
			i++
			b.WriteString(ereQuote(string(rs[i])))
		case '?':
			b.WriteString("[^/]")
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				atStart := i == 0 || rs[i-1] == '/'
				i++
				switch {
				case atStart && i+1 == len(rs):
					b.WriteString(".*")
					continue
				case atStart && rs[i+1] == '/':
					i++
					// A run of **/ is one: each would add a group for the
					// regex engine to try at every directory.
					for i+3 < len(rs) && rs[i+1] == '*' && rs[i+2] == '*' && rs[i+3] == '/' {
						i += 3
					}
					b.WriteString("(.*/)?")
					continue
				}
			}
			b.WriteString("[^/]*")
		case '[':
			end := i + 1
			if end < len(rs) && (rs[end] == '!' || rs[end] == '^') {
				end++
			}
			if end < len(rs) && rs[end] == ']' {
				end++
			}
			for end < len(rs) && rs[end] != ']' {
				end++
			}
			if end == len(rs) {
				return "", errors.New("has an unclosed [ class")
			}
			// A negated class gains the slash, which no class may match; it
			// goes after a leading ], which ERE reads as a member only there.
			class := string(rs[i+1 : end])
			if rest, neg := cutAny(class, "!", "^"); neg {
				lead := ""
				if r, ok := strings.CutPrefix(rest, "]"); ok {
					lead, rest = "]", r
				}
				class = "^" + lead + "/" + rest
			}
			b.WriteString("[" + class + "]")
			i = end
		case '{':
			end := i + 1
			for end < len(rs) && rs[end] != '}' {
				if rs[end] == '{' {
					return "", errors.New("nests one {…} alternation in another")
				}
				end++
			}
			if end == len(rs) {
				return "", errors.New("has an unclosed { alternation")
			}
			var alts []string
			for _, a := range strings.Split(string(rs[i+1:end]), ",") {
				re, err := globERE(a)
				if err != nil {
					return "", err
				}
				alts = append(alts, re)
			}
			b.WriteString("(" + strings.Join(alts, "|") + ")")
			i = end
		default:
			b.WriteString(ereQuote(string(c)))
		}
	}
	return b.String(), nil
}

// cutAny is strings.CutPrefix for the first of prefixes s starts with.
func cutAny(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return s, false
}

// ereQuote quotes s for a POSIX ERE, every character literal.
func ereQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`\.[]()*+?{}|^$`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// selection builds find's file test and directory-prune test from a type's
// globs and the glob rules, with rg's precedence: the last glob a path matches
// decides it; a file no glob matches is excluded when any glob is a positive
// one, and otherwise falls to the type, which matches its base name; and a
// directory is pruned only by a negated glob that is the last to match it.
// Each test is built outward from the first rule, so the last rule is the one
// find evaluates first. prune comes back ready to append to an -o chain, or
// empty.
func selection(types []string, rules []globRule) (sel, prune []string) {
	sel = []string{"-true"}
	for _, r := range rules {
		if !r.negate {
			sel = []string{"-false"}
			break
		}
	}
	if len(types) > 0 && sel[0] == "-true" {
		sel = nil
		for i, t := range types {
			if i > 0 {
				sel = append(sel, "-o")
			}
			sel = append(sel, "-name", t)
		}
	}
	dirs := []string{"-false"}
	for _, r := range rules {
		if r.negate {
			dirs = wrap([]string{"-regex", r.re, "-o"}, dirs)
		} else {
			dirs = wrap([]string{"!", "-regex", r.re}, dirs)
		}
		if r.dirOnly {
			continue
		}
		if r.negate {
			sel = wrap([]string{"!", "-regex", r.re}, sel)
		} else {
			sel = wrap([]string{"-regex", r.re, "-o"}, sel)
		}
	}
	if len(rules) > 0 {
		prune = wrap([]string{"-o"}, dirs)
	}
	return sel, prune
}

// wrap appends a parenthesized find expression to a test that leads into it.
func wrap(lead, expr []string) []string {
	out := append(lead, "(")
	out = append(out, expr...)
	return append(out, ")")
}
