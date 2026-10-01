package toolset_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// The descriptions below are the 2026-09-02 recording's, byte for byte
// (managed-agents-wire-recordings 2026-09-02/batch2.json, probe
// sessA.events.after-turn2): asked to print every tool it was given, the
// agent's message echoed each description between its "**Description:** "
// label and its "**JSON Schema:**" heading. The span.model_request_start
// behind that message is stamped recordedRequestAt.
var recordedRequestAt = time.Date(2026, 9, 2, 0, 2, 58, 832978000, time.UTC)

const recordedWebFetchDescription = `Fetch the contents of a web page or a PDF at a given URL.
Usage notes:
- This tool can only fetch EXACT URLs that have been provided directly by the user or have been returned in results from the web_search and web_fetch tools.
- This tool cannot access content that requires authentication, such as private Google Docs or pages behind login walls.
- Do not add www. to URLs that do not have them.
- URLs must include the schema: https://example.com is a valid URL while example.com is an invalid URL.`

const recordedWebSearchDescription = `The web_search tool searches the internet and returns up-to-date information from web sources.
<when_to_use_web_search>
Your knowledge is comprehensive and sufficient to answer queries that do not need recent info.

Do NOT search for general knowledge you already have:
- Stable info: changes slowly over years, changes since knowledge cutoff unlikely
- Fundamental explanations, definitions, theories, or established facts
- Casual chats, or about feelings or thoughts
- For example, never search for help me code X, eli5 special relativity, capital of france, when constitution signed, who is dario amodei, or how bloody mary was created.

Do search for queries where web search would be helpful:
- Answering requires real-time data or frequently changing info (daily/weekly/monthly)
- Finding specific facts you don't know
- When user implies recent info is necessary
- Current conditions or recent events (e.g. weather forecast, news) that are past the knowledge cutoff
- Clear indicators that the user wants a search, e.g. they explicitly ask for search
- To confirm technical info that is likely outdated

If web search is needed, search the fewest number of times possible to answer the user's query, and default to one search.
</when_to_use_web_search>
<query_guidelines>
- Keep search queries short and specific - 1-6 words for best results
- Include time frames or date ranges only when appropriate for time-sensitive queries. Include version numbers only if specified.
- Break complex information needs into multiple focused queries
- EVERY query must be meaningfully distinct from previous queries - repeating phrases does not yield different results
- Never use special search operators like '-', 'site', '+' or ` + "`" + `NOT` + "`" + ` unless explicitly asked or required for the query
- If you are asked about identifying a person using search, NEVER include the name of the person within the search query for privacy
- For real-time events (sports games, news, stock prices, etc.), you may search for up-to-date info by including 'today' in the search query
- Today's date is 2026-09-02
</query_guidelines>
<response_guidelines>
- Prioritize the highest-quality sources for the query (i.e. official docs for technical queries, peer-reviewed papers for academics, SEC filings for finance)
- Lead with the most recent, relevant information; prioritize sources from the last 1-3 months for rapidly evolving topics
- Note when sources conflict and cite both perspectives
- If a requested source isn't in the results, or there are no results, inform user
- Never explicitly mention the need to use the web search tool when answering a question or justify the use of the tool out loud. Instead, just search directly.
</response_guidelines>`

// recordedDateLine is the one line of recordedWebSearchDescription that moves.
const recordedDateLine = "- Today's date is 2026-09-02\n"

// descriptions resolves the two web tools at now and returns each one's
// description by name.
func descriptions(t *testing.T, now time.Time) map[string]string {
	t.Helper()
	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`), now)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	out := map[string]string{}
	for _, raw := range defs {
		var d struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition: %v", err)
		}
		if d.Name == "web_fetch" || d.Name == "web_search" {
			out[d.Name] = d.Description
		}
	}
	if len(out) != 2 {
		t.Fatalf("resolved %d of the two web tools", len(out))
	}
	return out
}

// Rendered at the instant the recorded request ran, both descriptions are the
// recording's, byte for byte — the date line included.
func TestWebToolDescriptionsMatchTheRecording(t *testing.T) {
	got := descriptions(t, recordedRequestAt)
	for name, want := range map[string]string{
		"web_fetch":  recordedWebFetchDescription,
		"web_search": recordedWebSearchDescription,
	} {
		if got[name] != want {
			t.Errorf("%s description =\n%q\nwant the recorded\n%q", name, got[name], want)
		}
	}
}

// The date line is the request's date in UTC, rendered when the definitions
// are, and it is the only thing that moves: the rest of web_search's text and
// all of web_fetch's are the same on every day. The UTC boundary is the
// rendering's, not the clock's location's — a clock reading 07:00 in UTC+8 is
// still the previous day in UTC.
func TestWebSearchDateLineFollowsTheClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Time
		date string
	}{
		{"a later day", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), "2026-10-01"},
		{"the last second of a UTC day", time.Date(2026, 9, 1, 23, 59, 59, 0, time.UTC), "2026-09-01"},
		{"the first instant of the next", time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), "2026-09-02"},
		{"a clock ahead of UTC", time.Date(2026, 9, 2, 7, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)), "2026-09-01"},
		{"a clock behind UTC", time.Date(2026, 9, 1, 20, 0, 0, 0, time.FixedZone("UTC-7", -7*3600)), "2026-09-02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := descriptions(t, tc.now)
			want := strings.Replace(recordedWebSearchDescription, recordedDateLine,
				"- Today's date is "+tc.date+"\n", 1)
			if got["web_search"] != want {
				t.Errorf("web_search description =\n%q\nwant the recorded text dated %s", got["web_search"], tc.date)
			}
			if got["web_fetch"] != recordedWebFetchDescription {
				t.Errorf("web_fetch description = %q, want the recorded text on every day", got["web_fetch"])
			}
		})
	}
}
