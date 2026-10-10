#!/bin/bash
# First installation of Graph Relay as a Docker container.
# Run as root (or a user allowed to use Docker) in the extracted release archive:
#
#   bash docker/setup.sh [--dir /opt/graphrelay-docker] [--repo owner/name] [--token-file /path]
#
# It builds the image graphrelay:<version> from the binary in this archive, and creates the
# installation directory with compose.yaml, config.yaml, graphrelay.env and data/. It does not
# start the container: you start it once the configuration is filled in.
# Needs Docker with the compose plugin. To upgrade later use graphrelay-docker-update.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd) # .../docker
SRC=$(cd "$HERE/.." && pwd)        # the extracted archive

DIR=/opt/graphrelay-docker
REPO_ARG=""
TOKEN_FILE_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR=${2:?--dir needs a path}; shift 2 ;;
    --repo) REPO_ARG=${2:?--repo needs owner/name}; shift 2 ;;
    --token-file) TOKEN_FILE_ARG=${2:?--token-file needs a path}; shift 2 ;;
    -h | --help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $1" >&2; exit 1 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }
[ -x "$SRC/graphrelay" ] || die "run this from the extracted release archive (no graphrelay binary in $SRC)"
command -v docker >/dev/null || die "Docker is not installed"
docker compose version >/dev/null 2>&1 || die "the Docker compose plugin is not installed"
[ ! -e "$DIR/compose.yaml" ] || die "$DIR already holds an installation; to upgrade run: bash docker/update.sh --dir $DIR"

VERSION=$("$SRC/graphrelay" -version)
echo "== Building image graphrelay:$VERSION"
# A minimal build context: just the binary.
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
cp "$SRC/graphrelay" "$ctx/graphrelay"
docker build --pull -t "graphrelay:$VERSION" -f "$HERE/Dockerfile" "$ctx"

echo "== Creating $DIR"
install -d -m 0750 "$DIR"
install -d -m 0700 "$DIR/data" "$DIR/backups"
install -d -m 0755 "$DIR/bin"
chown 10001:10001 "$DIR/data" # the user the container runs as
install -m 0644 "$HERE/compose.yaml" "$DIR/compose.yaml"
echo "GRAPHRELAY_VERSION=$VERSION" > "$DIR/.env"

# The example config, adjusted for the container: its own data directory and unprivileged ports.
sed -e 's#^data_dir: .*#data_dir: /var/lib/graphrelay#' \
    -e 's#^\( *listen_smtp: *\)":25"#\1":2525"#' \
    -e 's#^\( *listen_smtps: *\)":465"#\1":4465"#' \
    -e 's#^\( *listen_submission: *\)":587"#\1":5587"#' \
    "$SRC/config.example.yaml" > "$DIR/config.yaml"
chmod 0644 "$DIR/config.yaml"
install -m 0600 "$SRC/graphrelay.env.example" "$DIR/graphrelay.env"

# Update tooling stays on the host.
install -m 0755 "$HERE/update.sh" "$DIR/bin/graphrelay-docker-update"
install -m 0644 "$SRC/release-lib.sh" "$DIR/bin/release-lib.sh"
install -m 0644 "$HERE/Dockerfile" "$DIR/bin/Dockerfile"
ln -sfn "$DIR/bin/graphrelay-docker-update" /usr/local/sbin/graphrelay-docker-update 2>/dev/null || true

install -d -m 0755 /etc/graphrelay
CONF=/etc/graphrelay/update.conf
if [ ! -e "$CONF" ]; then
  repo=$REPO_ARG
  if [ -z "$repo" ] && [ -r "$SRC/RELEASE" ]; then
    repo=$(sed -n 's/^REPO=//p' "$SRC/RELEASE")
  fi
  {
    echo "# Where the update scripts look for new releases."
    echo "REPO=${repo:-owner/name}"
    echo "# Needed only when the repository is private: a file (mode 0600, root) holding a GitHub"
    echo "# token that can read the repository's contents."
    if [ -n "$TOKEN_FILE_ARG" ]; then echo "TOKEN_FILE=$TOKEN_FILE_ARG"; else echo "#TOKEN_FILE=/etc/graphrelay/update.token"; fi
  } > "$CONF"
fi

cat <<EOF

Graph Relay $VERSION is prepared in $DIR, not started yet. Next:

  1. Edit $DIR/config.yaml       (hostname, tls, portal; keep the 2525/4465/5587 ports and data_dir)
  2. Edit $DIR/graphrelay.env    (Graph and, for tls.mode acme, Cloudflare credentials)
  3. Check it:   cd $DIR && docker compose run --rm graphrelay -check-config -config /etc/graphrelay/config.yaml
  4. Start it:   cd $DIR && docker compose up -d && docker compose logs -f

Notes for Docker:
  - Leave firewall.allowlist_file and firewall.blocked_dir empty: the nftables integration is for
    direct installs. Restrict the published ports with your host or network firewall instead.
  - Host rules match the client address the container sees. With the port mappings in
    compose.yaml that is the real client address for connections from other machines.
Later updates:  graphrelay-docker-update --check    and    graphrelay-docker-update
EOF
