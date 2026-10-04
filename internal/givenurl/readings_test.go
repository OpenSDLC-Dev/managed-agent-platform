package givenurl

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// matcherAnswer is what the matcher, which read every payload at every lookup
// before #836, answers for the request raw over text: the given URL it fetches,
// or "". ok is false when the matcher spent its budget, where its answer is a
// refusal the index does not reproduce.
func matcherAnswer(text, raw string) (found string, ok bool) {
	m, valid := newURLMatcher(raw)
	if !valid {
		return "", true
	}
	m.scan(text)
	return m.found, m.found != "" || m.budget > 0
}

// indexAnswer is what an index holding rs answers for raw, as Source decides
// it: the reading spelled as raw when there is one, else the first.
func indexAnswer(rs []reading, raw string) string {
	m, valid := newURLMatcher(raw)
	if !valid {
		return ""
	}
	first := ""
	for _, r := range rs {
		if r.want != m.want {
			continue
		}
		if r.spelling == m.raw {
			return r.spelling
		}
		if first == "" {
			first = r.spelling
		}
	}
	return first
}

// requestsFor is the requests a differential check of text asks: every
// substring that starts at an occurrence, ended at every byte (inside a
// character too) — so any reading the matcher accepts is asked for as itself —
// every reading the index holds and its normalized form, and each of those
// spelled the ways a model might respell one: the scheme and host in capitals,
// a fragment appended, a byte dropped.
func requestsFor(text string, rs []reading) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	vary := func(r string) {
		add(r)
		add(r + "#x")
		add(strings.ToUpper(r[:min(len(r), 12)]) + r[min(len(r), 12):])
		if len(r) > 8 {
			add(r[:len(r)-1])
		}
		if w, ok := normalizeFetchURL(r); ok {
			add(w)
		}
	}
	for i := 0; i < len(text); i++ {
		if hasPrefixFold(text[i:], "http://") || hasPrefixFold(text[i:], "https://") {
			for j := i + len("http://"); j <= len(text) && j <= i+600; j++ {
				add(text[i:j])
			}
		}
	}
	for _, r := range rs {
		vary(r.spelling)
		vary(r.want)
	}
	return out
}

// checkText fails t when the index of text answers any request differently
// from the matcher.
func checkText(t *testing.T, text string) {
	t.Helper()
	rs, ok := readings(text)
	if !ok {
		t.Fatalf("readings(%q) spent the index budget", text)
	}
	for _, r := range rs {
		if want, ok := wantOf(r.spelling, r.plain); !ok || want != r.want {
			t.Errorf("text %q: reading %q is indexed under %q, and its spelling gives %q (%v)", text, r.spelling, r.want, want, ok)
			return
		}
	}
	for _, raw := range requestsFor(text, rs) {
		want, ok := matcherAnswer(text, raw)
		if !ok {
			continue
		}
		if got := indexAnswer(rs, raw); got != want {
			t.Errorf("text %q, request %q: index answers %q, matcher %q", text, raw, got, want)
			return
		}
	}
}

// The texts the matcher's own tests read (TestTextHoldsEveryReadingOfAURL),
// and texts built to reach each branch of its reading: a trimmed authority,
// one ended by a non-ASCII or a non-host byte, a run cut by its window, a
// window ending inside a character, a host IDNA rewrites.
var readingTexts = []string{
	"read https://example.com/page.",
	"(see https://example.com/a), or HTTP://Example.com/b!",
	"[next](https://example.com/next)[prev](https://example.com/prev)",
	"https://en.wikipedia.org/wiki/Go_(programming_language) is it",
	"[w](https://en.wikipedia.org/wiki/Go_(programming_language))",
	`"https://example.com/q?a=1&b=2"`,
	"'https://example.com/quoted'",
	"<https://example.com/x>",
	"**https://example.com/bold**",
	"https://r.jina.ai/https://example.com/",
	"İstanbul https://example.com/after-a-wide-rune",
	"https://example.com/search?",
	"https://example.com/release!",
	"https://example.com/a)",
	"看 https://example.com/cjk。",
	"https://example.com/wide　次",
	"请看https://example.com/docs，然后总结",
	"看https://example.com/docs然后总结",
	"看https://example.com然后总结",
	"https://example.com/日本語",
	"見てhttps://example.com/a。お願いします",
	"https://example.com/a—see",
	"https://example.com/a's",
	"See https://example.com's docs, (https://example.org), https://example.net.",
	"https://example.com/a?" + strings.Repeat("b", 300),
	"https://example.com",
	"https://example.com?q=1",
	"see https://example.com/release!.",
	"(https://example.com/a))",
	"https://fonts.googleapis.com/css?family=Roboto|Open+Sans",
	"no url here, only httpx://nope and http:/nope",
	"Read https://docs.example.com/guide, https://x.example/a%5C..%5Cadmin, https://bücher.de/x and https://Ünicode.example/page",
	"And https://ｅxample.org/path.",
	"See https://colon.example: it is. (https://paren.example:)",
	"Read HTTPS://Example.COM/Docs/Page#install. Then (https://other.example).",
	"http://[::1]:8080/x and http://[fe80::1%25en0]/y",
	"https://user:pass@example.com:8443/p?q#f",
	"https://example.com.:/a. https://example.com:..:/ https://example.com:80.",
	"https://ex­ample.com/soft https://exa­­­mple.com",
	"https://WİKİPEDİA.org/x https://BÜCHER.de/x",
	"https://example.com/%zz/ok https://example.com/a%2Fb/é",
	"https://example.com/a\x7fb https://example.com/\\path",
	"http://a.b/#" + strings.Repeat("x", 129) + "中文",
	"http://a.b/#" + strings.Repeat("x", 130) + "中文",
	"http://a.b/?q#" + strings.Repeat("x", 135) + "日本 tail",
	"http://" + strings.Repeat("a", 140) + ".com/p" + "中　",
	"http://" + strings.Repeat("a", 150) + "é/x" + strings.Repeat("y", 30) + " z",
	"http://h/#" + strings.Repeat("f", 131) + "　after",
	"http://h/#" + strings.Repeat("f", 132) + "　after",
	"http://h" + strings.Repeat("é", 70) + "/a",
	"https://example.com/a,https://example.com/b,https://example.com/c.",
	"HtTpS://ExAmPlE.CoM/MiXeD?Q=1#F",
	"https://x.y/a?b=日本&c=d。",
}

