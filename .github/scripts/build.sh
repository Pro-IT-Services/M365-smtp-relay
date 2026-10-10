#!/bin/bash
# Test and build Graph Relay, and package the release archives into $DIST (default: dist/).
# Meant to run as root inside a throwaway Debian container (see .github/workflows/build.yml):
# it installs the newest stable Go itself, so nothing has to be preinstalled on the runner.
#
#   VERSION     version string baked into the binary and the archive names (required)
#   DIST        output directory. In the workflow the source is mounted read-only and this is a
#               separate mount, so the build cannot change the checked-out source or scripts.
#   HOST_UID/HOST_GID  owner to give the output files when done
#   SKIP_SETUP=1       use the Go already on PATH instead of installing it (local runs)
set -euo pipefail
: "${VERSION:?VERSION is required}"
SRC=$(cd "$(dirname "$0")/../.." && pwd)
cd "$SRC"
DIST=${DIST:-$SRC/dist}
mkdir -p "$DIST"
DIST=$(cd "$DIST" && pwd)

# The output directory is mounted from the runner. Whatever root creates in it must be handed
# back to the runner's user, or the runner could not clean up after the job.
trap 'chown -R "${HOST_UID:-0}:${HOST_GID:-0}" "$DIST" 2>/dev/null || true' EXIT

if [ "${SKIP_SETUP:-0}" != 1 ]; then
  echo "::group::Install tools and the newest stable Go"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q
  apt-get install -y -q --no-install-recommends ca-certificates curl git jq
  case "$(dpkg --print-architecture)" in
    amd64) goarch=amd64 ;;
    arm64) goarch=arm64 ;;
    *) echo "unsupported build architecture $(dpkg --print-architecture)" >&2; exit 1 ;;
  esac
  # go.dev lists releases newest first; take the newest stable one and its published checksum.
  meta=$(curl -fsSL 'https://go.dev/dl/?mode=json')
  go_version=$(jq -r '[.[] | select(.stable)][0].version' <<<"$meta")
  file="$go_version.linux-$goarch.tar.gz"
  sha=$(jq -r --arg f "$file" '[.[] | select(.stable)][0].files[] | select(.filename == $f) | .sha256' <<<"$meta")
  [ -n "$sha" ] || { echo "no checksum published for $file" >&2; exit 1; }
  curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/$file"
  echo "$sha  /tmp/go.tar.gz" | sha256sum -c -
  tar -C /usr/local -xzf /tmp/go.tar.gz
  rm -f /tmp/go.tar.gz
  export PATH=/usr/local/go/bin:$PATH GOPATH=/tmp/gopath GOCACHE=/tmp/gocache
  echo "::endgroup::"
fi
# Build with exactly the Go installed above, and don't read VCS state from the mounted checkout.
export GOTOOLCHAIN=local GOFLAGS=-buildvcs=false CGO_ENABLED=0
go version

echo "::group::Download and verify dependencies"
go mod download
go mod verify
echo "::endgroup::"

echo "::group::Check formatting, vet, test"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "not gofmt-formatted:" >&2; echo "$unformatted" >&2; exit 1
fi
go vet ./...
go test ./... -count=1
# Changes that installed servers don't pick up by themselves need a decision about a migration.
bash deploy/migrations/guard.sh
for f in deploy/*.sh deploy/migrations/*.sh deploy/docker/*.sh deploy/graphrelay-fw-sync deploy/graphrelay-fw-report deploy/graphrelay-egress-sync; do
  bash -n "$f"
done
echo "::endgroup::"

echo "::group::Build and package $VERSION"
find "$DIST" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
for arch in amd64 arm64; do
  name="graphrelay_${VERSION}_linux_${arch}"
  out="$DIST/$name"
  mkdir -p "$out/docs" "$out/migrations" "$out/docker"
  GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/graphrelay" ./cmd/graphrelay
  # Everything setup.sh and update.sh need, so the archive installs and updates a server that has
  # neither Go nor the repository.
  for f in deploy/setup.sh deploy/update.sh deploy/install.sh deploy/release-lib.sh deploy/harden-host.sh \
           deploy/graphrelay.service deploy/graphrelay.env.example \
           deploy/graphrelay-fw-sync deploy/graphrelay-fw.service deploy/graphrelay-fw.path \
           deploy/graphrelay-fw-report deploy/graphrelay-fw-report.service deploy/graphrelay-fw-report.timer \
           deploy/graphrelay-egress-sync deploy/graphrelay-egress.service deploy/graphrelay-egress.timer \
           config.example.yaml README.md; do
    cp "$f" "$out/"
  done
  cp deploy/migrations/[0-9]*.sh deploy/migrations/README.md "$out/migrations/"
  cp deploy/docker/Dockerfile deploy/docker/compose.yaml deploy/docker/setup.sh deploy/docker/update.sh "$out/docker/"
  cp docs/*.md "$out/docs/"
  # Which release this is and where it came from; setup.sh uses REPO as the update source.
  printf 'VERSION=%s\nREPO=%s\n' "$VERSION" "${GITHUB_REPOSITORY:-}" > "$out/RELEASE"
  for f in graphrelay setup.sh update.sh install.sh harden-host.sh graphrelay-fw-sync graphrelay-fw-report graphrelay-egress-sync \
           docker/setup.sh docker/update.sh; do
    chmod 0755 "$out/$f"
  done
  chmod 0755 "$out"/migrations/*.sh
  tar -C "$DIST" --owner=0 --group=0 --numeric-owner -czf "$DIST/$name.tar.gz" "$name"
  rm -rf "$out"
done
(cd "$DIST" && sha256sum graphrelay_*.tar.gz > SHA256SUMS && cat SHA256SUMS)
echo "::endgroup::"
