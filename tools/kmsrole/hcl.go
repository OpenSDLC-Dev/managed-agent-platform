package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The .tf reader. It is a deliberate near-mirror of deploy/gcp/check_split.py's
// scrub()/blocks(), down to which constructs it refuses: that guard's header
// argues each refusal against a wrong answer review actually produced, and the
// two cannot share code across languages.
//
// It is stricter than the Python in one place, deliberately. The Python matches a
// heredoc terminator on the trimmed line, so an INDENTED terminator closes a
// plain `<<EOT` for it while Terraform reads on — and everything between is
// configuration to the reader and string content to Terraform, which is how a
// grant that does not exist gets read as one. This reader requires the exact
// terminator for `<<EOT` and allows indentation only for `<<-EOT`, as Terraform
// does. The same hole in check_split.py is #758.
//
// Structure is read from the scrubbed copy of a line — where a `{` inside a
// display_name cannot shift the brace depth and a `<<EOF` inside a string cannot
// open a heredoc — and attribute VALUES from the raw one, because scrubbing
// rewrites the characters an interpolated value is made of.

var (
	tfResourceRe = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"`)
	tfModuleRe   = regexp.MustCompile(`^\s*module\s+"([^"]+)"`)
	tfHeredocRe  = regexp.MustCompile(`<<([-~]?)([A-Za-z_][A-Za-z0-9_]*)`)
	// A nested block opener, with or without labels. Matched structurally rather
	// than by brace arithmetic, because a block written on one line nets to zero
	// braces and would go unseen.
	tfNestedRe = regexp.MustCompile(`^\s*([a-z_]+)(?:\s+"[^"]*")*\s*\{`)
	// The attribute keys this guard reads, compiled once rather than per lookup.
	tfAttrRe = map[string]*regexp.Regexp{}
)

func init() {
	for _, k := range []string{"crypto_key_id", "member", "role", "count", "for_each"} {
		tfAttrRe[k] = regexp.MustCompile(`^\s*` + k + `\s*=`)
	}
}

// tfLine is one line of a block body. Heredoc marks a line inside a heredoc
// body, which contributes neither structure nor value: prose in a variable
// description routinely contains both braces and text that looks like an
// assignment. The flag rides on the line rather than in a parallel slice, so no
// index alignment has to stay true for the reader to be right.
type tfLine struct {
	Raw      string
	Scrubbed string
	N        int // 1-based, in the file
	Heredoc  bool
}

// tfBlock is one top-level `resource` or `module` block. Modules are read
// because they are how configuration — and a grant — escapes the files this
// guard looked at.
type tfBlock struct {
	Type  string // "resource" or "module"
	Kind  string // the resource type; empty for a module
	Label string
	File  string
	N     int
	Body  []tfLine
}

// Addr is how a failure names the block: the Terraform address plus the file and
// line, so a red run can be walked to without a search.
func (b tfBlock) Addr() string {
	if b.Type == "module" {
		return fmt.Sprintf("module.%s (%s:%d)", b.Label, b.File, b.N)
	}
	return fmt.Sprintf("%s.%s (%s:%d)", b.Kind, b.Label, b.File, b.N)
}

