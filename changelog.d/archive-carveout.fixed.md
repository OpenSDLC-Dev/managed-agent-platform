- **AGENTS.md stops forbidding the release ritual's own last step, and the fragment sweep
  stops depending on a fresh local `main`** — compressing AGENTS.md's docs-move-with-code
  bullet dropped the carve-out that lets `make changelog-archive` touch CHANGELOG.md, so for
  all of v0.3.0 the mirror external agents read called that step a rule violation; CLAUDE.md
  had kept it. Restored, with three corrections to [docs/RELEASING.md](./docs/RELEASING.md)
  beside it: step 6's sweep enumerates consumed fragments against `origin/main` rather than
  `main`, because a stale local ref moves the merge base back and sweeps earlier cuts' too;
  its exclusion names the frozen archives by pattern instead of exempting all of
  docs/changelog/, so an index or notes file landing beside them is swept; and step 9 returns
  Active work to **None** only when nothing else is in flight. Step 8 now also says what
  becomes of a change merged between the release PR and the tag: it ships in that release
  while its fragment folds into the next one's section, so the fragment names its
  version. (#426)
