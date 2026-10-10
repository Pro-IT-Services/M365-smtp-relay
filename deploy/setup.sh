#!/bin/bash
# First installation of Graph Relay on a Debian-style host with systemd.
# Run as root in the extracted release archive:
#
#   bash setup.sh [--repo owner/name] [--token-file /path]
#
# It installs the binary, the systemd units, the firewall helpers and the update tooling, creates
# /opt/graphrelay/config.yaml and /etc/graphrelay.env to fill in, and records where updates come
# from. It does not start the relay: you start it once the configuration is filled in.
# To upgrade an existing installation use graphrelay-update (update.sh) instead.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"

REPO_ARG=""
TOKEN_FILE_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO_ARG=${2:?--repo needs owner/name}; shift 2 ;;
    --token-file) TOKEN_FILE_ARG=${2:?--token-file needs a path}; shift 2 ;;
    -h | --help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $1" >&2; exit 1 ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -x "$HERE/graphrelay" ] || { echo "run this in the extracted release archive (no graphrelay binary here)" >&2; exit 1; }
if [ -e /opt/graphrelay/graphrelay ]; then
  echo "Graph Relay $(/opt/graphrelay/graphrelay -version 2>/dev/null || echo '?') is already installed." >&2
  echo "To upgrade it, run: bash update.sh    (in this directory)  or  graphrelay-update" >&2
  exit 1
fi
for tool in systemctl curl tar sha256sum; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

VERSION=$("$HERE/graphrelay" -version)
echo "== Installing Graph Relay $VERSION"
bash "$HERE/install.sh" --no-start

# Where updates come from. The archive records the repository it was built from.
CONF=/etc/graphrelay/update.conf
if [ ! -e "$CONF" ]; then
  repo=$REPO_ARG
  if [ -z "$repo" ] && [ -r "$HERE/RELEASE" ]; then
    repo=$(sed -n 's/^REPO=//p' "$HERE/RELEASE")
  fi
  {
    echo "# Where graphrelay-update looks for new releases."
    echo "REPO=${repo:-owner/name}"
    echo "# Needed only when the repository is private: a file (mode 0600, root) holding a GitHub"
    echo "# token that can read the repository's contents."
    if [ -n "$TOKEN_FILE_ARG" ]; then echo "TOKEN_FILE=$TOKEN_FILE_ARG"; else echo "#TOKEN_FILE=/etc/graphrelay/update.token"; fi
  } > "$CONF"
  chmod 0644 "$CONF"
  [ -n "$repo" ] || echo "note: set REPO=owner/name in $CONF so graphrelay-update can find new releases"
fi

# A fresh installation is already in the state the host migrations lead to.
STATE=/opt/graphrelay/host-migrations.applied
if [ ! -e "$STATE" ]; then
  : > "$STATE"
  for m in "$HERE"/migrations/[0-9]*.sh; do
    [ -e "$m" ] && basename "$m" .sh >> "$STATE"
  done
fi

cat <<EOF

Graph Relay $VERSION is installed, not started yet. Next:

  1. Edit /opt/graphrelay/config.yaml   (hostname, tls, portal, firewall; see docs/setup.md)
  2. Edit /etc/graphrelay.env           (Graph and, for tls.mode acme, Cloudflare credentials)
  3. Check it:   set -a; . /etc/graphrelay.env; set +a; /opt/graphrelay/graphrelay -check-config -config /opt/graphrelay/config.yaml
  4. Start it:   systemctl start graphrelay && journalctl -u graphrelay -f

Optional: harden this host (updates, key-only SSH, firewall):  bash /opt/graphrelay/bin/harden-host.sh
Later updates:  graphrelay-update --check    and    graphrelay-update
EOF
