#!/bin/bash
# Test and build Graph Relay, and package the release archives into dist/.
# Meant to run as root inside a throwaway Debian container (see .github/workflows/build.yml):
# it installs the newest stable Go itself, so nothing has to be preinstalled on the runner.
#
#   VERSION     version string baked into the binary and the archive names (required)
#   HOST_UID/HOST_GID  owner to give the files in the mounted workspace when done
#   SKIP_SETUP=1       use the Go already on PATH instead of installing it (local runs)
set -euo pipefail
: "${VERSION:?VERSION is required}"
SRC=$(cd "$(dirname "$0")/../.." && pwd)
cd "$SRC"

# The workspace is mounted from the runner. Whatever root creates in it must be handed back to
# the runner's user, or the runner could not clean up after the job.
trap 'chown -R "${HOST_UID:-0}:${HOST_GID:-0}" "$SRC" 2>/dev/null || true' EXIT

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
echo "::endgroup::"

echo "::group::Build and package $VERSION"
rm -rf dist
mkdir -p dist
for arch in amd64 arm64; do
  name="graphrelay_${VERSION}_linux_${arch}"
  mkdir -p "dist/$name/docs"
  GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "dist/$name/graphrelay" ./cmd/graphrelay
  # Everything install.sh needs, so the archive installs on a server without the repository.
  cp deploy/install.sh deploy/harden-host.sh deploy/graphrelay.service deploy/graphrelay.env.example \
     deploy/graphrelay-fw-sync deploy/graphrelay-fw.service deploy/graphrelay-fw.path \
     deploy/graphrelay-fw-report deploy/graphrelay-fw-report.service deploy/graphrelay-fw-report.timer \
     config.example.yaml README.md "dist/$name/"
  cp docs/*.md "dist/$name/docs/"
  chmod 0755 "dist/$name/graphrelay" "dist/$name/install.sh" "dist/$name/harden-host.sh" \
             "dist/$name/graphrelay-fw-sync" "dist/$name/graphrelay-fw-report"
  tar -C dist --owner=0 --group=0 --numeric-owner -czf "dist/$name.tar.gz" "$name"
  rm -rf "dist/$name"
done
(cd dist && sha256sum graphrelay_*.tar.gz > SHA256SUMS && cat SHA256SUMS)
echo "::endgroup::"
