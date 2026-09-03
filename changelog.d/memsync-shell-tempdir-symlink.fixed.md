- **The memsync shell-contract tests stop reddening `make verify` on macOS** (#554). Both
  handed `mountPrelude` a `t.TempDir()`, which macOS puts under `/var` — a symlink to
  `/private/var` — so the guard refused the mount and the tests failed before reaching the
  contract they pin. The fixtures now resolve to their physical path, while the two that
  deliberately build a symlink still pass it unresolved to assert the refusal. Their
  prerequisite check was too shallow to have protected them anyway: it probed one of the six
  GNU extensions these commands use, so a host with BSD `rmdir` ran the suite and returned a
  wrong answer rather than skipping. All six are probed now — each test only the ones its own
  command uses, so macOS keeps the hash tree it can run and skips only the removal — and on
  Linux a missing one fails instead of skipping. Test-only.
