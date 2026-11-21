#!/usr/bin/env bash
# Worktree verification stamp helper for .claude/hooks/verify-commit-gate.sh
# (plan 75 slice 7).
#
# Computes a staging-invariant content fingerprint of the current git worktree
# (HEAD commit + every modified/staged/deleted/untracked non-ignored path and
# its content hash) so running `make test-fast` or `make verify` before
# `git add` keeps the stamp valid for `git commit`, while any subsequent file
# edit or commit immediately invalidates it.
#
# Usage:
#   verify-stamp.sh --fingerprint        Print the current worktree fingerprint.
#   verify-stamp.sh --write <method>     Record a fresh stamp in .git/.verify-stamp.
#   verify-stamp.sh --stamp-path         Print the resolved stamp file path.
set -euo pipefail
LC_ALL=C
export LC_ALL

require_git_repo() {
  git rev-parse --is-inside-work-tree >/dev/null 2>&1
}

stamp_path() {
  local git_dir
  git_dir="$(git rev-parse --git-dir)"
  printf '%s/.verify-stamp\n' "$(CDPATH='' cd -- "$git_dir" && pwd -P)"
}

hash_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  else
    python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
  fi
}

hash_file() {
  local f="$1"
  if [ -L "$f" ]; then
    printf 'symlink:%s' "$(readlink "$f")" | hash_stdin
  elif [ -f "$f" ]; then
    hash_stdin < "$f"
  else
    printf 'MISSING'
  fi
}

compute_fingerprint() {
  local root head
  root="$(git rev-parse --show-toplevel)"
  head="$(git rev-parse --verify HEAD 2>/dev/null || printf 'UNBORN')"

  {
    printf 'HEAD %s\n' "$head"
    (
      cd "$root"
      {
        git diff -z --name-only HEAD 2>/dev/null || true
        git diff -z --cached --name-only 2>/dev/null || true
        git ls-files -z --others --exclude-standard 2>/dev/null || true
      } | LC_ALL=C sort -z -u | while IFS= read -r -d '' rel; do
        [ -n "$rel" ] || continue
        printf 'PATH %s\0%s\n' "$rel" "$(hash_file "$rel")"
      done
    )
  } | hash_stdin
}

# has_partial_staging succeeds (exit 0) if any staged path differs from its
# working-tree contents (e.g. MM, AM, MD, AD, RM, CM in `git status --porcelain=v1 -z`).
has_partial_staging() {
  local entry xy
  while IFS= read -r -d '' entry; do
    [ "${#entry}" -ge 3 ] || continue
    xy="${entry:0:2}"
    case "$xy" in
      '??'|'!!'|' '*|?' ')
        ;;
      *)
        return 0
        ;;
    esac
    # Rename/copy entries carry a second NUL-terminated source path; consume it.
    case "${xy:0:1}" in
      R|C)
        IFS= read -r -d '' _ || true
        ;;
    esac
  done < <(git status --porcelain=v1 -z 2>/dev/null)
  return 1
}

write_stamp() {
  local method="$1"
  local path tmp fp now
  path="$(stamp_path)"
  fp="$(compute_fingerprint)"
  now="$(date +%s)"
  # Write atomically inside the git dir so concurrent readers never observe a
  # half-written stamp.
  tmp="$(mktemp "${path}.XXXXXX")"
  {
    printf 'timestamp=%s\n' "$now"
    printf 'method=%s\n' "$method"
    printf 'fingerprint=%s\n' "$fp"
  } > "$tmp"
  mv -f "$tmp" "$path"
}

case "${1:-}" in
  --fingerprint)
    require_git_repo || { echo "verify-stamp: not inside a git worktree" >&2; exit 1; }
    compute_fingerprint
    ;;
  --stamp-path)
    require_git_repo || { echo "verify-stamp: not inside a git worktree" >&2; exit 1; }
    stamp_path
    ;;
  --check-partial-staging)
    require_git_repo || exit 1
    has_partial_staging
    ;;
  --write)
    shift
    method="${*:-manual}"
    if ! require_git_repo; then
      # Allow `make test-fast` / `make verify` in non-git exports to succeed silently.
      exit 0
    fi
    write_stamp "$method"
    ;;
  *)
    echo "usage: $0 (--fingerprint | --stamp-path | --write <method>)" >&2
    exit 2
    ;;
esac
