- **The `repo-answer` eval runs again** (#358) — the repository-mounting trial
  is back in the live suite now that its fixture exists. It needs
  `EVAL_GITHUB_REPO_URL` and `EVAL_GITHUB_REPO_TOKEN` — renamed from
  `GITHUB_EVAL_REPO_URL` / `GITHUB_EVAL_REPO_TOKEN`, since GitHub refuses to
  store a secret whose name begins with `GITHUB_` — naming a **private**
  repository holding a root-level `PASSPHRASE.txt`, and a fine-grained token
  scoped to that one repository at Contents: Read-only. That privacy is
  load-bearing: it is the only answer-style trial with no per-trial nonce. The
  token and the passphrase now join the artifact scrub, a clone the executor
  retries past a network fault no longer reds the trial, and offline tests pin
  the registered trial set and the count the docs spell, so the two cannot drift
  apart again.
