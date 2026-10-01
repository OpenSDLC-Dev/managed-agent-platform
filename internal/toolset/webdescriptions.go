package toolset

import "time"

// The two web tools' descriptions are the reference's, copied verbatim from a
// 2026-09-02 recording in which the agent, asked to print every tool it was
// given, echoed each one's name, description and input schema
// (docs/DIVERGENCES.md weighs that echo as evidence). Adopting them, the daily
// date line included, is an owner decision on #682.

// webFetchDescription is web_fetch's, whole. Its rule that the tool "can only
// fetch EXACT URLs" the user gave or a search or fetch returned is the
// reference's text, not a check this platform makes: the executor enforces
// the scheme and the operator allowlist only (docs/DIVERGENCES.md, #823).
const webFetchDescription = `Fetch the contents of a web page or a PDF at a given URL.
Usage notes:
- This tool can only fetch EXACT URLs that have been provided directly by the user or have been returned in results from the web_search and web_fetch tools.
- This tool cannot access content that requires authentication, such as private Google Docs or pages behind login walls.
- Do not add www. to URLs that do not have them.
- URLs must include the schema: https://example.com is a valid URL while example.com is an invalid URL.`

// webSearchDescription renders web_search's for a request made at now. The
// reference's text carries the day's date as the last of its query guidelines,
// "- Today's date is 2026-09-02" on the recorded day, so the definition is
// rebuilt for every request and changes once a day — moving the cached prompt
// prefix with it, a cost the owner accepted. The date is now's in UTC: the
// recording rules out a zone behind UTC (its request ran three minutes after
// a UTC midnight and read the new day) but cannot tell UTC from one ahead of
// it, so the zone is INFERRED (docs/DIVERGENCES.md).
func webSearchDescription(now time.Time) string {
	return webSearchDescriptionHead + now.UTC().Format(time.DateOnly) + webSearchDescriptionTail
}

// webSearchDescriptionHead and webSearchDescriptionTail are the recorded text
// either side of the date.
const (
	webSearchDescriptionHead = `The web_search tool searches the internet and returns up-to-date information from web sources.
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
- Today's date is `
	webSearchDescriptionTail = `
</query_guidelines>
<response_guidelines>
- Prioritize the highest-quality sources for the query (i.e. official docs for technical queries, peer-reviewed papers for academics, SEC filings for finance)
- Lead with the most recent, relevant information; prioritize sources from the last 1-3 months for rapidly evolving topics
- Note when sources conflict and cite both perspectives
- If a requested source isn't in the results, or there are no results, inform user
- Never explicitly mention the need to use the web search tool when answering a question or justify the use of the tool out loud. Instead, just search directly.
</response_guidelines>`
)
