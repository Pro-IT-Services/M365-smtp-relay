#!/bin/bash
# Update a Graph Relay Docker installation made with docker/setup.sh.
# Installed as graphrelay-docker-update.
#
#   graphrelay-docker-update --check          report installed and newest version; exit 10 if an
#                                             update is available, 0 if not
#   graphrelay-docker-update                  update to the newest release, if there is one
#   graphrelay-docker-update --version v1.2.3 install exactly that release
#   graphrelay-docker-update --archive FILE   install from a downloaded archive
#   bash docker/update.sh                     in an extracted release archive: install that release
#   options: --dir DIR (installation directory, default /opt/graphrelay-docker),
#            --force (reinstall or downgrade), --keep N (backups to keep, default 5)
#
# An update always runs the same steps, taken from the NEW release:
#   1 preflight   2 build the new image   3 validate the config with it   4 back up files
#   5 stop the container   6 back up the database   7 install the new files
#   8 start (applies database migrations)   9 health check   10 clean up
# If any step from 5 on fails, the previous image, files and database are restored and the old
# version is started again.
set -Eeuo pipefail # -E: the rollback trap also fires for failures inside functions

SELF=$(readlink -f "$0")
HERE=$(cd "$(dirname "$SELF")" && pwd)

MODE=update TARGET="" ARCHIVE="" FORCE=0 KEEP=5 APPLY_DIR="" DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check; shift ;;
    --version) TARGET=${2:?--version needs a tag such as v1.2.3}; shift 2 ;;
    --archive) ARCHIVE=${2:?--archive needs a file}; shift 2 ;;
    --dir) DIR=${2:?--dir needs a path}; shift 2 ;;
    --force) FORCE=1; shift ;;
    --keep) KEEP=${2:?--keep needs a number}; shift 2 ;;
    --apply-from) APPLY_DIR=${2:?}; shift 2 ;; # internal: the fetched release hands over to its own script
    -h | --help) sed -n '2,19p' "$SELF" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $1 (try --help)" >&2; exit 1 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

# Where is the installation? The installed copy lives in <dir>/bin.
if [ -z "$DIR" ]; then
  if [ -e "$HERE/../compose.yaml" ] && [ -e "$HERE/../.env" ]; then DIR=$(cd "$HERE/.." && pwd); else DIR=/opt/graphrelay-docker; fi
fi
installed_version() { sed -n 's/^GRAPHRELAY_VERSION=//p' "$DIR/.env" 2>/dev/null | head -n 1; }

# ---------------------------------------------------------------------------------------------
# Part 1: find the release, then hand over to the update.sh inside it.
# ---------------------------------------------------------------------------------------------
if [ -z "$APPLY_DIR" ] && [ -x "$HERE/../graphrelay" ] && [ -e "$HERE/Dockerfile" ] && [ -e "$HERE/setup.sh" ] && [ -z "$ARCHIVE" ] && [ "$MODE" = update ]; then
  APPLY_DIR=$(cd "$HERE/.." && pwd) # started inside an extracted archive
fi

if [ -z "$APPLY_DIR" ]; then
  [ -r "$HERE/release-lib.sh" ] || die "release-lib.sh is missing next to $SELF"
  # shellcheck disable=SC1091
  . "$HERE/release-lib.sh"
  current=$(installed_version)
  [ -n "$current" ] || die "no installation found in $DIR (use docker/setup.sh, or pass --dir)"

  if [ -n "$ARCHIVE" ]; then
    [ -r "$ARCHIVE" ] || die "cannot read $ARCHIVE"
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
      if gr_newer "$TARGET" "$current"; then echo "an update is available: run graphrelay-docker-update"; exit 10; fi
      echo "up to date"
      exit 0
    fi
    if [ "$FORCE" != 1 ] && ! gr_newer "$TARGET" "$current"; then
      echo "Graph Relay $current is up to date (newest release: $TARGET). Use --force to reinstall."
      exit 0
    fi
    stage=$(mktemp -d /var/tmp/graphrelay-update.XXXXXX)
    echo "== Downloading $TARGET from $REPO"
    dir=$(gr_download "$TARGET" "$stage")
  fi
  [ -e "$dir/docker/update.sh" ] || die "this release has no Docker update script"
  GR_STAGE=$stage exec bash "$dir/docker/update.sh" --apply-from "$dir" --dir "$DIR" --keep "$KEEP" $([ "$FORCE" = 1 ] && echo --force)
