# Shared by update.sh and docker/update.sh: find and download Graph Relay releases on GitHub.
# Sourced, not executed. Needs curl, tar and sha256sum; no jq, so it runs on a minimal server.
#
# Settings come from /etc/graphrelay/update.conf (shell syntax):
#   REPO=owner/name            the GitHub repository that publishes the releases
# Without a REPO there, the one recorded in /opt/graphrelay/RELEASE is used.
#   TOKEN_FILE=/path           optional: file holding a token with read access to the repository's
#                              contents; required when the repository is private
# GITHUB_TOKEN in the environment overrides TOKEN_FILE.

GR_CONF=${GR_CONF:-/etc/graphrelay/update.conf}
GR_RELEASE_FILE=${GR_RELEASE_FILE:-/opt/graphrelay/RELEASE}

gr_die() { echo "error: $*" >&2; exit 1; }

# gr_load_conf sets REPO and GR_TOKEN.
gr_load_conf() {
  REPO=${REPO:-}
  TOKEN_FILE=${TOKEN_FILE:-}
  if [ -r "$GR_CONF" ]; then
    # shellcheck disable=SC1090
    . "$GR_CONF"
  fi
  # An installation upgraded from a release without setup.sh has no update.conf yet: fall back to
  # the repository the installed release was built from.
  if [ -z "$REPO" ] && [ -r "$GR_RELEASE_FILE" ]; then
    REPO=$(sed -n 's/^REPO=//p' "$GR_RELEASE_FILE" | head -n 1)
  fi
  [ -n "$REPO" ] || gr_die "no repository configured: set REPO=owner/name in $GR_CONF"
  case "$REPO" in
    */*) ;;
    *) gr_die "REPO in $GR_CONF must look like owner/name" ;;
  esac
  GR_TOKEN=${GITHUB_TOKEN:-}
  if [ -z "$GR_TOKEN" ] && [ -n "$TOKEN_FILE" ]; then
    [ -r "$TOKEN_FILE" ] || gr_die "token file $TOKEN_FILE is not readable"
    GR_TOKEN=$(tr -d ' \r\n' < "$TOKEN_FILE")
  fi
}

# gr_api URL [curl options...]: GET from the GitHub API, with the token if there is one.
# The token is passed through a private header file, not on the command line.
gr_api() {
  local url=$1; shift
  if [ -n "${GR_TOKEN:-}" ]; then
    local hdr; hdr=$(mktemp); chmod 600 "$hdr"
    printf 'Authorization: Bearer %s\n' "$GR_TOKEN" > "$hdr"
    local rc=0
    curl -fsSL --retry 2 -H @"$hdr" -H "X-GitHub-Api-Version: 2022-11-28" "$@" "$url" || rc=$?
    rm -f "$hdr"
    return $rc
  fi
  curl -fsSL --retry 2 -H "X-GitHub-Api-Version: 2022-11-28" "$@" "$url"
}

# gr_release_json TAG|latest: the release's JSON document.
gr_release_json() {
  local which=$1 path
  if [ "$which" = latest ]; then path="releases/latest"; else path="releases/tags/$which"; fi
  gr_api "https://api.github.com/repos/$REPO/$path" -H "Accept: application/vnd.github+json" || {
    if [ -z "${GR_TOKEN:-}" ]; then
      gr_die "cannot read $path of $REPO. If the repository is private, a token is needed: see TOKEN_FILE in $GR_CONF"
    fi
    gr_die "cannot read $path of $REPO (check the repository name and the token's access)"
  }
}

# gr_tag_of JSON: the release's tag name.
gr_tag_of() {
  printf '%s\n' "$1" | sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
}

# gr_asset_url JSON NAME: the API URL of the release asset called NAME.
# In GitHub's JSON every asset object starts with its "url" (…/releases/assets/<id>) and has
# "name" a few lines later.
gr_asset_url() {
  printf '%s\n' "$1" | awk -F'"' -v want="$2" '
    /"url": *"[^"]*\/releases\/assets\/[0-9]+"/ { url = $4 }
    /"name": *"/ { if (url != "" && $4 == want) { print url; exit } ; if ($2 == "name") url = "" }'
}

# gr_arch: the release architecture name of this machine.
gr_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) gr_die "no release is built for $(uname -m)" ;;
  esac
}

# gr_newer A B: true if version A is newer than B. Anything that is not a vN.N.N release
# (a development build) counts as older than every release.
gr_newer() {
  local a=$1 b=$2
  [ "$a" != "$b" ] || return 1
  case "$b" in v[0-9]*) ;; *) return 0 ;; esac
  case "$a" in v[0-9]*) ;; *) return 1 ;; esac
  [ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n 1)" = "$a" ]
}

# gr_download TAG DESTDIR: download the archive for this machine and SHA256SUMS, verify the
# checksum and extract. Prints the path of the extracted directory.
gr_download() {
  local tag=$1 dest=$2 json name url f
  json=$(gr_release_json "$tag")
  name="graphrelay_${tag}_linux_$(gr_arch).tar.gz"
  mkdir -p "$dest"
  for f in "$name" SHA256SUMS; do
    url=$(gr_asset_url "$json" "$f")
    [ -n "$url" ] || gr_die "release $tag of $REPO has no file $f"
    gr_api "$url" -H "Accept: application/octet-stream" -o "$dest/$f" || gr_die "download of $f failed"
  done
  (cd "$dest" && sha256sum -c --ignore-missing SHA256SUMS >&2) || gr_die "checksum of $name does not match SHA256SUMS"
  tar -C "$dest" -xzf "$dest/$name" || gr_die "cannot extract $name"
  [ -x "$dest/${name%.tar.gz}/graphrelay" ] || gr_die "$name does not contain the graphrelay binary"
  echo "$dest/${name%.tar.gz}"
}
