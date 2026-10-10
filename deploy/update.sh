#!/bin/bash
# Update Graph Relay on a host installed with setup.sh. Installed as graphrelay-update.
#
#   graphrelay-update --check          report the installed and the newest version; exit 10 if an
#                                      update is available, 0 if not
#   graphrelay-update                  update to the newest release, if there is one
#   graphrelay-update --version v1.2.3 install exactly that release
#   graphrelay-update --archive FILE   install from a downloaded archive (no network needed);
#                                      SHA256SUMS next to it is checked if present
#   bash update.sh                     in an extracted release archive: install that release
#   options: --force (reinstall or downgrade), --keep N (backups to keep, default 5)
#
# An update always runs the same steps, taken from the NEW release so that everything the new
# version needs is included:
#   1 preflight        2 validate the config against the new binary     3 back up files
#   4 stop the relay   5 back up the database                           6 host migrations
#   7 install files    8 start (applies database migrations)            9 health check
#   10 clean up
# If any step from 4 on fails, the previous binary, files and database are restored and the old
# version is started again.
set -Eeuo pipefail # -E: the rollback trap also fires for failures inside functions

PREFIX=/opt/graphrelay
ENVFILE=/etc/graphrelay.env
STATE=$PREFIX/host-migrations.applied
LOCK=/run/graphrelay-update.lock
SELF=$(readlink -f "$0")
HERE=$(cd "$(dirname "$SELF")" && pwd)

MODE=update TARGET="" ARCHIVE="" FORCE=0 KEEP=5 APPLY_DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check; shift ;;
    --version) TARGET=${2:?--version needs a tag such as v1.2.3}; shift 2 ;;
    --archive) ARCHIVE=${2:?--archive needs a file}; shift 2 ;;
    --force) FORCE=1; shift ;;
    --keep) KEEP=${2:?--keep needs a number}; shift 2 ;;
    --apply-from) APPLY_DIR=${2:?}; shift 2 ;; # internal: the fetched release hands over to its own script
    -h | --help) sed -n '2,20p' "$SELF" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $1 (try --help)" >&2; exit 1 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }
installed_version() { "$PREFIX/graphrelay" -version 2>/dev/null || echo none; }

# ---------------------------------------------------------------------------------------------
# Part 1: find the release. Runs from the installed copy; ends by handing over to the update.sh
# inside the new release, so the steps below are always the new release's own.
# ---------------------------------------------------------------------------------------------
if [ -z "$APPLY_DIR" ] && [ -x "$HERE/graphrelay" ] && [ -e "$HERE/install.sh" ] && [ -z "$ARCHIVE" ] && [ "$MODE" = update ]; then
  APPLY_DIR=$HERE # started inside an extracted archive
fi

if [ -z "$APPLY_DIR" ]; then
  [ -r "$HERE/release-lib.sh" ] || die "release-lib.sh is missing next to $SELF"
  # shellcheck disable=SC1091
  . "$HERE/release-lib.sh"
  current=$(installed_version)

  if [ -n "$ARCHIVE" ]; then
    [ -r "$ARCHIVE" ] || die "cannot read $ARCHIVE"
    [ "$(id -u)" = 0 ] || die "run as root"
    stage=$(mktemp -d /var/tmp/graphrelay-update.XXXXXX)
    cp "$ARCHIVE" "$stage/"
    sums=$(dirname "$ARCHIVE")/SHA256SUMS
    if [ -r "$sums" ]; then
      cp "$sums" "$stage/"
      (cd "$stage" && sha256sum -c --ignore-missing SHA256SUMS) || die "checksum of $(basename "$ARCHIVE") does not match SHA256SUMS"
    else
      echo "warning: no SHA256SUMS next to the archive, its checksum was not verified" >&2
    fi
    tar -C "$stage" -xzf "$stage/$(basename "$ARCHIVE")"
    dir=$(find "$stage" -mindepth 1 -maxdepth 1 -type d | head -n 1)
    [ -x "$dir/graphrelay" ] || die "$ARCHIVE does not look like a Graph Relay release"
  else
    gr_load_conf
    if [ -z "$TARGET" ]; then
      TARGET=$(gr_tag_of "$(gr_release_json latest)")
      [ -n "$TARGET" ] || die "could not determine the newest release of $REPO"
    fi
    if [ "$MODE" = check ]; then
      echo "installed: $current"
      echo "newest:    $TARGET  ($REPO)"
      if gr_newer "$TARGET" "$current"; then echo "an update is available: run graphrelay-update"; exit 10; fi
      echo "up to date"
      exit 0
    fi
    if [ "$FORCE" != 1 ] && ! gr_newer "$TARGET" "$current"; then
      echo "Graph Relay $current is up to date (newest release: $TARGET). Use --force to reinstall."
      exit 0
    fi
    [ "$(id -u)" = 0 ] || die "run as root"
    stage=$(mktemp -d /var/tmp/graphrelay-update.XXXXXX)
    echo "== Downloading $TARGET from $REPO"
    dir=$(gr_download "$TARGET" "$stage")
  fi
  [ -e "$dir/update.sh" ] || die "this release predates the update script: install it by hand with 'bash install.sh' in $dir"
  # Hand over to the new release's own update script.
  GR_STAGE=$stage exec bash "$dir/update.sh" --apply-from "$dir" --keep "$KEEP" $([ "$FORCE" = 1 ] && echo --force)
