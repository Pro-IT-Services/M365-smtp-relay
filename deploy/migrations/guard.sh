#!/bin/bash
# Tripwire for changes that existing servers do not pick up by themselves.
#
#   guard.sh            check: fail if a guarded file differs from the recorded checksum
#   guard.sh --update   record the current checksums, after deciding about a migration
#
# The guarded files are the ones an update does not simply overwrite on a server:
#   deploy/harden-host.sh                  writes /etc/nftables.conf and the sshd settings once
#   config.example.yaml                    the shape of config.yaml, which the server keeps
#   internal/store/testdata/schema.golden  the database schema
# When one of them changes, somebody has to decide whether servers that are already installed
# need a host migration (deploy/migrations/NNNN-*.sh) or a database migration. This check makes
# sure that decision is not skipped. The build runs it.
set -euo pipefail
cd "$(dirname "$0")/../.."

GUARD=deploy/migrations/GUARD
FILES="deploy/harden-host.sh config.example.yaml internal/store/testdata/schema.golden"

sums() {
  local f
  for f in $FILES; do
    # Normalise line endings, so a checkout with CRLF gives the same checksum.
    printf '%s  %s\n' "$(tr -d '\r' < "$f" | sha256sum | cut -d' ' -f1)" "$f"
  done
}

if [ "${1:-}" = "--update" ]; then
  {
    echo "# Checksums of files whose changes may need a migration on existing servers."
    echo "# Do not edit by hand. See deploy/migrations/README.md, then run: bash deploy/migrations/guard.sh --update"
    sums
  } > "$GUARD"
  echo "recorded in $GUARD"
  exit 0
fi

if [ "$(sums)" = "$(grep -v '^#' "$GUARD")" ]; then
  echo "migration guard: no guarded file changed"
  exit 0
fi

echo "migration guard: guarded files changed since the last recorded decision:" >&2
diff <(grep -v '^#' "$GUARD") <(sums) | sed -n 's/^> [0-9a-f]*  /  /p' >&2
cat >&2 <<'EOF'

Servers that are already installed do not receive these changes automatically. Decide:
  - deploy/harden-host.sh changed (firewall, sshd): do existing servers need the change?
    If yes, add deploy/migrations/NNNN-<what>.sh that applies it.
  - config.example.yaml changed: is a new key required, or was one renamed or removed?
    If an existing config.yaml would stop working or behave differently, add a host migration
    that edits it (or make the new key optional with a safe default).
  - schema.golden changed: internal/store/migrations.go must have a new migration; the store
    tests check that.
Then record that you decided:  bash deploy/migrations/guard.sh --update
EOF
exit 1
