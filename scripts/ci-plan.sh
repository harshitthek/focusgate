#!/usr/bin/env bash
set -euo pipefail

event=$1
base=$2
force_full=$3

report() {
  printf 'mode=%s\n' "$1" >> "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
  printf 'Validation tier: %s (%s)\n' "$1" "$2"
}

if [[ "$event" != pull_request && "$event" != push ]]; then
  report full "scheduled or manual run"
  exit 0
fi
if [[ "$force_full" == true || "${GITHUB_REF_TYPE:-}" == tag ]]; then
  report full "ci:full label or release tag"
  exit 0
fi
if ! [[ "$base" =~ ^[0-9a-f]{40}$ ]] || ! git merge-base --is-ancestor "$base" HEAD 2>/dev/null; then
  report full "base commit unavailable"
  exit 0
fi

git diff --check --no-ext-diff "$base" HEAD
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
