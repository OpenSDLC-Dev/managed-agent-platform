- **Two documented lists and one documented limit are now enforced by tests** (#413) — a doc that
  goes stale silently is the failure this work exists to stop, so three of them are pinned. The
  changelog assembler refuses a fragment over the 1,500-byte cap `changelog.d/README.md` states,
  measured in bytes rather than runes because the entries are full of multi-byte punctuation. An
  offline test fails if any doc enumerates a wire ID-prefix list that disagrees with
  `internal/domain/id.go`, and another fails if a `RUN_LIVE_*` consent variable the tree reads has
  no row in README's tier table. All three found real drift on their first run.
