#!/usr/bin/env bash
# Sourced by the deploy, eval and parked-staging notifiers.
notifier_marker() {
  printf '<!-- platform-notifier:%s -->\n' "$1"
}

# Print every open tracking issue, newest first. The REST creator filter keeps
# pagination small. Check the author and exclude pull requests before using a
# number for a comment or an automatic close.
open_notifier_issues() {
  local kind="$1" title="$2" first="" last marker
  # Older bot issues have no source marker. Match their original title and
  # the lines these workflows wrote, including the per-run footer for alerts.
  # The backticks below are part of the Markdown copied into the issue body.
  # shellcheck disable=SC2016
  case "$kind" in
    deploy-failure) first='`deploy` finished **'; last='<!-- deploy-alert run:' ;;
    deploy-notifier) first='`deploy` succeeded on `'; last='<!-- deploy-alert run:' ;;
    evals) last='<!-- evals-alert ' ;;
    staging-parked)
      first='The staging cluster `'
      last='It closes automatically: the next run of this workflow that finds the cluster unparked, or gone, closes it,' ;;
    *) echo "unknown notifier: $kind" >&2; return 1 ;;
  esac
  marker="$(notifier_marker "$kind")"
  gh api --paginate "repos/$REPO/issues?state=open&creator=github-actions%5Bbot%5D&per_page=100" |
    jq -r --arg marker "$marker" --arg title "$title" --arg first "$first" --arg last "$last" '
      .[] | select(.pull_request == null and .user.login == "github-actions[bot]")
      | ((.body // "") | rtrimstr("\n") | split("\n")) as $lines
      | (($lines[0] // "") | rtrimstr("\r")) as $header
      | select(if ($header | startswith("<!-- platform-notifier:")) then $header == $marker
               else .title == $title and ($header | startswith($first)) and
                    (($lines[-1] // "") | startswith($last)) end)
      | .number'
}