fi

# ---------------------------------------------------------------------------------------------
# Part 2: apply the release in $APPLY_DIR.
# ---------------------------------------------------------------------------------------------
[ "$(id -u)" = 0 ] || die "run as root"
SRC=$APPLY_DIR
[ -x "$SRC/graphrelay" ] && [ -e "$SRC/install.sh" ] || die "$SRC is not an extracted Graph Relay release"
[ -x "$PREFIX/graphrelay" ] || die "Graph Relay is not installed here; use setup.sh for a first installation"

exec 9> "$LOCK"
flock -n 9 || die "another update is running"

TOTAL=10
N=0
step() { N=$((N + 1)); echo; echo "[$N/$TOTAL] $*"; }

OLD=$(installed_version)
NEW=$("$SRC/graphrelay" -version)
BACKUP=$PREFIX/backups/$(date -u +%Y%m%dT%H%M%SZ)-$OLD
STOPPED=0

# Everything an update can change, so that a rollback restores exactly the previous state.
FILES="
$PREFIX/graphrelay
$PREFIX/config.yaml
$PREFIX/RELEASE
$PREFIX/host-migrations.applied
$PREFIX/bin/graphrelay-update
$PREFIX/bin/release-lib.sh
$PREFIX/bin/harden-host.sh
$ENVFILE
/etc/graphrelay/update.conf
/etc/nftables.conf
/usr/local/sbin/graphrelay-fw-sync
/usr/local/sbin/graphrelay-fw-report
/usr/local/sbin/graphrelay-egress-sync
/etc/systemd/system/graphrelay.service
/etc/systemd/system/graphrelay-fw.service
/etc/systemd/system/graphrelay-fw.path
/etc/systemd/system/graphrelay-fw-report.service
/etc/systemd/system/graphrelay-fw-report.timer
/etc/systemd/system/graphrelay-egress.service
/etc/systemd/system/graphrelay-egress.timer
"

backup_files() {
  install -d -m 0700 "$BACKUP/files"
  for f in $FILES; do
    if [ -e "$f" ]; then
      install -d "$BACKUP/files$(dirname "$f")"
      cp -a "$f" "$BACKUP/files$f"
    fi
  done
  echo "$OLD" > "$BACKUP/VERSION"
}

backup_database() {
  install -d -m 0700 "$BACKUP/data"
  # The relay is stopped, so these files are consistent. Certificates are not touched by updates.
  for f in "$PREFIX"/data/relay.db "$PREFIX"/data/relay.db-wal "$PREFIX"/data/relay.db-shm "$PREFIX"/data/firewall-allowlist.txt; do
    if [ -e "$f" ]; then cp -a "$f" "$BACKUP/data/"; fi
  done
  : > "$BACKUP/data/.complete"
}

