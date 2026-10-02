#!/usr/bin/env bash
# Tests for scripts/upgrade.sh using throwaway git repos. Run: make test/scripts
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/upgrade.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
export UPGRADE_INSTALL="touch installed.marker" UPGRADE_SADDLE=/nonexistent

ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

# setup <name>: origin (bare) + clone on main with one commit.
setup() {
    local d="$tmp/$1"; mkdir -p "$d"
    git init -q --bare -b main "$d/origin.git"
    git clone -q "$d/origin.git" "$d/work" 2>/dev/null
    (cd "$d/work" && git checkout -q -b main 2>/dev/null; echo 1 >f; git add f; git commit -qm one; git push -q origin main)
    echo "$d"
}
advance() { # push a new commit to origin from a second clone
    git clone -q "$1/origin.git" "$1/other" && (cd "$1/other" && echo 2 >g && git add g && git commit -qm two && git push -q origin main)
}

d="$(setup uptodate)"; cd "$d/work"
out="$(bash "$script" 2>&1)"; rc=$?
[ $rc -eq 0 ] && grep -q "Already up to date" <<<"$out" && [ -f installed.marker ] \
    && ok up-to-date || fail up-to-date "rc=$rc $out"

d="$(setup behind)"; advance "$d"; cd "$d/work"; old="$(git rev-parse HEAD)"
out="$(bash "$script" 2>&1)"; rc=$?
[ $rc -eq 0 ] && [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] && [ "$old" != "$(git rev-parse HEAD)" ] \
    && grep -q "Old commit: $old" <<<"$out" && [ -f installed.marker ] \
    && ok behind-and-clean || fail behind-and-clean "rc=$rc $out"

d="$(setup dirty)"; advance "$d"; cd "$d/work"; old="$(git rev-parse HEAD)"; echo x >>f
out="$(bash "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && [ "$(git rev-parse HEAD)" = "$old" ] && [ "$(cat f)" = "$(printf '1\nx')" ] \
    && grep -q "uncommitted" <<<"$out" && grep -q " M f" <<<"$out" && [ ! -f installed.marker ] \
    && ok dirty-refuses || fail dirty-refuses "rc=$rc $out"

d="$(setup notmain)"; advance "$d"; cd "$d/work"; git switch -q -c feature; old="$(git rev-parse HEAD)"
out="$(bash "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && [ "$(git rev-parse HEAD)" = "$old" ] && [ "$(git branch --show-current)" = feature ] \
    && grep -q "'feature', not 'main'" <<<"$out" && [ ! -f installed.marker ] \
    && ok not-on-main-refuses || fail not-on-main-refuses "rc=$rc $out"

exit $((fails > 0))
