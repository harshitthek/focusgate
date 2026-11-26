#!/usr/bin/env bash
set -euo pipefail

event=$1
base=$2
pr_mode=$3

report() {
  printf 'mode=%s\n' "$1" >> "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
  printf 'Validation tier: %s (%s)\n' "$1" "$2"
}

if [[ "${GITHUB_REF_TYPE:-}" == tag ]]; then
  report full "release tag"
  exit 0
fi
if [[ "$event" != pull_request && "$event" != push ]]; then
  report full "scheduled or manual run"
  exit 0
fi
if [[ "$event" == pull_request ]]; then
  case "$pr_mode" in
    skip) report skip "ci:skip label"; exit 0 ;;
    basic|full) ;;
    *) printf 'Could not determine the PR validation tier.\n' >&2; exit 1 ;;
  esac
fi
if ! [[ "$base" =~ ^[0-9a-f]{40}$ ]] || ! git merge-base --is-ancestor "$base" HEAD 2>/dev/null; then
  if [[ "$event" == pull_request ]]; then
    printf 'Could not verify the PR base commit.\n' >&2
    exit 1
  fi
  report full "base commit unavailable"
  exit 0
fi

git diff --check --no-ext-diff "$base" HEAD
if [[ "$event" == pull_request ]]; then
  if [[ "$pr_mode" == full ]]; then
    report full "ci:full label"
  else
    report basic "default PR validation"
  fi
  exit 0
fi

changed=$(mktemp)
trap 'rm -f -- "$changed"' EXIT
git diff --name-only --no-ext-diff --no-renames -z "$base" HEAD -- > "$changed"
if [[ ! -s "$changed" ]]; then
  report full "no changed paths"
  exit 0
fi

while IFS= read -r -d '' path; do
  case "$path" in
    README.md|CONTRIBUTING.md|docs/*.md|.github/pull_request_template.md) ;;
    *)
      report full "changes outside the documentation allowlist"
      exit 0
      ;;
  esac
done < "$changed"
report docs "documentation-only changes"