// windowEdgeTexts walk a window's edge across the characters it can land
// in. A want's windows are three times its length plus slack, so only a
// reading far longer than its want — a long fragment, which the want drops,
// or a host spelled with characters IDNA deletes — has a window that ends
// inside its own text: inside a CJK character, inside the ideographic space
// that ends the run, or short of the authority's end.
func windowEdgeTexts() []string {
	var out []string
	for n := 118; n <= 142; n++ {
		out = append(out,
			"http://a.b/#"+strings.Repeat("x", n)+"中文",
			"http://h/#"+strings.Repeat("f", n)+"　after",
			"http://h/?q#"+strings.Repeat("f", n)+"é ",
		)
	}
	for n := 20; n <= 90; n += 7 {
		out = append(out,
			"https://ex"+strings.Repeat("­", n)+"ample.com/path.",
			"https://ex"+strings.Repeat("­", n)+"ample.com:8080?q",
			"http://a"+strings.Repeat("­", n)+".com　x",
		)
	}
	return out
}

func TestReadingsAreWhatTheMatcherAccepts(t *testing.T) {
	for _, text := range append(readingTexts, windowEdgeTexts()...) {
		checkText(t, text)
	}
}

// fuzzTokens are what generated texts are made of: URL starts in any case,
// hosts in every alphabet the matcher folds, the punctuation that ends or
// does not end a URL in text, escapes good and bad, and the long authorities
// and fragments that make a window cut a run inside a character.
var fuzzTokens = []string{
	"http://", "https://", "HTTP://", "hTTps://", "example.com", "Example.COM", "a.b", "xn--bcher-kva.de",
	"bücher.de", "ｅxample.org", "[::1]", ":8080", ":", ".", "..", "/", "/a", "/path", "?", "?q=1", "&",
	"#", "#frag", "(", ")", "[", "]", "'", "\"", "<", ">", " ", " ", "　", "。", "，", "日本",
	"é", "%41", "%zz", "%E6%97%A5", "user@", "u:p@", "\\", "|", "!", "*", "~", "-", "_", ",", ";", "\t",
	"­", "İ", "\x7f", " ", "`",
	strings.Repeat("a", 130), "#" + strings.Repeat("x", 128), strings.Repeat("中", 5),
}

func TestReadingsAreWhatTheMatcherAcceptsOnGeneratedText(t *testing.T) {
	rng := rand.New(rand.NewPCG(836, 823))
	n := 1500
	if testing.Short() {
		n = 200
	}
	for range n {
		var b strings.Builder
		for range 1 + rng.IntN(24) {
			b.WriteString(fuzzTokens[rng.IntN(len(fuzzTokens))])
		}
		checkText(t, b.String())
		if t.Failed() {
			return
		}
	}
}

// FuzzReadingsAreWhatTheMatcherAccepts runs the same check on whatever the
// fuzzer builds; plain `go test` runs its seeds.
func FuzzReadingsAreWhatTheMatcherAccepts(f *testing.F) {
	for _, text := range readingTexts {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		// A payload's strings are JSON's: valid UTF-8. The length bounds the
		// check's own cost: it asks the matcher every substring.
		if !utf8.ValidString(text) || len(text) > 512 {
			return
		}
		checkText(t, text)
	})
}
