package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The .tf reader. It is a deliberate near-mirror of deploy/gcp/check_split.py's
// scrub()/blocks(), down to which constructs it refuses: that guard's header
// argues each refusal against a wrong answer review actually produced, and the
// two cannot share code across languages.
//
// Both end a heredoc at the first line that, TRIMMED, is the terminator — for
// `<<EOT` as much as `<<-EOT`, because the marker decides how the body is
// dedented, not where the string ends. check_split.py records the terraform
// 1.15.8 runs behind that. Trimming is where the two languages could drift and
// do not: over every file Terraform will parse, strings.TrimSpace is its
// whitespace set, and the four characters Python's wider str.strip() adds are
// excluded on that side rather than trimmed. A lone U+000D never reaches that
// comparison on either side: a file holding one is refused where its bytes are
// read, as Terraform refuses it. Requiring an exact match instead, as this reader
// did until #758, reads on past a terminator Terraform honoured: what follows
// is configuration to Terraform and string content to the reader, so a deny
// policy that should have refused the file is never seen, and the swallowed
// braces can balance again at EOF with nothing to report.
//
// Structure is read from the scrubbed copy of a line — where a `{` inside a
// display_name cannot shift the brace depth and a `<<EOF` inside a string cannot
// open a heredoc — and attribute VALUES from the raw one, because scrubbing
// rewrites the characters an interpolated value is made of.

var (
	tfResourceRe = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"`)
	tfModuleRe   = regexp.MustCompile(`^\s*module\s+"([^"]+)"`)
	// `~` is not a Terraform heredoc marker at all — `<<~EOT` is an `Invalid
	// expression`, so terraform reads the lines behind it as structure. It is
	// therefore NOT matched, here as in check_split.py: a reader that took them
	// for a heredoc body skipped a resource and balanced its braces again
	// (opener_tilde_marker.tf). Unmatched, it falls into the refusal below.
	// The tag is an HCL identifier, which allows a `-` INSIDE it — `<<EOT-X` is
	// terraform-clean, and a leading one cannot reach the class, so `<<-EOT`
	// stays the indent marker plus `EOT`. An identifier also allows Unicode
	// letters, which this class deliberately does not: tfBlocks refuses a `<<`
	// it cannot match in full rather than read the body as configuration.
	tfHeredocRe = regexp.MustCompile(`<<-?([A-Za-z_][A-Za-z0-9_-]*)`)
	// A nested block opener, with or without labels. Matched structurally rather
	// than by brace arithmetic, because a block written on one line nets to zero
	// braces and would go unseen.
	tfNestedRe = regexp.MustCompile(`^\s*([a-z_]+)(?:\s+"[^"]*")*\s*\{`)
	// The attribute keys this guard reads, compiled once rather than per lookup.
	tfAttrRe = map[string]*regexp.Regexp{}
)

// The two ways a heredoc opener can be one this reader must not read past.
// Named, as every refusal here is, so an edit to one of them cannot drift from
// check_split.py's wording unnoticed; the corpus matches a substring of each.
var errOpenerTrailer = errors.New("nothing may follow a heredoc opener on its line — terraform refuses the file over a trailing comment, and over a single trailing space; read on, the configuration behind the opener becomes string content and the braces still balance")

var errOpenerTag = errors.New("a `<<` this reader cannot read as a heredoc opener — Terraform's tag is any HCL identifier, Unicode letters included, and this reads only [A-Za-z_][A-Za-z0-9_-]*; unrecognised, the body is read as configuration and a `<<WORD` inside it becomes the opener")

// Raised where a U+FEFF past the file's leading byte-order mark survives
// scrubbing. Mirrors check_split.py's BAD_BOM.
var errBadBOM = errors.New("a U+FEFF that is not the file's leading byte-order mark, outside a comment — terraform refuses the file over one (Invalid character) and accepts it inside a string, a comment and a heredoc body, and here it glues to whatever follows, so a block header behind one matches nothing")

