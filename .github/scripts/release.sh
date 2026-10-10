#!/bin/bash
# Publish dist/ as a GitHub release for the tag $VERSION. Runs inside a throwaway Debian
# container; it only needs curl and jq, so nothing has to be installed on the runner.
#
#   VERSION, GITHUB_REPOSITORY (owner/name), GITHUB_TOKEN (contents: write)
set -euo pipefail
: "${VERSION:?}" "${GITHUB_REPOSITORY:?}" "${GITHUB_TOKEN:?}"
cd "$(dirname "$0")/../.."

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends ca-certificates curl jq

api="https://api.github.com/repos/$GITHUB_REPOSITORY"
gh() { curl -fsS -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" "$@"; }

ls dist/graphrelay_*.tar.gz dist/SHA256SUMS >/dev/null

# Reuse the release if this tag was already published (a re-run), otherwise create it.
# A tag with a hyphen (v1.2.0-rc1) is marked as a pre-release.
if release=$(gh "$api/releases/tags/$VERSION" 2>/dev/null); then
  echo "release $VERSION exists, replacing its files"
else
  prerelease=false
  case "$VERSION" in *-*) prerelease=true ;; esac
  body=$(jq -n --arg tag "$VERSION" --argjson pre "$prerelease" \
    '{tag_name: $tag, name: $tag, prerelease: $pre, generate_release_notes: true}')
  release=$(gh -X POST "$api/releases" -d "$body")
fi
id=$(jq -r .id <<<"$release")

for file in dist/graphrelay_*.tar.gz dist/SHA256SUMS; do
  name=$(basename "$file")
  old=$(jq -r --arg n "$name" '.assets[]? | select(.name == $n) | .id' <<<"$release")
  if [ -n "$old" ]; then
    gh -X DELETE "$api/releases/assets/$old" >/dev/null
  fi
  echo "uploading $name"
  gh -X POST -H "Content-Type: application/octet-stream" --data-binary "@$file" \
     "https://uploads.github.com/repos/$GITHUB_REPOSITORY/releases/$id/assets?name=$name" >/dev/null
done
echo "published: $(jq -r .html_url <<<"$release")"
