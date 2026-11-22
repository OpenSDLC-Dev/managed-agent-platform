---
trigger: glob
globs: "**/AGENTS.md,**/REVIEW.md,**/Makefile,**/harness/**,**/.claude/**,**/.codex/**,**/.agents/**,**/tools/harnessdoctor/**,**/tools/harnesshook/**,**/.github/workflows/ci.yml,**/.github/scripts/review_checklist_test.py,**/.github/scripts/verify_commit_gate_test.py,**/.github/scripts/issue_triage_test.py"
---

Edit instructions, skill resources, roles and hook registrations in `harness/`. Run `go run ./tools/harnessdoctor sync` and then `check`. Review the generated diff alongside the source. Use a fresh client to verify changes to discovery, permissions or hook protocols. A gate change takes the full independent verification and two-reviewer path. Load its review checklist from `origin/main:REVIEW.md`.
