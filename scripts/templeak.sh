#!/usr/bin/env bash
# Fails if any test binary leaves something in its temp dir just by starting
# (#320): an init() or TestMain that makes a temp dir and never removes it
# leaks one per run. internal/app's binary also runs as the ref guard hook,
# once per ref update, and filled /tmp with ~437k empty dirs that way.
#
# Each package's test binary runs once with no tests (-run '^$', so only
# init and TestMain run) and TMPDIR pointing at an empty dir. The go command
# keeps its own build files in GOTMPDIR, elsewhere.
#
# Usage: scripts/templeak.sh [go test flags and packages...] (default ./...)
# Run: make check/templeak
set -euo pipefail
GO="${GO:-go}"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
mkdir "$work/tmp" "$work/gotmp"
[ $# -gt 0 ] || set -- ./...
if ! TMPDIR="$work/tmp" GOTMPDIR="$work/gotmp" "$GO" test -count=1 -run '^$' "$@" >"$work/log" 2>&1; then
    cat "$work/log"
    echo "templeak: go test failed before the temp dir could be checked"
    exit 1
fi
left="$(ls -A "$work/tmp")"
if [ -n "$left" ]; then
    echo "templeak: test binaries left these in an empty TMPDIR by only starting (an init or TestMain that never cleans up?):"
    echo "$left" | head -n 20 | sed 's/^/  /'
    exit 1
fi
echo "templeak: no test binary leaves anything in TMPDIR"