// attr returns the single body line assigning key, and whether key is assigned
// at all. A second assignment is an error rather than a last-one-wins read: the
// two would disagree and this guard would report on whichever it happened to
// keep.
func (b tfBlock) attr(key string) (tfLine, bool, error) {
	re, ok := tfAttrRe[key]
	if !ok {
		return tfLine{}, false, fmt.Errorf("internal: no pattern for attribute %q", key)
	}
	var found []tfLine
	for _, l := range b.Body {
		if re.MatchString(l.Scrubbed) {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 0:
		return tfLine{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return tfLine{}, false, fmt.Errorf("%s assigns %s %d times", b.Addr(), key, len(found))
	}
}

// nested returns the name of every nested block in the body, in order. ALL of
// them: returning only the first let an allowed `lifecycle` shadow a `condition`
// written after it, and a conditional grant credited unconditionally is the one
// thing this whole function exists to prevent.
func (b tfBlock) nested() []string {
	var out []string
	for _, l := range b.Body {
		if m := tfNestedRe.FindStringSubmatch(l.Scrubbed); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func braces(s string) int {
	return strings.Count(s, "{") - strings.Count(s, "}")
}

// neutral strips a character's ability to affect structure while keeping the
// line's text readable.
func neutral(ch byte) byte {
	if strings.IndexByte("{}#<", ch) >= 0 {
		return '_'
	}
	return ch
}

// scrubTF drops comments and neutralizes the structural characters inside
// strings. String TEXT survives — the resource header is read from it.
func scrubTF(line string) (string, error) {
	var out strings.Builder
	quoted, interp := false, 0
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quoted {
			switch {
			case ch == '\\':
				// The escape and the character it escapes both lose their
				// structural meaning; nothing downstream reads the columns.
				out.WriteByte('_')
				i++
			case strings.HasPrefix(line[i:], "${"), strings.HasPrefix(line[i:], "%{"):
				// An interpolation or a template directive. BOTH, because they
				// are the same hazard: their own braces are not structure, but
				// their contents are HCL again. The leading character is kept as
				// the only surviving marker that a value was computed rather
				// than literal.
				interp++
				out.WriteByte(ch)
				out.WriteByte('_')
				i++
			case interp > 0:
				switch ch {
				case '{':
					interp++
				case '}':
					interp--
				case '"':
					// Reading this needs real HCL lexing: from the outside the
					// quote parity is shifted, so a later `{` and a later `}`
					// both fall out of the string and the depth desyncs around
					// an unread resource while still balancing at EOF.
					return "", errors.New(`a quoted string inside a ${...} interpolation or %{...} directive cannot be read by this guard — assign it to a "locals" value and interpolate that instead`)
				}
				out.WriteByte(neutral(ch))
			case ch == '"':
				quoted = false
				out.WriteByte('"')
			default:
				out.WriteByte(neutral(ch))
			}
			continue
		}
		switch {
		case ch == '"':
			quoted = true
			out.WriteByte('"')
		case ch == '#', strings.HasPrefix(line[i:], "//"):
			return out.String(), nil
		case strings.HasPrefix(line[i:], "/*"):
			// Not tracked across lines; opening one is enough of an oddity in
			// this tree to refuse rather than guess.
			return "", errors.New("/* */ block comments are not supported here — use # so this guard can read the file")
		default:
			out.WriteByte(ch)
		}
	}
	if quoted {
		// Quote state is tracked per line, so the closing `}"` on a later line
		// would count as structure and desync the brace depth for the rest of
		// the file.
		return "", errors.New(`a string is still open at end of line — write multi-line interpolations as a single-line "locals" value so this guard can read the file`)
	}
	return out.String(), nil
}

// tfBlocks yields every top-level `resource` and `module` block in path.
//
// Every way the reader can lose its place is an error rather than a quiet short
// read: an unterminated heredoc, unbalanced braces, a string still open at end
// of line. The only unacceptable outcome is reporting on configuration it did
// not look at.
func tfBlocks(path string) ([]tfBlock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Split on "\n" alone: strings.Split does not also break on U+2028, U+2029,
	// \v, \f or \x85, which HCL treats as ordinary characters inside a string.
	raw := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")

	src := make([]tfLine, 0, len(raw))
	term, indented := "", false
	for i, line := range raw {
		if term != "" {
			src = append(src, tfLine{Raw: line, N: i + 1, Heredoc: true})
			// `<<-EOT` and `<<~EOT` strip leading whitespace, so their
			// terminator may be indented. A plain `<<EOT` ends only at a line
			// that IS the terminator — matching an indented one there would end
			// the string early for this reader while Terraform read on, leaving
			// everything between as configuration to one and text to the other.
			// The comparison is exact rather than right-trimmed for the same
			// reason: a terminator this reader does not recognise makes the
			// heredoc unterminated, which is a refusal, and `make gcp-fmt`
			// already keeps trailing whitespace out of the tree.
			if (indented && strings.TrimSpace(line) == term) || line == term {
				term = ""
			}
			continue
		}
		s, err := scrubTF(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		if m := tfHeredocRe.FindStringSubmatchIndex(s); m != nil {
			indented = m[3] > m[2] // a `-` or `~` between `<<` and the word
			term = s[m[4]:m[5]]
			s = s[:m[0]]
		}
		src = append(src, tfLine{Raw: line, Scrubbed: s, N: i + 1})
	}
	if term != "" {
		return nil, fmt.Errorf("%s: heredoc <<%s is never terminated, so the rest of the file went unread — refusing to report on a partial scan", path, term)
	}

	// Headers are matched only at brace depth 0, and with leading whitespace
	// allowed: a check that only works once `terraform fmt` has run is a check
	// that can be bypassed by running the two in the wrong order.
	var out []tfBlock
	depth, i := 0, 0
	for i < len(src) {
		var blk tfBlock
		if depth == 0 {
			if m := tfResourceRe.FindStringSubmatch(src[i].Scrubbed); m != nil {
				blk = tfBlock{Type: "resource", Kind: m[1], Label: m[2], File: path, N: src[i].N}
			} else if m := tfModuleRe.FindStringSubmatch(src[i].Scrubbed); m != nil {
				blk = tfBlock{Type: "module", Label: m[1], File: path, N: src[i].N}
			}
		}
		if blk.Type == "" {
			depth += braces(src[i].Scrubbed)
			i++
			continue
		}
		d := braces(src[i].Scrubbed)
		for i++; i < len(src) && d > 0; i++ {
			d += braces(src[i].Scrubbed)
			if d > 0 && !src[i].Heredoc {
				blk.Body = append(blk.Body, src[i])
			}
		}
		if d > 0 {
			return nil, fmt.Errorf("%s: unbalanced braces — the file ended inside %s, so the reader lost its place and anything after it went unread", path, blk.Addr())
		}
		out = append(out, blk)
	}
	if depth != 0 {
		return nil, fmt.Errorf("%s: unbalanced braces at end of file (depth %d) — the reader lost its place, so part of the file went unread", path, depth)
	}
	return out, nil
}
