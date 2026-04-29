#!/usr/bin/env bash
# Generate the Downloads section for a release and (optionally) update the
# release body in place. Reads the actual uploaded assets via `gh release
# view`, so adding or removing a platform in the workflow needs no change
# here.
#
# Usage:
#   TAG=v0.4.0 ./scripts/release-notes.sh           # print to stdout (dry run)
#   TAG=v0.4.0 ./scripts/release-notes.sh --update  # also `gh release edit`
#
# Idempotent: if the body already contains a downloads block (delimited by
# the marker comments below), it is stripped before prepending the fresh
# one, so re-running on the same tag does not stack tables.

set -euo pipefail

TAG="${TAG:-${GITHUB_REF_NAME:-}}"
[ -n "$TAG" ] || { echo "TAG is required (env or GITHUB_REF_NAME)" >&2; exit 1; }

REPO="${GH_REPO:-${GITHUB_REPOSITORY:-selfbase-dev/s2-sync}}"
BASE="https://github.com/$REPO/releases/download/$TAG"

START_MARK="<!-- downloads:start -->"
END_MARK="<!-- downloads:end -->"

release_json=$(gh release view "$TAG" --repo "$REPO" --json assets,body)
assets=$(printf '%s' "$release_json" | jq -r '.assets[].name' | sort)
existing_body=$(printf '%s' "$release_json" | jq -r '.body // ""')

# Display name for known os/arch tuples. Unknown values fall through as-is
# instead of breaking, so a new platform never blocks a release.
pretty_platform() {
  case "$1" in
    darwin_arm64)  echo "macOS (Apple Silicon)" ;;
    darwin_amd64)  echo "macOS (Intel)" ;;
    windows_amd64) echo "Windows x86_64" ;;
    windows_arm64) echo "Windows arm64" ;;
    linux_amd64)   echo "Linux x86_64" ;;
    linux_arm64)   echo "Linux arm64" ;;
    *)             echo "$1" ;;
  esac
}

# Preferred display order. Anything not in this list is appended afterward
# (in lexicographic order via the sorted asset list).
ORDER="darwin_arm64 darwin_amd64 windows_amd64 windows_arm64 linux_amd64 linux_arm64"

emit_table() {
  local prefix="$1"
  local matched=()
  while IFS= read -r name; do
    [ -n "$name" ] && matched+=("$name")
  done < <(printf '%s\n' "$assets" | grep -E "^${prefix}_[^/]+\.(zip|tar\.gz)$" || true)

  [ "${#matched[@]}" -eq 0 ] && return 0

  printf "| Platform | Download |\n|---|---|\n"

  local emitted=" "
  local plat name label
  for plat in $ORDER; do
    for name in "${matched[@]}"; do
      [[ "$name" =~ ^${prefix}_${plat}\.(zip|tar\.gz)$ ]] || continue
      label=$(pretty_platform "$plat")
      printf "| %s | [\`%s\`](%s/%s) |\n" "$label" "$name" "$BASE" "$name"
      emitted+="$name "
    done
  done
  for name in "${matched[@]}"; do
    [[ "$emitted" == *" $name "* ]] && continue
    plat=$(printf '%s' "$name" | sed -E "s|^${prefix}_||; s|\.(zip|tar\.gz)$||")
    printf "| %s | [\`%s\`](%s/%s) |\n" "$plat" "$name" "$BASE" "$name"
  done
}

# Strip any prior downloads block so re-runs do not stack.
clean_body=$(printf '%s\n' "$existing_body" | awk -v s="$START_MARK" -v e="$END_MARK" '
  $0 == s        { skip = 1; next }
  $0 == e        { skip = 0; next }
  skip != 1      { print }
')

gui_table=$(emit_table "s2sync")
cli_table=$(emit_table "s2sync-cli")

tmpfile=$(mktemp)
{
  if [ -n "$gui_table" ] || [ -n "$cli_table" ]; then
    echo "$START_MARK"
    echo "## Downloads"
    echo
    if [ -n "$gui_table" ]; then
      echo "### Desktop App"
      echo
      echo "Recommended for most users. macOS / Windows / Linux."
      echo
      printf '%s\n' "$gui_table"
      echo
    fi
    if [ -n "$cli_table" ]; then
      echo "### CLI"
      echo
      echo "For scripting and headless environments."
      echo
      printf '%s\n' "$cli_table"
      echo
    fi
    if printf '%s\n' "$assets" | grep -qx 'checksums.txt'; then
      echo "Verify with [\`checksums.txt\`]($BASE/checksums.txt): \`shasum -a 256 -c checksums.txt --ignore-missing\`"
      echo
    fi
    echo "$END_MARK"
    echo
  fi
  printf '%s' "$clean_body"
} > "$tmpfile"

if [ "${1:-}" = "--update" ]; then
  gh release edit "$TAG" --repo "$REPO" --notes-file "$tmpfile"
  echo "Updated release notes for $TAG"
else
  cat "$tmpfile"
fi
rm -f "$tmpfile"
