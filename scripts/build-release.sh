#!/usr/bin/env bash
# Builds the release archives of fam and their checksums.
# Usage: scripts/build-release.sh <version> [out-dir]   (version without the "v", for example 0.2.0)
# The release workflow runs it for each tag v*. It also works locally.
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: build-release.sh <version> [out-dir]}
version=${version#v}
out=${2:-dist}

# The platforms with a pinned pkl binary (internal/pklbin). macOS on Intel has none.
platforms="linux/amd64 linux/arm64 darwin/arm64"

rm -rf "$out"
mkdir -p "$out"
for p in $platforms; do
  os=${p%/*}
  arch=${p#*/}
  work=$(mktemp -d)
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X github.com/gregor-tokarev/fam/internal/version.Version=$version" \
    -o "$work/fam" ./cmd/fam
  tar -C "$work" -czf "$out/fam_${os}_${arch}.tar.gz" fam
  rm -rf "$work"
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum fam_*.tar.gz > checksums.txt
else
  shasum -a 256 fam_*.tar.gz > checksums.txt
fi
cat checksums.txt
