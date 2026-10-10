#!/bin/bash
# For development: build Graph Relay from this checkout and install it on a host, from a
# workstation with Go (Git Bash, Linux or macOS). Normal installs use a published release.
#
#   deploy/push.sh root@relay.example.com
#
# It builds the same directory layout as a release archive and runs that layout's own update.sh
# on the host (or setup.sh, if the host has no installation yet), so a development build goes
# through exactly the steps a release does: backup, migrations, install, health check, rollback.
set -euo pipefail
HOST=${1:?usage: deploy/push.sh user@host}
cd "$(dirname "$0")/.."

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
VERSION=$(git describe --always --dirty --tags)
NAME=graphrelay_${VERSION}_linux_amd64
OUT=$TMP/$NAME
mkdir -p "$OUT/migrations" "$OUT/docker" "$OUT/docs"

echo "== build $VERSION"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/graphrelay" ./cmd/graphrelay
for f in deploy/setup.sh deploy/update.sh deploy/install.sh deploy/release-lib.sh deploy/harden-host.sh \
         deploy/graphrelay.service deploy/graphrelay.env.example \
         deploy/graphrelay-fw-sync deploy/graphrelay-fw.service deploy/graphrelay-fw.path \
         deploy/graphrelay-fw-report deploy/graphrelay-fw-report.service deploy/graphrelay-fw-report.timer \
         deploy/graphrelay-egress-sync deploy/graphrelay-egress.service deploy/graphrelay-egress.timer \
         config.example.yaml README.md; do
  cp "$f" "$OUT/"
done
cp deploy/migrations/[0-9]*.sh deploy/migrations/README.md "$OUT/migrations/"
cp deploy/docker/Dockerfile deploy/docker/compose.yaml deploy/docker/setup.sh deploy/docker/update.sh "$OUT/docker/"
cp docs/*.md "$OUT/docs/"
printf 'VERSION=%s\nREPO=\n' "$VERSION" > "$OUT/RELEASE"
tar -C "$TMP" -czf "$TMP/$NAME.tar.gz" "$NAME"

echo "== upload to $HOST"
scp -q "$TMP/$NAME.tar.gz" "$HOST:/root/$NAME.tar.gz"

echo "== install"
# shellcheck disable=SC2029
ssh "$HOST" "set -e; cd /root; rm -rf '$NAME'; tar -xzf '$NAME.tar.gz'; cd '$NAME'; chmod 0755 graphrelay *.sh graphrelay-fw-sync graphrelay-fw-report graphrelay-egress-sync migrations/*.sh docker/*.sh
  if [ -x /opt/graphrelay/graphrelay ]; then bash update.sh --force; else bash setup.sh; fi
  cd /root; rm -rf '$NAME' '$NAME.tar.gz'"