rollback() {
  local rc=$?
  trap - ERR
  set +e
  echo
  echo "!! The update to $NEW failed (exit $rc). Restoring $OLD from $BACKUP"
  systemctl stop graphrelay 2>/dev/null
  local fw_changed=0
  if [ -e "$BACKUP/files/etc/nftables.conf" ] && ! cmp -s "$BACKUP/files/etc/nftables.conf" /etc/nftables.conf 2>/dev/null; then
    fw_changed=1
  fi
  for f in $FILES; do
    if [ -e "$BACKUP/files$f" ]; then
      cp -a "$BACKUP/files$f" "$f"
    fi
  done
  if [ -e "$BACKUP/data/.complete" ]; then # never restore from a half-written backup
    rm -f "$PREFIX"/data/relay.db "$PREFIX"/data/relay.db-wal "$PREFIX"/data/relay.db-shm
    # File by file: copying the directory itself would give data/ the backup directory's owner
    # and mode, and the relay could no longer open its database.
    for f in "$BACKUP"/data/*; do
      if [ -e "$f" ]; then cp -a "$f" "$PREFIX/data/"; fi
    done
  fi
  systemctl daemon-reload
  # The loop above put the previous ruleset back; load it only if a migration had changed it.
  if [ "$fw_changed" = 1 ] && systemctl is-active -q nftables 2>/dev/null; then
    nft -f /etc/nftables.conf 2>/dev/null && /usr/local/sbin/graphrelay-fw-sync 2>/dev/null
  fi
  if [ "$STOPPED" = 1 ]; then
    systemctl start graphrelay
    sleep 5
  fi
  if systemctl is-active -q graphrelay; then
    echo "!! Rolled back: Graph Relay $(installed_version) is running again."
  else
    echo "!! Rollback finished but the relay is not running. Check: journalctl -u graphrelay -n 50"
  fi
  echo "!! The failed update's backup was kept in $BACKUP"
  exit 1
}

healthy() {
  # Running and stable, the expected version, listening, and no fatal error since the start.
  local since=$1 i
  for i in $(seq 1 30); do
    if ! systemctl is-active -q graphrelay; then
      # It may be in its restart delay after a crash.
      if journalctl -u graphrelay --since "$since" --no-pager -o cat 2>/dev/null | grep -q 'level=ERROR msg=fatal'; then
        echo "the relay stopped with a fatal error:"
        journalctl -u graphrelay --since "$since" --no-pager -o cat | grep 'level=ERROR' | tail -n 3
        return 1
      fi
      sleep 2
      continue
    fi
    if ss -ltnp 2>/dev/null | grep -q '"graphrelay"'; then
      sleep 5 # still up after a moment?
      if systemctl is-active -q graphrelay && [ "$(installed_version)" = "$NEW" ]; then
        return 0
      fi
    fi
    sleep 2
  done
  echo "the relay did not come up within the expected time:"
  journalctl -u graphrelay --since "$since" --no-pager -o cat | tail -n 8
  return 1
}

echo "== Updating Graph Relay: $OLD -> $NEW"

step "Preflight"
for tool in systemctl ss flock cmp; do
  command -v "$tool" >/dev/null || die "missing required tool: $tool"
done
if [ "$OLD" = "$NEW" ] && [ "$FORCE" != 1 ]; then
  echo "$NEW is already installed. Use --force to reinstall."
  exit 0
fi
free_kb=$(df -Pk "$PREFIX" | awk 'NR==2 {print $4}')
[ "$free_kb" -gt 102400 ] || die "less than 100 MB free under $PREFIX"
echo "installed $OLD, installing $NEW; backup goes to $BACKUP"

step "Validate the configuration with the new binary"
(
  set -a
  # shellcheck disable=SC1090
  . "$ENVFILE"
  set +a
  "$SRC/graphrelay" -check-config -config "$PREFIX/config.yaml"
) || die "the new release does not accept the current configuration; nothing was changed. Fix $PREFIX/config.yaml or $ENVFILE and run the update again"

step "Back up the installed files"
backup_files
echo "saved to $BACKUP/files"

# From here on a failure restores the previous state.
trap rollback ERR

step "Stop the relay"
systemctl stop graphrelay
STOPPED=1
echo "stopped (clients get a connection error and retry)"

step "Back up the database"
backup_database
ls "$BACKUP/data" 2>/dev/null | sed 's/^/saved /'

step "Host migrations"
touch "$STATE"
ran=0
for m in "$SRC"/migrations/[0-9]*.sh; do
  [ -e "$m" ] || continue
  id=$(basename "$m" .sh)
  if grep -qxF "$id" "$STATE"; then continue; fi
  echo "-- $id"
  bash "$m"
  echo "$id" >> "$STATE"
  ran=$((ran + 1))
done
echo "$ran applied, $(wc -l < "$STATE") recorded in $STATE"

step "Install the new files"
bash "$SRC/install.sh" --no-start
echo "binary, units, helpers and update tooling installed"

step "Start the relay (database migrations run now)"
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl start graphrelay

step "Health check"
healthy "$since"
trap - ERR
echo "Graph Relay $(installed_version) is running"
journalctl -u graphrelay --since "$since" --no-pager -o cat | grep -E 'level=(WARN|ERROR)' | tail -n 5 || true

step "Clean up"
# Keep the newest backups only.
ls -1d "$PREFIX"/backups/*/ 2>/dev/null | sort | head -n -"$KEEP" | while read -r old; do
  rm -rf "$old"
  echo "removed old backup $old"
done || true
if [ -n "${GR_STAGE:-}" ] && [ -d "$GR_STAGE" ]; then
  rm -rf "$GR_STAGE"
fi

echo
echo "== Updated Graph Relay $OLD -> $NEW. Backup of the previous version: $BACKUP"
