#!/usr/bin/env bash
# Tests for scripts/install.sh against a fake release on disk (file:// URLs).
# Run: make test/scripts
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/install.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0

ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# release <name> <tag>: a fake release dir with an archive whose saddle
# prints the tag, plus checksums.txt.
release() {
    local d="$tmp/$1" tag="$2" file="saddle_${2#v}_${os}_${arch}.tar.gz"
    mkdir -p "$d/rel/download/$tag" "$d/build"
    printf '#!/bin/sh\necho %s\n' "$tag" >"$d/build/saddle"; chmod 755 "$d/build/saddle"
    tar -czf "$d/rel/download/$tag/$file" -C "$d/build" saddle
    echo "$(sha "$d/rel/download/$tag/$file")  $file" >"$d/rel/download/$tag/checksums.txt"
    printf '{"url": "x", "tag_name": "%s", "name": "saddle %s"}\n' "$tag" "$tag" >"$d/latest.json"
    echo "$d"
}

d="$(release latest v0.2.0)"
out="$(SADDLE_RELEASES="file://$d/rel" SADDLE_LATEST_URL="file://$d/latest.json" SADDLE_INSTALL_DIR="$d/bin" sh "$script" 2>&1)"; rc=$?
[ $rc -eq 0 ] && [ "$("$d/bin/saddle")" = v0.2.0 ] && grep -q "Verified" <<<"$out" \
    && ok installs-latest || fail installs-latest "rc=$rc $out"

d="$(release pinned v0.1.0)"
out="$(SADDLE_VERSION=0.1.0 SADDLE_RELEASES="file://$d/rel" SADDLE_LATEST_URL=file:///nonexistent SADDLE_INSTALL_DIR="$d/bin" sh "$script" 2>&1)"; rc=$?
[ $rc -eq 0 ] && [ "$("$d/bin/saddle")" = v0.1.0 ] && ok installs-pinned || fail installs-pinned "rc=$rc $out"

d="$(release tampered v0.2.0)"
file="saddle_0.2.0_${os}_${arch}.tar.gz"
printf '#!/bin/sh\necho evil\n' >"$d/build/saddle"; tar -czf "$d/rel/download/v0.2.0/$file" -C "$d/build" saddle
out="$(SADDLE_VERSION=v0.2.0 SADDLE_RELEASES="file://$d/rel" SADDLE_INSTALL_DIR="$d/bin" sh "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "checksum mismatch" <<<"$out" && [ ! -e "$d/bin/saddle" ] \
    && ok tampered-refuses || fail tampered-refuses "rc=$rc $out"

d="$(release unlisted v0.2.0)"
: >"$d/rel/download/v0.2.0/checksums.txt"
out="$(SADDLE_VERSION=v0.2.0 SADDLE_RELEASES="file://$d/rel" SADDLE_INSTALL_DIR="$d/bin" sh "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "unverified" <<<"$out" && [ ! -e "$d/bin/saddle" ] \
    && ok unlisted-refuses || fail unlisted-refuses "rc=$rc $out"

exit $((fails > 0))
