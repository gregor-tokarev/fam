#!/bin/sh
# Installs fam from the GitHub releases of gregor-tokarev/fam.
#
#   curl -fsSL https://raw.githubusercontent.com/gregor-tokarev/fam/main/install.sh | sh
#
# FAM_VERSION=0.2.0   install this version (default: the latest release)
# FAM_INSTALL_DIR=... install into this folder (default: ~/.local/bin)
# FAM_PKL=0           do not install pkl
#
# When no pkl is in PATH, the script also installs the pkl that fam uses (the Pkl plugins of
# editors need it).
#
# Run it again to update fam, or run `fam update`.
set -eu

repo=gregor-tokarev/fam
dir=${FAM_INSTALL_DIR:-$HOME/.local/bin}
version=${FAM_VERSION:-latest}

fail() {
  echo "fam install: $*" >&2
  exit 1
}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "$(uname -s) is not supported. fam runs on Linux and macOS" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "the CPU $(uname -m) is not supported. fam runs on x86-64 and arm64" ;;
esac
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
  fail "fam needs a Mac with Apple Silicon: pkl, which fam uses to read the config, has no build for Intel Macs"
fi

if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/v${version#v}"
fi
asset="fam_${os}_${arch}.tar.gz"

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "sha256sum or shasum is necessary to check the download"
fi

new=
tmp=$(mktemp -d "${TMPDIR:-/tmp}/fam-install.XXXXXX")
trap 'rm -rf "$tmp"; [ -z "$new" ] || rm -f "$new"' EXIT
trap 'exit 1' INT TERM

echo "Downloading $asset ($version)"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || fail "download of $base/$asset failed"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "download of $base/checksums.txt failed"
want=$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "checksums.txt has no entry for $asset"
got=$(sha "$tmp/$asset")
[ "$got" = "$want" ] || fail "the SHA-256 of $asset is $got, but checksums.txt says $want"

tar -xzf "$tmp/$asset" -C "$tmp" fam
mkdir -p "$dir"
# Write a new file next to the target, then rename: a running fam keeps working during the
# update. mktemp creates the file with a random name, so it never writes through an existing
# file or link.
new=$(mktemp "$dir/.fam.new.XXXXXX") || fail "cannot create a file in $dir"
cat "$tmp/fam" > "$new"
chmod 755 "$new"
mv -f "$new" "$dir/fam"
new=

echo "Installed $("$dir/fam" version) to $dir/fam"
if [ "${FAM_PKL:-1}" != 0 ] && ! command -v pkl >/dev/null 2>&1; then
  "$dir/fam" internal install-pkl "$dir" ||
    echo "fam install: pkl was not installed. Editors with the Pkl plugin need pkl in PATH: https://pkl-lang.org" >&2
fi
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