// Raised where a byte that is not UTF-8 survives scrubbing, or reaches a
// heredoc body line — which is to say where it sits outside a comment. Mirrors
// check_split.py's BAD_UTF8.
var errBadUTF8 = errors.New("a byte that is not UTF-8, outside a comment — terraform refuses the file over one (Invalid character encoding) and accepts it inside a comment, which runs to the newline, and here it glues to whatever follows, so a block header behind one matches nothing and this reader would report over a file it had not read")

// Raised from the two places a lone carriage return can reach this reader, so
// both say the same thing about the same character.
var errLoneCR = errors.New("a carriage return that is not part of a CRLF — terraform refuses the file over one (Invalid character, or Invalid multi-line string when it sits inside a quoted string), so this reader refuses rather than guessing where the lines end")

// The five characters that look like a line ending to something splitting text
// and are not one to Terraform. Splitting on "\n" does not break on them, and
// check_split.py's split does not either — deliberately, because a description
// holding one would otherwise be cut mid-string — so a file using one AS its
// separator arrives as a single line, where only the first header can match and
// every block behind it is invisible (#768). Measured on terraform 1.15.8:
// among structure each is an `Invalid character`; inside a string, a comment or
// a heredoc body each is ordinary text. So they are neutralised where terraform
// reads them and refused where it does not, which is the U+FEFF mechanism at
// different codepoints.
//
// That covers a `${...}` interpolation and a `%{...}` directive too, whose
// contents are HCL again rather than string text: scrubbing neutralises these
// only at interpolation depth 0, and `$${`/`%%{`, which are literal text, are
// consumed before they can be read as either. An inline `/* ... */` inside a
// template expression is the exception that proves the rule: HCL allows a
// comment there, terraform reads all five in one, and scrubTF blanks the whole
// span so this reader does too.
//
// Two places the parity still ends, both named because counting them wrong is
// how this sentence was written twice. A heredoc BODY line is never scrubbed,
// so a template inside one is not seen and a file terraform refuses is read
// (interp_in_heredoc_body_is_read.tf) — benign, since such a file cannot deploy
// and `make gcp-fmt` reddens on it first. And behind a backslash terraform
// answers `Invalid escape sequence` while scrubbing collapses the pair — but
// that is the general escape class and not these five (`"a\zb"` draws it
// too), and neither reader validates escapes. Mirrors check_split.py's
// FAKE_EOL.
const fakeEOL = "\v\f\u0085\u2028\u2029"

var errFakeEOL = errors.New("a line separator Terraform does not read as one (U+000B, U+000C, U+0085, U+2028 or U+2029), outside a string, a comment and a heredoc body — terraform refuses the file over one (Invalid character) and this reader does not break lines on it, so everything behind it would arrive on the line in front of it, where only the first header can match and the rest goes unread")

// fakeEOLAt returns the byte length of the not-a-line-ending character at the
// start of s, or 0. s must be non-empty: the only caller slices a line inside a
// loop bounded by its length. Byte-switched rather than read off fakeEOL
// because scrubTF walks a line one byte at a time and every other byte must
// cost one compare; TestFakeEOLAtMatchesTheConstant holds the two together.
func fakeEOLAt(s string) int {
	switch s[0] {
	case '\v', '\f':
		return 1
	case 0xc2:
		if strings.HasPrefix(s, "\u0085") {
			return 2
		}
	case 0xe2:
		if strings.HasPrefix(s, "\u2028") || strings.HasPrefix(s, "\u2029") {
			return 3
		}
	}
	return 0
}

func init() {
	// Either at the start of the line, or straight after a `{`. The second form
	// is what a one-line nested block looks like — `lifecycle { ignore_changes =
	// [role] }` — and a line-anchored pattern reads it as no assignment at all,
	// which is how an argument inside such a block goes unseen. This matches on
	// the SCRUBBED copy, where a brace inside a string has already been
	// neutralised, so the `{` here is always structure. It is the same hazard
	// nested() is matched structurally for: a one-line block nets to zero braces.
	for _, k := range []string{"crypto_key_id", "member", "role", "count", "for_each", "ignore_changes"} {
		tfAttrRe[k] = regexp.MustCompile(`(?:^|\{)\s*` + k + `\s*=`)
	}
}

