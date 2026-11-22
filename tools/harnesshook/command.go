package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

type commit struct {
	dir     string
	trailer bool
}

type shellSegment struct {
	argv         []string
	scope        byte
	stdinTrailer bool
}

// A quoted delimiter makes a here-document literal. Recognize a single
// trailing input redirection on the command line, so its message stays bound
// to the command receiving stdin. Other forms cannot certify a trailer.
var quotedHeredoc = regexp.MustCompile(`^<<(-?)[ \t]*(?:'([A-Za-z_][A-Za-z0-9_.-]*)'|"([A-Za-z_][A-Za-z0-9_.-]*)")[ \t]*\r?\n`)

func literalInput(s string) (int, bool) {
	header := quotedHeredoc.FindStringSubmatch(s)
	if header == nil {
		return 0, false
	}
	delimiter := header[2] + header[3]
	verified := false
	for offset := len(header[0]); offset < len(s); {
		line, tail, newline := strings.Cut(s[offset:], "\n")
		end := len(s) - len(tail)
		line = strings.TrimSuffix(line, "\r")
		if header[1] == "-" {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delimiter {
			return end, verified
		}
		verified = verified || trailerValue.MatchString(line)
		if !newline {
			break
		}
		offset = end
	}
	return 0, false
}

// Keep subshell boundaries and quoted newlines without executing shell input.
func shellSegments(command string) []shellSegment {
	var segments []shellSegment
	var argv []string
	var word strings.Builder
	var quote byte
	haveWord := false
	stdinTrailer := false
	finishWord := func() {
		if haveWord {
			argv = append(argv, word.String())
			word.Reset()
			haveWord = false
		}
	}
	finishSegment := func() {
		finishWord()
		if len(argv) > 0 {
			segments = append(segments, shellSegment{argv: argv, stdinTrailer: stdinTrailer})
			argv = nil
		}
		stdinTrailer = false
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if quote == 0 {
			switch ch {
			case '<':
				// Only the first '<' in a redirection can introduce a here-document.
				if i != 0 && command[i-1] == '<' {
					break
				}
				if consumed, verified := literalInput(command[i:]); consumed != 0 {
					// A decimal immediately before << can redirect another descriptor.
					// Credit only an unambiguous stdin binding, implicit or explicit.
					explicitStdin := haveWord && word.String() == "0"
					stdinTrailer = verified && (!haveWord || explicitStdin)
					if explicitStdin {
						word.Reset()
						haveWord = false
					}
					finishSegment()
					i += consumed - 1
					continue
				}
			case ' ', '\t', '\r':
				finishWord()
				continue
			case '(', ')':
				finishSegment()
				segments = append(segments, shellSegment{scope: ch})
				continue
			case ';', '|', '&', '\n':
				finishSegment()
				continue
			case '#':
				if !haveWord {
					for i < len(command) && command[i] != '\n' {
						i++
					}
					finishSegment()
					continue
				}
			}
		}
		if ch == '\'' || ch == '"' {
			if quote == 0 {
				quote = ch
				haveWord = true
				continue
			}
			if quote == ch {
				quote = 0
				continue
			}
		}
		if ch == '\\' && quote != '\'' && i+1 < len(command) && strings.ContainsRune(" \t'\"\\", rune(command[i+1])) {
			i++
			ch = command[i]
		}
		haveWord = true
		word.WriteByte(ch)
	}
	finishSegment()
	return segments
}

var messageTrailer = regexp.MustCompile("(?im)^Verified-By:[[:blank:]]*[^[:space:]]")

// A short option that takes an argument consumes the rest of the token.
// Only argument-free flags can precede it in a cluster (such as -am).
var shortValue = regexp.MustCompile(`(?s)^-[aiposeqnzvb]*([mFCct])(.*)$`)
var trailerValue = regexp.MustCompile("(?i)^Verified-By:[[:blank:]]*[^[:space:]]")
var assignment = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]*=`)

func hasTrailer(argv []string, stdinTrailer bool) bool {
	fromStdin := false
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			break // The remaining arguments are pathspecs.
		}
		flag, val, joined := strings.Cut(argv[i], "=")
		if short := shortValue.FindStringSubmatch(argv[i]); short != nil {
			flag, val = "-"+short[1], short[2]
			joined = val != ""
		}
		switch flag {
		case "-m", "--message", "-F", "--file", "--trailer", "-c", "-C", "-t",
			"--reuse-message", "--reedit-message", "--template", "--author", "--date",
			"--fixup", "--squash", "--pathspec-from-file":
			if !joined && i+1 < len(argv) {
				i++
				val = argv[i]
			}
		default:
			continue
		}
		switch flag {
		case "--trailer":
			if trailerValue.MatchString(val) {
				return true
			}
		case "-m", "--message":
			if messageTrailer.MatchString(val) {
				return true
			}
		case "-F", "--file":
			fromStdin = val == "-"
		}
	}
	return stdinTrailer && fromStdin
}

func commits(command, base string) []commit {
	var result []commit
	var parents []string
	for _, segment := range shellSegments(command) {
		switch segment.scope {
		case '(':
			parents = append(parents, base)
			continue
		case ')':
			if len(parents) > 0 {
				base = parents[len(parents)-1]
				parents = parents[:len(parents)-1]
			}
			continue
		}
		args := segment.argv
		for len(args) > 0 && assignment.MatchString(args[0]) {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(args[0], "\\", "/"))), ".exe")
		argv := args[1:]
		switch name {
		case "cd", "chdir", "set-location", "pushd", "push-location":
			if len(argv) > 0 {
				base = resolveDir(base, argv[len(argv)-1])
			} else {
				base = ""
			}
		case "popd", "pop-location":
			base = ""
		case "pwsh", "powershell", "bash", "sh", "zsh":
			for i, a := range argv {
				if (strings.EqualFold(a, "-command") || a == "-lc" || a == "-c") && i+1 < len(argv) {
					result = append(result, commits(strings.Join(argv[i+1:], " "), base)...)
					break
				}
			}
		case "git":
			dir := base
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				opt, val, joined := strings.Cut(argv[0], "=")
				argv = argv[1:]
				switch opt {
				case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env":
					if !joined && len(argv) > 0 {
						val = argv[0]
						argv = argv[1:]
					}
					if opt == "-C" {
						dir = resolveDir(dir, val)
					}
					if opt == "--git-dir" || opt == "--work-tree" {
						dir = ""
					}
				}
			}
			if len(argv) > 0 && argv[0] == "commit" {
				result = append(result, commit{dir, hasTrailer(argv[1:], segment.stdinTrailer)})
			}
		}
	}
	return result
}
