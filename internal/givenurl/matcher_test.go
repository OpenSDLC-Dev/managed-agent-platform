package givenurl

import (
	"strings"
	"testing"
)

func TestNormalizeFetchURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/page", "https://example.com/page"},
		{"HTTPS://Example.COM/Page", "https://example.com/Page"}, // the path keeps its case
		{"https://example.com/page#intro", "https://example.com/page"},
		{"https://example.com", "https://example.com/"},
		{"https://example.com#top", "https://example.com/"},
		{"https://example.com/a?q=1&r=2", "https://example.com/a?q=1&r=2"},
		{"http://[::1]:8080/x", "http://[::1]:8080/x"},
		// The host takes egress.CanonicalHost's form: ASCII case folded and a
		// Unicode name as its A-label, so a look-alike "İ" host stays distinct
		// and a name and its A-label are one host.
		{"https://WİKİPEDİA.org/x", "https://xn--wikipedia-6jfce.org/x"},
		{"https://BÜCHER.de/x", "https://xn--bcher-kva.de/x"},
		{"https://xn--bcher-kva.de/x", "https://xn--bcher-kva.de/x"},
	} {
		if got, ok := normalizeFetchURL(tc.in); !ok || got != tc.want {
			t.Errorf("normalizeFetchURL(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "example.com/page", "file:///etc/passwd", "mailto:a@b.c", "https:///nohost", "%zz"} {
		if got, ok := normalizeFetchURL(bad); ok {
			t.Errorf("normalizeFetchURL(%q) = %q, want it refused", bad, got)
		}
	}
}

// textHolds reports whether text provides url, as the matcher decides it for
// one string.
func textHolds(text, url string) bool {
	m, ok := newURLMatcher(url)
	if !ok {
		return false
	}
	m.scan(text)
	return m.found != ""
}

func TestTextHoldsEveryReadingOfAURL(t *testing.T) {
	for _, tc := range []struct {
		text string
		urls []string // each one the text holds
	}{
		{"read https://example.com/page.", []string{"https://example.com/page", "https://example.com/page."}},
		{"(see https://example.com/a), or HTTP://Example.com/b!",
			[]string{"https://example.com/a", "http://example.com/b", "http://example.com/b!"}},
		{"[next](https://example.com/next)[prev](https://example.com/prev)",
			[]string{"https://example.com/next", "https://example.com/prev"}},
		{"https://en.wikipedia.org/wiki/Go_(programming_language) is it",
			[]string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{"[w](https://en.wikipedia.org/wiki/Go_(programming_language))",
			[]string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{`"https://example.com/q?a=1&b=2"`, []string{"https://example.com/q?a=1&b=2"}},
		{"'https://example.com/quoted'", []string{"https://example.com/quoted"}},
		{"<https://example.com/x>", []string{"https://example.com/x"}},
		{"**https://example.com/bold**", []string{"https://example.com/bold"}},
		{"https://r.jina.ai/https://example.com/", []string{"https://r.jina.ai/https://example.com/", "https://example.com/"}},
		{"İstanbul https://example.com/after-a-wide-rune", []string{"https://example.com/after-a-wide-rune"}},
		// A URL's own last character survives where text punctuation would be
		// dropped: a search hit's source is returned exactly so.
		{"https://example.com/search?", []string{"https://example.com/search?", "https://example.com/search"}},
		{"https://example.com/release!", []string{"https://example.com/release!"}},
		{"https://example.com/a)", []string{"https://example.com/a)", "https://example.com/a"}},
		// CJK text ends a URL with its own punctuation and spaces.
		{"看 https://example.com/cjk。", []string{"https://example.com/cjk"}},
		{"https://example.com/wide\u3000次", []string{"https://example.com/wide"}},
		// No space before what follows: CJK text, an em dash, a possessive.
		{"请看https://example.com/docs，然后总结", []string{"https://example.com/docs"}},
		{"看https://example.com/docs然后总结", []string{"https://example.com/docs"}},
		{"看https://example.com然后总结", []string{"https://example.com", "https://example.com/"}},
		// A raw CJK path is the request's escaped one: the text is shorter
		// than the normalized form it matches.
		{"https://example.com/日本語", []string{"https://example.com/%E6%97%A5%E6%9C%AC%E8%AA%9E"}},
		{"見てhttps://example.com/a。お願いします", []string{"https://example.com/a"}},
		{"https://example.com/a—see", []string{"https://example.com/a"}},
		{"https://example.com/a's", []string{"https://example.com/a"}},
		// A bare host ends at text that no host carries, and a long given
		// URL is read short where it can be cut.
		{"See https://example.com's docs, (https://example.org), https://example.net.",
			[]string{"https://example.com", "https://example.org", "https://example.net"}},
		{"https://example.com/a?" + strings.Repeat("b", 2048), []string{"https://example.com/a"}},
		// A bare host, or a query straight after it, reads with the "/" an
		// empty path normalizes to.
		{"https://example.com", []string{"https://example.com/"}},
		{"https://example.com?q=1", []string{"https://example.com/?q=1"}},
		// A URL's own punctuation followed by the sentence's, and a closing
		// parenthesis that is the URL's own inside a link's.
		{"see https://example.com/release!.", []string{"https://example.com/release!", "https://example.com/release"}},
		{"(https://example.com/a))", []string{"https://example.com/a)", "https://example.com/a"}},
		// A search hit's source with characters running text rarely carries.
		{"https://fonts.googleapis.com/css?family=Roboto|Open+Sans", []string{"https://fonts.googleapis.com/css?family=Roboto|Open+Sans"}},
	} {
		for _, u := range tc.urls {
			if !textHolds(tc.text, u) {
				t.Errorf("text %q does not hold %q, want it to", tc.text, u)
			}
		}
	}
	for _, tc := range []struct{ text, url string }{
		{"https://example.com/a", "https://example.com/a?d=1"},
		{"https://example.com/a", "https://example.com/ab"},
		{"https://example.com/a", "https://example.com/"},
		{"https://example.com/a.", "https://example.com/a.b"},
		{"no url here, only httpx://nope and http:/nope", "http://nope/"},
	} {
		if textHolds(tc.text, tc.url) {
			t.Errorf("text %q holds %q, want it not to", tc.text, tc.url)
		}
	}
}

// A page built to make reading quadratic — 100 KiB of URLs on one host run
// together, each cut at every punctuation mark to the end of the run — costs a
// lookup parsing linear in its length, not the gigabytes the unbounded reading
// spent; a request long enough to widen the window past use
// spends the budget and stops.
func TestURLMatcherStaysLinearOnAHostileRun(t *testing.T) {
	run := strings.Repeat("https://docs.example.com/,", 4000)
	m, ok := newURLMatcher("https://docs.example.com/not-given")
	if !ok {
		t.Fatal("matcher refused a plain URL")
	}
	m.scan(run)
	// The window bounds what each occurrence parses, so the total grows with
	// the run's length; the unbounded reading parsed gigabytes of this run.
	if spent := readingBudget - m.budget; m.found != "" || spent > 24<<20 {
		t.Errorf("found %q after parsing %d bytes of a %d-byte run, want nothing found within 24 MiB", m.found, spent, len(run))
	}

	long, ok := newURLMatcher("https://docs.example.com/" + strings.Repeat("a", maxFetchURL-len("https://docs.example.com/")))
	if !ok {
		t.Fatal("matcher refused a URL at maxFetchURL")
	}
	if !long.scan(run) || long.budget > 0 || long.found != "" {
		t.Errorf("a window-wide request: budget left %d, found %q; want the budget spent and nothing found", long.budget, long.found)
	}
	if _, ok := newURLMatcher("https://docs.example.com/" + strings.Repeat("a", maxFetchURL)); ok {
		t.Error("a request longer than maxFetchURL was accepted")
	}
}

// The scan is charged too: a long request, whose window is wide, over text of
// bare "https://" — nothing the matcher would parse — still spends the budget,
// where scanning it uncharged ran for seconds.
func TestURLMatcherChargesTheScan(t *testing.T) {
	long := "https://docs.example.com/" + strings.Repeat("a", maxFetchURL-len("https://docs.example.com/"))
	m, ok := newURLMatcher(long)
	if !ok {
		t.Fatal("matcher refused a URL at maxFetchURL")
	}
	m.scan(strings.Repeat("https://", 200<<10/len("https://")))
	if m.budget > 0 {
		t.Errorf("budget left %d after scanning 200 KiB of bare schemes in a %d-byte window, want it spent", m.budget, m.authMax+m.tailMax)
	}
}