// tfLine is one line of a block body. Heredoc marks a line inside a heredoc
// body, which contributes neither structure nor value: prose in a variable
// description routinely contains both braces and text that looks like an
// assignment. The flag rides on the line rather than in a parallel slice, so no
// index alignment has to stay true for the reader to be right.
type tfLine struct {
	Raw string
	// Code is Raw with any trailing comment removed and strings left intact.
	// Neither of the other two answers for it: Raw carries the comment, and
	// Scrubbed has rewritten the `${` and `}` an interpolated value is made of.
	// A reader matching a value across a whole line needs this one, or a quoted
	// example inside a comment reads as configuration.
	Code     string
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
//
// codeLen is how many bytes of line are code: the index the comment starts at,
// or the whole line when there is none. The comment boundary is only knowable
// from this quote-aware pass — `#` inside a string does not start one — so it is
// returned rather than recomputed by a caller that would get it wrong.
func scrubTF(line string) (string, int, error) {
	var out strings.Builder
	quoted, interp := false, 0
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quoted {
			if w := fakeEOLAt(line[i:]); interp == 0 && w > 0 {
				// Neutralised rather than refused, for the reason the BOM is:
				// terraform reads these inside string TEXT, and blanking them
				// here is what lets the refusal in tfBlocks mean exactly the
				// position terraform refuses. Handled ahead of the switch so
				// the width is computed once.
				//
				// Only at interp == 0. A `${...}` or `%{...}` is not string
				// text — its contents are HCL again, and terraform answers
				// `Invalid character` for these in there — so they are written
				// through for tfBlocks to refuse, as a carriage return and a
				// non-UTF-8 byte in that position already were.
				out.WriteByte('_')
				i += w - 1
				continue
			}
			switch {
			case ch == '\\':
				// The escape and the character it escapes both lose their
				// structural meaning; nothing downstream reads the columns. A
				// carriage return is the exception, kept so the lone-return
				// check in tfBlocks can still see it: `"a\<CR>b"` is three
				// errors to terraform 1.15.8, and collapsing the pair to `_`
				// hid it from every later look.
				// The whole rune is consumed rather than one byte: collapsing
				// `\` plus a lead byte left the continuation bytes behind and
				// manufactured invalid UTF-8 out of a file that had none, which
				// this reader would then have refused for the wrong reason
				// while its Python mirror refused nothing. A byte that is not
				// UTF-8 is NOT kept here — keeping it put it next to whatever
				// preceded the backslash and could splice the two into a legal
				// rune; tfBlocks checks the raw code for that instead, and says
				// why there.
				if i+1 < len(line) {
					_, w := utf8.DecodeRuneInString(line[i+1:])
					if line[i+1] == '\r' {
						out.WriteByte('\r')
					} else {
						out.WriteByte('_')
					}
					i += w
				} else {
					out.WriteByte('_')
				}
			case interp == 0 && strings.HasPrefix(line[i:], "\ufeff"):
				// Neutralized rather than refused: terraform accepts a BOM
				// inside a string, a comment and a heredoc body, and refuses it
				// among structure — so blanking it here is what lets the
				// refusal in tfBlocks mean exactly the position terraform
				// refuses. neutral() cannot do it: it works a byte at a time
				// and this character is three.
				out.WriteByte('_')
				i += len("\ufeff") - 1
			case interp > 0 && strings.HasPrefix(line[i:], "/*") &&
				strings.Contains(line[i+2:], "*/"):
				// An inline block comment INSIDE a template expression, where
				// HCL allows one — terraform 1.15.8 parses
				// `"p${ 1 /* c */ }q"`. The whole span is blanked, so nothing in it
				// is read as structure: not a brace, not a quote, and not one
				// of the characters refused in tfBlocks, which terraform reads
				// here as the comment text they are.
				//
				// Only when the `*/` is on this line. An unterminated one
				// leaves the template open past the end of the line, which
				// terraform answers with `Invalid expression` — refusing to
				// guess there is this reader's rule everywhere else and stays
				// its rule here.
				//
				// No state is carried across lines, and none is needed: a
				// template that spans lines leaves the string open at end of
				// line, which the check below refuses before a second line of
				// it is ever scrubbed. Terraform PARSES such a file —
				// `"p${ 1 +` then `1 }q"` is exit 3 on 1.15.8 — so that is a
				// false refusal, older than this case and untouched by it
				// (interp_open_at_eol.tf).
				w := strings.Index(line[i+2:], "*/") + len("/**/")
				out.WriteString(strings.Repeat("_", w))
				i += w - 1
			case strings.HasPrefix(line[i:], "$${"), strings.HasPrefix(line[i:], "%%{"):
				// An ESCAPED template: `$${` and `%%{` are literal text to
				// Terraform, which accepts a file holding one. All three bytes
				// are consumed, the brace blanked as the string text it is, so
				// the `${` this would otherwise see does not open a template
				// that is not there. Harmless while a template's contents were
				// neutralised like string text; a false refusal the moment they
				// stopped being (interp_escaped_is_text.tf).
				//
				// THREE bytes and not the `$$` pair: measured on terraform
				// 1.15.8, a run of N `$` before `{` opens a template only at
				// N == 1 — the escape binds to the two characters adjacent to
				// the brace, not to pairs from the left. Eating pairs matched
				// that at even N and opened a template on the leftover `$` at
				// odd N, refusing `$$${...}`, which terraform takes
				// (interp_escaped_odd_run_is_text.tf).
				out.WriteByte(ch)
				out.WriteByte(ch)
				out.WriteByte('_')
				i += 2
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
				if ch == '#' || strings.HasPrefix(line[i:], "//") {
					// A LINE comment inside an OPEN template. Terraform reads
					// it to the end of the line, which swallows the `}` closing
					// the template and the quote closing the string, and
					// answers `Invalid multi-line string`. This scrubber has no
					// such rule: it would close the string at that quote and
					// read on over a file terraform refuses. Two models
					// disagreeing silently about where a string ends is exactly
					// what #761 was, so the disagreement is refused rather than
					// kept.
					//
					// Only while the template is open. Once its `}` has closed,
					// a `#` is ordinary string text and terraform takes the
					// file (interp_hash_after_close_is_text.tf).
					return "", 0, errors.New(`a # or // comment inside a ${...} interpolation or %{...} directive runs past the brace that closes it and the quote that closes the string — Terraform refuses the file, so this guard does too`)
				}
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
					return "", 0, errors.New(`a quoted string inside a ${...} interpolation or %{...} directive cannot be read by this guard — assign it to a "locals" value and interpolate that instead`)
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
			return out.String(), i, nil
		case strings.HasPrefix(line[i:], "/*"):
			// Not tracked across lines; opening one is enough of an oddity in
			// this tree to refuse rather than guess.
			return "", 0, errors.New("/* */ block comments are not supported here — use # so this guard can read the file")
		default:
			out.WriteByte(ch)
		}
	}
	if quoted {
		// Quote state is tracked per line, so the closing `}"` on a later line
		// would count as structure and desync the brace depth for the rest of
		// the file.
		return "", 0, errors.New(`a string is still open at end of line — write multi-line interpolations as a single-line "locals" value so this guard can read the file`)
	}
	return out.String(), len(line), nil
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
	// A leading BOM is dropped rather than left in front of the first header,
	// which `^\s*` does not match — U+FEFF is whitespace to neither this regexp
	// nor Python's. terraform parses a BOM'd file (`fmt -check` reports only
	// formatting drift), so refusing it would reject configuration the binary
	// takes; dropping it is what lets the header be read (#765).
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	// Split on "\n" alone: strings.Split does not also break on U+2028, U+2029,
	// \v, \f or \x85, which HCL treats as ordinary characters inside a string.
	raw := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")

	src := make([]tfLine, 0, len(raw))
	term := ""
	for i, line := range raw {
		if term != "" {
			// A body line, where a lone return is refused outright: terraform
			// refuses the file over one, and a trailing `\r` before the
			// terminator word is padding to strings.TrimSpace and not to
			// terraform, so it would close the string HERE and not THERE.
			if strings.Contains(line, "\r") {
				return nil, fmt.Errorf("%s:%d: %w", path, i+1, errLoneCR)
			}
			// And the byte terraform refuses here too (`Invalid character
			// encoding`, with an `Unterminated template string` behind it). A
			// body line is never scrubbed, so the check further down never sees
			// it. U+FEFF is deliberately NOT checked: terraform reads one in a
			// body as ordinary text.
			if !utf8.ValidString(line) {
				return nil, fmt.Errorf("%s:%d: %w", path, i+1, errBadUTF8)
			}
			src = append(src, tfLine{Raw: line, N: i + 1, Heredoc: true})
			// Trimmed, whatever the opener's marker — see the header.
			if strings.TrimSpace(line) == term {
				// ...but HCL wants the newline after the terminator that the
				// file's last line never gets: terraform 1.15.8 answers
				// `Unterminated template string`, and accepts the same file the
				// moment a trailing newline is added. An ordinary last line
				// without one it accepts either way, so this is the
				// terminator's rule and not the file's. Split on "\n" leaves a
				// trailing "" when the content ends in one, so the last element
				// is a line the file never ended — and it can only be reached
				// here when it is non-empty, a terminator never being blank.
				if i == len(raw)-1 {
					return nil, fmt.Errorf("%s:%d: heredoc <<%s's terminator is the last line of a file with no trailing newline, so HCL does not close the string there — refusing rather than reading on", path, i+1, term)
				}
				term = ""
			}
			continue
		}
		s, codeLen, err := scrubTF(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		// Checked on the SCRUBBED line, because terraform's own answer depends
		// on where the return sits: measured on 1.15.8, `# note\r` at end of
		// file, `# a\rb` and `# note\r\r\n` are all accepted — a comment runs to
		// the newline and a lone return is ordinary text inside it — while the
		// same return among structure is an `Invalid character` and inside a
		// quoted string an `Invalid multi-line string`. Scrubbing removes the
		// comments, and keeps a return everywhere else including behind a
		// backslash, so what reaches here is the half terraform refuses —
		// inside a template expression as well as outside one, because scrubTF
		// blanks an inline `/* ... */` there too. The other two comment markers
		// need no such care: inside a single-line template `#` and `//` swallow
		// the closing brace and the quote, and terraform refuses the file.
		// It has to be a refusal rather than a translation because this reader
		// and check_split.py disagreed about such a file: Python's read_text()
		// broke lines on a bare \r and this one never has, so the same bytes
		// were four lines there and one line here — and one line means only the
		// first header can match, with the rest unread and nothing said (#761).
		if strings.Contains(s, "\r") {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, errLoneCR)
		}
		// The same position rule for the five characters that look like a line
		// ending and are not one. scrubTF has neutralised them inside strings
		// and dropped the comments, so one that survives is one terraform
		// refuses — and one this reader would read straight past, taking a
		// whole file for a single line (#768).
		if strings.ContainsAny(s, fakeEOL) {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, errFakeEOL)
		}
		// Same position rule for a byte that is not UTF-8, and the same reason.
		// terraform accepts one inside a comment (`# caf\xe9` is fmt- and
		// validate-clean on 1.15.8) and refuses it anywhere else as an `Invalid
		// character encoding`. Reading on is what this reader did until now, and
		// it is not harmless: `\xffresource "…"` is a single word to the header
		// regexp, so the block behind it is invisible and a scan that finds
		// nothing reports as if there were nothing.
		//
		// Checked on the RAW code — the line up to where its comment starts —
		// and not on the scrubbed copy, which cannot answer the question. This
		// scrubber works a byte at a time and DELETES the backslash of an
		// escape, so in `"a<C3>\<A9>b"` it would write 0xC3, then 0xA9, leaving
		// them adjacent: a well-formed `é` that the file never contained, over
		// which utf8.ValidString says yes. terraform gives that file three
		// errors and check_split.py refuses it — it decodes the whole file
		// first, so its two surrogates can never recombine. The raw code cannot
		// splice, which is what makes the two readers agree here.
		if !utf8.ValidString(line[:codeLen]) {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, errBadUTF8)
		}
		// And U+FEFF, which is valid UTF-8 and so invisible to the check above.
		// Only the file's first one was a byte-order mark; the strip above took
		// that, so anything left is a character terraform refuses among
		// structure, and one that glues to a header just as `\xff` does.
		if strings.Contains(s, "\ufeff") {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, errBadBOM)
		}
		code := line[:codeLen]
		m := tfHeredocRe.FindStringSubmatchIndex(s)
		// A `<<` this expression cannot match IN FULL is refused rather than
		// read past. Terraform's tag is an HCL identifier, so `<<Ö` opens a
		// heredoc for the binary and nothing here, and `<<EÖT` opens one whose
		// tag this truncates to `E`. Either way the body is then read as
		// configuration, a `<<WORD` inside it becomes this reader's opener, and
		// a terminator far below closes it with the braces balanced and a
		// resource swallowed — measured on terraform 1.15.8, and pinned by
		// opener_unicode_tag.tf.
		//
		// "In full" asks only whether the next byte is ASCII, not whether it is
		// an identifier character: what can still continue an HCL identifier
		// past this class is exactly a non-ASCII character, and "non-ASCII"
		// means the same thing in both languages where unicode.IsLetter and
		// str.isalpha() do not. It is read over bytes here and over codepoints
		// in check_split.py, which agree because the class matches only ASCII,
		// so whatever follows it starts at a byte boundary. A `<` inside a
		// string is neutralised by scrubbing, so a `<<` still here is structure.
		//
		// The refusal covers every `<<` this cannot read — a bare one, a
		// digit-initial tag, `<<<EOT` — and not only the Unicode tag that
		// motivated it, which is why errOpenerTag names the condition rather
		// than one cause.
		if at := strings.Index(s, "<<"); at >= 0 {
			if m == nil || m[0] != at || (m[1] < len(s) && s[m[1]] >= 0x80) {
				return nil, fmt.Errorf("%s:%d: %w", path, i+1, errOpenerTag)
			}
		}
		if m != nil {
			// Nothing may follow the opener on its line. Measured against
			// terraform 1.15.8. Every trailer named here has a corpus row, so
			// `make tf-corpus-check` re-asks the binary rather than trusting
			// this sentence: a `#` or `//` comment, a single space, a tab, a
			// `}`, a `,` inside a call and a second opener are each an
			// `Invalid expression`, while `<<-EOT` and an opener that ends the
			// line inside a call are fine. A trailing `/* */` is refused
			// further up, by the rule that this reader does not read block
			// comments at all.
			//
			// Two clauses, each read against its own string, because the
			// scrubbed line is NOT the raw line's length: an escape pair and a
			// not-a-line-ending character inside a string each collapse to one
			// byte. `m` indexes the scrubbed line, so the opener has to end THAT
			// one; and a comment — the only thing scrubbing takes off the tail,
			// a `/* */` being refused outright — shows as a code length short of
			// the raw line's. Neither `<<EOT# note`, where the scrubbed line
			// ends at the opener, nor `<<EOT<<EOT`, where the raw line does, is
			// caught by the other.
			//
			// Reading on instead turned the configuration behind the opener into
			// string content, with the braces still balancing and nothing to
			// report (#766).
			if m[1] != len(s) || codeLen != len(line) {
				return nil, fmt.Errorf("%s:%d: %w", path, i+1, errOpenerTrailer)
			}
			term = s[m[2]:m[3]]
			s = s[:m[0]]
		}
		src = append(src, tfLine{Raw: line, Code: code, Scrubbed: s, N: i + 1})
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