fi

# ---------------------------------------------------------------------------------------------
# Part 2: apply the release in $APPLY_DIR.
# ---------------------------------------------------------------------------------------------
SRC=$APPLY_DIR
[ -x "$SRC/graphrelay" ] && [ -e "$SRC/docker/Dockerfile" ] || die "$SRC is not an extracted Graph Relay release"
[ -e "$DIR/compose.yaml" ] && [ -e "$DIR/.env" ] || die "no installation in $DIR; use docker/setup.sh for a first installation"
command -v docker >/dev/null || die "Docker is not installed"
docker compose version >/dev/null 2>&1 || die "the Docker compose plugin is not installed"

exec 9> "$DIR/.update.lock"
flock -n 9 || die "another update is running"

TOTAL=10
N=0
step() { N=$((N + 1)); echo; echo "[$N/$TOTAL] $*"; }
compose() { (cd "$DIR" && docker compose "$@"); }

OLD=$(installed_version)
NEW=$("$SRC/graphrelay" -version)
BACKUP=$DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)-$OLD
STOPPED=0

rollback() {
  local rc=$?
  trap - ERR
  set +e
  echo
  echo "!! The update to $NEW failed (exit $rc). Restoring $OLD from $BACKUP"
  compose stop >/dev/null 2>&1
  for f in .env compose.yaml config.yaml graphrelay.env; do
    if [ -e "$BACKUP/files/$f" ]; then cp -a "$BACKUP/files/$f" "$DIR/$f"; fi
  done
  if [ -d "$BACKUP/files/bin" ]; then cp -a "$BACKUP/files/bin/." "$DIR/bin/"; fi
  if [ -e "$BACKUP/data/.complete" ]; then # never restore from a half-written backup
    rm -f "$DIR"/data/relay.db "$DIR"/data/relay.db-wal "$DIR"/data/relay.db-shm
    # File by file: copying the directory itself would give data/ the backup directory's owner
    # and mode, and the container could no longer open its database.
    for f in "$BACKUP"/data/*; do
      if [ -e "$f" ]; then cp -a "$f" "$DIR/data/"; fi
    done
  fi
  if [ "$STOPPED" = 1 ]; then
    compose up -d >/dev/null 2>&1
    sleep 5
  fi
  if [ "$(docker inspect -f '{{.State.Running}}' graphrelay 2>/dev/null)" = true ]; then
    echo "!! Rolled back: Graph Relay $(installed_version) is running again."
  else
    echo "!! Rollback finished but the container is not running. Check: cd $DIR && docker compose logs --tail 50"
  fi
  echo "!! The failed update's backup was kept in $BACKUP"
  exit 1
}

healthy() {
  local i started restarts
  for i in $(seq 1 30); do
    if [ "$(docker inspect -f '{{.State.Running}}' graphrelay 2>/dev/null)" = true ]; then
      sleep 6 # still up, and not restarting, after a moment?
      restarts=$(docker inspect -f '{{.RestartCount}}' graphrelay 2>/dev/null || echo 1)
      if [ "$(docker inspect -f '{{.State.Running}}' graphrelay 2>/dev/null)" = true ] && [ "$restarts" = 0 ] \
        && ! compose logs --no-color 2>/dev/null | grep -q 'level=ERROR msg=fatal' \
        && compose logs --no-color 2>/dev/null | grep -q 'listening'; then
        return 0
      fi
    fi
    if compose logs --no-color 2>/dev/null | grep -q 'level=ERROR msg=fatal'; then
      echo "the relay stopped with a fatal error:"
      compose logs --no-color --tail 20 | grep 'level=ERROR' | tail -n 3
      return 1
    fi
    sleep 2
  done
  echo "the container did not come up within the expected time:"
  compose logs --no-color --tail 8
  return 1
}

echo "== Updating Graph Relay (Docker) in $DIR: $OLD -> $NEW"

step "Preflight"
if [ "$OLD" = "$NEW" ] && [ "$FORCE" != 1 ]; then
  echo "$NEW is already installed. Use --force to reinstall."
  exit 0
fi
free_kb=$(df -Pk "$DIR" | awk 'NR==2 {print $4}')
[ "$free_kb" -gt 204800 ] || die "less than 200 MB free under $DIR"
echo "installed $OLD, installing $NEW; backup goes to $BACKUP"

step "Build the new image"
ctx=$(mktemp -d)
cp "$SRC/graphrelay" "$ctx/graphrelay"
docker build --pull -q -t "graphrelay:$NEW" -f "$SRC/docker/Dockerfile" "$ctx"
rm -rf "$ctx"
echo "built graphrelay:$NEW"

step "Validate the configuration with the new image"
docker run --rm --env-file "$DIR/graphrelay.env" -v "$DIR/config.yaml:/etc/graphrelay/config.yaml:ro" \
  "graphrelay:$NEW" -check-config -config /etc/graphrelay/config.yaml \
  || die "the new release does not accept the current configuration; nothing was changed. Fix $DIR/config.yaml or $DIR/graphrelay.env and run the update again"

step "Back up the installed files"
install -d -m 0700 "$BACKUP/files"
for f in .env compose.yaml config.yaml graphrelay.env; do cp -a "$DIR/$f" "$BACKUP/files/$f"; done
cp -a "$DIR/bin" "$BACKUP/files/bin"
echo "$OLD" > "$BACKUP/VERSION"
echo "saved to $BACKUP/files"

# From here on a failure restores the previous state.
trap rollback ERR

step "Stop the container"
compose stop
STOPPED=1
echo "stopped (clients get a connection error and retry)"

step "Back up the database"
install -d -m 0700 "$BACKUP/data"
for f in "$DIR"/data/relay.db "$DIR"/data/relay.db-wal "$DIR"/data/relay.db-shm; do
  if [ -e "$f" ]; then cp -a "$f" "$BACKUP/data/"; fi
done
: > "$BACKUP/data/.complete"
ls "$BACKUP/data" 2>/dev/null | sed 's/^/saved /'

step "Install the new files"
install -m 0755 "$SRC/docker/update.sh" "$DIR/bin/graphrelay-docker-update"
install -m 0644 "$SRC/release-lib.sh" "$DIR/bin/release-lib.sh"
install -m 0644 "$SRC/docker/Dockerfile" "$DIR/bin/Dockerfile"
# compose.yaml may carry your own changes (a certificate mount, other ports): it is kept, and the
# new release's version is put next to it for comparison when it differs.
if ! cmp -s "$SRC/docker/compose.yaml" "$DIR/compose.yaml"; then
  install -m 0644 "$SRC/docker/compose.yaml" "$DIR/compose.yaml.new"
  echo "note: compose.yaml differs from this release's; yours was kept, the new one is $DIR/compose.yaml.new"
else
  rm -f "$DIR/compose.yaml.new"
fi
echo "GRAPHRELAY_VERSION=$NEW" > "$DIR/.env"

step "Start the container (database migrations run now)"
compose up -d

step "Health check"
healthy
trap - ERR
echo "Graph Relay $NEW is running"
compose logs --no-color --tail 50 | grep -E 'level=(WARN|ERROR)' | tail -n 5 || true

step "Clean up"
ls -1d "$DIR"/backups/*/ 2>/dev/null | sort | head -n -"$KEEP" | while read -r old; do
  rm -rf "$old"
  echo "removed old backup $old"
done || true
# Keep the new image and the previous one (for a manual rollback); remove older ones.
docker images --format '{{.Tag}}' graphrelay 2>/dev/null | while read -r tag; do
  if [ "$tag" != "$NEW" ] && [ "$tag" != "$OLD" ]; then
    docker image rm "graphrelay:$tag" >/dev/null 2>&1 && echo "removed old image graphrelay:$tag"
  fi
done || true
if [ -n "${GR_STAGE:-}" ] && [ -d "$GR_STAGE" ]; then
  rm -rf "$GR_STAGE"
fi

echo
echo "== Updated Graph Relay $OLD -> $NEW. Backup of the previous version: $BACKUP"
