#!/bin/bash
# Publish the archives in $DIST as a GitHub release for the tag $VERSION. Runs inside a throwaway
# Debian container; it only needs curl and jq, so nothing has to be installed on the runner.
# The workflow runs a copy of this script that was set aside before the build, with only the
# archives mounted: nothing the build could have modified is executed while the token is present.
#
# $DIST was written by the build container, which also runs third-party code, so its contents are
# not trusted beyond the bytes of the expected files: only fixed file names are uploaded, and
# anything that is not a regular file (a symlink to /proc/self/environ, say) is refused.
#
#   VERSION, GITHUB_REPOSITORY (owner/name), GITHUB_TOKEN (contents: write), DIST (default: dist)
set -euo pipefail
: "${VERSION:?}" "${GITHUB_REPOSITORY:?}" "${GITHUB_TOKEN:?}"
DIST=${DIST:-dist}

case "$VERSION" in
  *[!A-Za-z0-9._-]*) echo "unexpected characters in version $VERSION" >&2; exit 1 ;;
esac
archives=("graphrelay_${VERSION}_linux_amd64.tar.gz" "graphrelay_${VERSION}_linux_arm64.tar.gz")
files=("${archives[@]}" SHA256SUMS)

# Check the files before anything else, and before the token is used.
for name in "${files[@]}"; do
  if [ -L "$DIST/$name" ] || [ ! -f "$DIST/$name" ]; then
    echo "refusing to publish: $name is missing or not a regular file" >&2
    exit 1
  fi
done
# SHA256SUMS must list exactly the archives, and they must match.
listed=$(awk '{print $2}' "$DIST/SHA256SUMS" | sed 's/^\*//' | sort)
expected=$(printf '%s\n' "${archives[@]}" | sort)
if [ "$listed" != "$expected" ]; then
  echo "refusing to publish: SHA256SUMS does not list exactly the expected archives" >&2
  exit 1
fi
(cd "$DIST" && sha256sum -c SHA256SUMS)

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends ca-certificates curl jq

# Give curl the token through a private file and drop it from the environment, so no child
# process carries it.
auth=$(mktemp)
chmod 600 "$auth"
trap 'rm -f "$auth"' EXIT
printf 'Authorization: Bearer %s\n' "$GITHUB_TOKEN" > "$auth"
unset GITHUB_TOKEN

api="https://api.github.com/repos/$GITHUB_REPOSITORY"
gh() { curl -fsS -H @"$auth" -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" "$@"; }

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

for name in "${files[@]}"; do
  old=$(jq -r --arg n "$name" '.assets[]? | select(.name == $n) | .id' <<<"$release")
  if [ -n "$old" ]; then
    gh -X DELETE "$api/releases/assets/$old" >/dev/null
  fi
  echo "uploading $name"
  gh -X POST -H "Content-Type: application/octet-stream" --data-binary "@$DIST/$name" \
     "https://uploads.github.com/repos/$GITHUB_REPOSITORY/releases/$id/assets?name=$name" >/dev/null
done
echo "published: $(jq -r .html_url <<<"$release")"
