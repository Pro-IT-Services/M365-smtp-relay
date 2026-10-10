#!/bin/bash
# Put the files of this release in place on a Debian-style host with systemd. This is the file
# installer used by setup.sh (first install) and update.sh (upgrade); run those instead of this.
# It is idempotent and never overwrites an existing config.yaml or /etc/graphrelay.env.
#
#   install.sh [--no-start]
#
# Run as root from the extracted release archive, which holds: graphrelay (the binary),
# config.example.yaml (or your own config.yaml), the systemd units, the firewall helpers,
# setup.sh, update.sh, release-lib.sh and migrations/.
set -euo pipefail
cd "$(dirname "$0")"

START=1
[ "${1:-}" = "--no-start" ] && START=0
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

PREFIX=/opt/graphrelay

id graphrelay >/dev/null 2>&1 || useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin graphrelay

install -d -o root -g graphrelay -m 0750 "$PREFIX"
install -d -o graphrelay -g graphrelay -m 0700 "$PREFIX/data"
install -d -o root -g root -m 0755 "$PREFIX/bin"
install -d -o root -g root -m 0700 "$PREFIX/backups"
install -d -o root -g root -m 0755 /etc/graphrelay

# The binary and config are root-owned, so the service cannot modify them.
install -o root -g root -m 0755 graphrelay "$PREFIX/graphrelay.new"
mv -f "$PREFIX/graphrelay.new" "$PREFIX/graphrelay"

if [ ! -e "$PREFIX/config.yaml" ]; then
  if [ -e config.yaml ]; then
    install -o root -g graphrelay -m 0640 config.yaml "$PREFIX/config.yaml"
  else
    # A starting point. data_dir must point into the service's writable directory.
    sed 's#^data_dir: .*#data_dir: /opt/graphrelay/data#' config.example.yaml > "$PREFIX/config.yaml"
    chown root:graphrelay "$PREFIX/config.yaml"
    chmod 0640 "$PREFIX/config.yaml"
    echo "created $PREFIX/config.yaml from the example: edit it before starting the relay"
  fi
fi
[ -e /etc/graphrelay.env ] || install -o root -g root -m 0600 graphrelay.env.example /etc/graphrelay.env

install -o root -g root -m 0755 graphrelay-fw-sync graphrelay-fw-report graphrelay-egress-sync /usr/local/sbin/
install -o root -g root -m 0644 graphrelay.service graphrelay-fw.service graphrelay-fw.path \
  graphrelay-fw-report.service graphrelay-fw-report.timer \
  graphrelay-egress.service graphrelay-egress.timer /etc/systemd/system/

# The update tooling stays on the host, so later updates need nothing but this server.
install -o root -g root -m 0755 update.sh "$PREFIX/bin/graphrelay-update"
install -o root -g root -m 0644 release-lib.sh "$PREFIX/bin/release-lib.sh"
install -o root -g root -m 0755 harden-host.sh "$PREFIX/bin/harden-host.sh"
ln -sfn "$PREFIX/bin/graphrelay-update" /usr/local/sbin/graphrelay-update
if [ -e RELEASE ]; then
  install -o root -g root -m 0644 RELEASE "$PREFIX/RELEASE"
fi

systemctl daemon-reload
systemctl enable -q graphrelay-fw.service graphrelay-fw.path graphrelay-fw-report.timer graphrelay.service \
  graphrelay-egress.service graphrelay-egress.timer
systemctl start graphrelay-fw.path
systemctl start graphrelay-fw.service
systemctl start graphrelay-fw-report.timer
systemctl start graphrelay-fw-report.service
# Does nothing unless the firewall limits outbound traffic (harden-host.sh --restrict-outbound).
systemctl start graphrelay-egress.timer
systemctl start graphrelay-egress.service

[ "$START" = 1 ] || exit 0
if grep -qE '^[A-Z_]+=\s*$' /etc/graphrelay.env; then
  echo "Installed. /etc/graphrelay.env still has empty values, so graphrelay was not started."
  echo "Fill it in, then: systemctl start graphrelay && journalctl -u graphrelay -f"
else
  systemctl restart graphrelay
  sleep 3
  systemctl --no-pager --lines=15 status graphrelay || true
fi
