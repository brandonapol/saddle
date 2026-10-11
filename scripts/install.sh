#!/bin/sh
# Install a saddle release binary, verifying its SHA-256 checksum.
#
#   curl -fsSL https://raw.githubusercontent.com/brandonapol/saddle/main/scripts/install.sh | sh
#
# Environment:
#   SADDLE_VERSION      release to install, e.g. v0.1.0 (default: the latest)
#   SADDLE_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   SADDLE_RELEASES     release base URL (default: GitHub; tests use file://)
#   SADDLE_LATEST_URL   JSON naming the latest tag (default: the GitHub API)
#
# Nothing is installed unless the archive matches checksums.txt.
set -eu

repo="brandonapol/saddle"
releases="${SADDLE_RELEASES:-https://github.com/$repo/releases}"
latest_url="${SADDLE_LATEST_URL:-https://api.github.com/repos/$repo/releases/latest}"
dir="${SADDLE_INSTALL_DIR:-$HOME/.local/bin}"

die() { echo "install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "needs $1"; }
need curl
need tar

case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "no saddle build for $(uname -s); build from source (make install)" ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "no saddle build for $(uname -m); build from source (make install)" ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
    sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
    sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
    die "needs sha256sum or shasum to verify the download"
fi

tag="${SADDLE_VERSION:-}"
if [ -z "$tag" ]; then
    tag="$(curl -fsSL "$latest_url" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
    [ -n "$tag" ] || die "could not find the latest release at $latest_url"
fi
case "$tag" in v*) ;; *) tag="v$tag" ;; esac

file="saddle_${tag#v}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading saddle $tag for $os/$arch..."
curl -fsSL -o "$tmp/$file" "$releases/download/$tag/$file" || die "download of $file failed"
curl -fsSL -o "$tmp/checksums.txt" "$releases/download/$tag/checksums.txt" || die "download of checksums.txt failed"

want="$(awk -v f="$file" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt lists no $file; refusing to install an unverified binary"
got="$(sha256 "$tmp/$file")"
[ "$got" = "$want" ] || die "checksum mismatch for $file (got $got, want $want): the download is corrupt or tampered. Nothing was installed."
echo "Verified $file (sha256 $got)."

tar -xzf "$tmp/$file" -C "$tmp" saddle || die "no saddle binary in $file"
mkdir -p "$dir"
# Write next to the target, then rename: a running saddle keeps the old file.
cp "$tmp/saddle" "$dir/.saddle.new"
chmod 755 "$dir/.saddle.new"
mv -f "$dir/.saddle.new" "$dir/saddle"
echo "Installed $dir/saddle ($("$dir/saddle" version 2>/dev/null || echo "$tag"))."

case ":$PATH:" in
    *":$dir:"*) ;;
    *) echo "Note: $dir is not on your PATH. Add it to run saddle directly." ;;
esac
echo "Next: cd into a repo and run saddle init (docs/QUICKSTART.md). Later, saddle upgrade."
