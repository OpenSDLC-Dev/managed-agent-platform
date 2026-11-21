package shell

import (
	"bytes"
	"math/rand/v2"
	"os/exec"
	"strings"
	"testing"
)

// unquotePosixSingle parses the single-quoted + '\” concatenation emitted by
// shellSingleQuote back to its raw bytes. Any syntax deviation fails the check.
func unquotePosixSingle(q string) (string, bool) {
	if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(q); {
		if q[i] != '\'' {
			return "", false
		}
		i++ // opening '
		for i < len(q) && q[i] != '\'' {
			b.WriteByte(q[i])
			i++
		}
		if i >= len(q) {
			return "", false
		}
		i++ // closing '
		if i < len(q) {
			if !strings.HasPrefix(q[i:], `\'`) {
				return "", false
			}
			b.WriteByte('\'')
			i += 2
		}
	}
	return b.String(), true
}

func FuzzShellSingleQuote(f *testing.F) {
	for _, seed := range []string{
		"",
		"'",
		"'''",
		"hello world",
		`$(id) && rm -rf /`,
		"`uname -a`",
		"/workspace/it's a very long path with 'many' 'single' quotes/file.txt",
		"line1\nline2\t\\$HOME\"double\"",
		"中文/路径/'引号'/file",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		q := shellSingleQuote(in)
		got, ok := unquotePosixSingle(q)
		if !ok {
			t.Fatalf("shellSingleQuote(%q) = %q is not valid POSIX single-quote syntax", in, q)
		}
		if got != in {
			t.Fatalf("round-trip mismatch: got %q, want %q (quoted=%q)", got, in, q)
		}
	})
}

// TestShellSingleQuotePosixShRoundTrip drives a real /bin/sh over generated
// adversarial strings (quotes, expansions, command substitutions, newlines,
// UTF-8, and long quote-heavy paths) to verify byte-for-byte round-trip safety.
func TestShellSingleQuotePosixShRoundTrip(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh not found: %v", err)
	}
	alphabet := []string{
		"a", "Z", "0", "/", ".", "-", "_", " ", "\t", "\n",
		"'", `"`, `\`, "$", "`", ";", "&", "|", "(", ")", "<", ">", "*", "?", "!", "#",
		"$(echo PWNED)", "$HOME", "'''", "中文", "é",
	}
	rng := rand.New(rand.NewPCG(75, 3))
	cases := []string{
		"",
		"'",
		strings.Repeat("'", 200),
		"/workspace/" + strings.Repeat("o'reilly/", 30) + "target's file.txt",
		"$(touch /tmp/should-not-run) `id` $PATH \"$USER\" 'a'\\''b'",
	}
	for range 80 {
		n := rng.IntN(40) + 1
		var sb strings.Builder
		for range n {
			sb.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		cases = append(cases, sb.String())
	}
	for i, tc := range cases {
		cmd := exec.Command(shPath, "-c", "printf '%s' "+shellSingleQuote(tc))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("case %d (%q): sh failed: %v", i, tc, err)
		}
		if !bytes.Equal(out, []byte(tc)) {
			t.Fatalf("case %d: sh printed %q, want %q", i, out, tc)
		}
	}
}
