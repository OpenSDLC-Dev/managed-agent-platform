- **A `protocol: anthropic` model route can opt into flattening `search_result`
  blocks in replayed `tool_result` content** (#565) — set `flatten_search_results: true`
  in `model_providers` config for an endpoint whose Messages implementation rejects the
  block, as MiniMax's does (`400 invalid params, invalid tool_result content`), which
  otherwise ends the first turn after a `web_search` call with `retries_exhausted`. Each
  `search_result` block is rewritten to text using the rendering shared with
  `internal/provider/openai` (`provider.SearchResultText`); every other content block,
  the string form of `tool_result` content, and a block the rendering cannot parse all
  pass through unchanged. Default off, and rejected at config load on a
  `protocol: openai` route, which already flattens unconditionally — see
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) for why.
