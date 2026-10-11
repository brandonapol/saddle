#!/usr/bin/env bash
# Tests for scripts/release.sh using throwaway git repos. Run: make test/scripts
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/release.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
export RELEASE_CHECK="touch checked.marker" YES=1
unset SADDLE_TASK VERSION

ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

# setup <name> [changelog version]: origin (bare) + clone on main whose
# CHANGELOG.md has a section for the version (default v0.1.0).
setup() {
    local d="$tmp/$1"; mkdir -p "$d"
    git init -q --bare -b main "$d/origin.git"
    git clone -q "$d/origin.git" "$d/work" 2>/dev/null
    (cd "$d/work" && git checkout -q -b main 2>/dev/null
     printf '# Changelog\n\n## %s (2026-10-10)\n\n### Features\n\n- first (#1)\n' "${2:-v0.1.0}" >CHANGELOG.md
     printf 'checked.marker\n' >.gitignore
     git add . && git commit -qm one && git push -q origin main)
    echo "$d"
}
remote_tags() { git ls-remote --tags --refs origin | awk '{print $2}'; }

d="$(setup good)"; cd "$d/work"
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -eq 0 ] && [ -f checked.marker ] && [ "$(remote_tags)" = "refs/tags/v0.1.0" ] \
    && [ "$(git cat-file -t v0.1.0)" = tag ] && grep -q -- "- first (#1)" <<<"$out" \
    && ok tags-and-pushes || fail tags-and-pushes "rc=$rc $out"

out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "already exists" <<<"$out" && ok existing-tag-refuses || fail existing-tag-refuses "rc=$rc $out"

d="$(setup agent)"; cd "$d/work"
out="$(SADDLE_TASK=t9 bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "only the owner" <<<"$out" && [ -z "$(remote_tags)" ] && [ ! -f checked.marker ] \
    && ok agent-refuses || fail agent-refuses "rc=$rc $out"

d="$(setup badver)"; cd "$d/work"
out="$(bash "$script" 0.1 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "not a release version" <<<"$out" && ok bad-version-refuses || fail bad-version-refuses "rc=$rc $out"

d="$(setup dirty)"; cd "$d/work"; echo x >>CHANGELOG.md
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "uncommitted" <<<"$out" && [ -z "$(remote_tags)" ] && ok dirty-refuses || fail dirty-refuses "rc=$rc $out"

d="$(setup notmain)"; cd "$d/work"; git switch -q -c feature
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "'feature', not 'main'" <<<"$out" && ok not-on-main-refuses || fail not-on-main-refuses "rc=$rc $out"

d="$(setup nochangelog v0.0.9)"; cd "$d/work"
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "saddle changelog --version v0.1.0 --write" <<<"$out" && [ ! -f checked.marker ] \
    && ok missing-changelog-refuses || fail missing-changelog-refuses "rc=$rc $out"

d="$(setup behind)"; cd "$d/work"
git clone -q "$d/origin.git" "$d/other" && (cd "$d/other" && echo 2 >g && git add g && git commit -qm two && git push -q origin main)
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "not at origin/main" <<<"$out" && ok behind-refuses || fail behind-refuses "rc=$rc $out"

d="$(setup older v0.1.0)"; cd "$d/work"; git tag -a v0.2.0 -m old
out="$(bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "not newer than the latest release v0.2.0" <<<"$out" && ok older-refuses || fail older-refuses "rc=$rc $out"

d="$(setup checkfails)"; cd "$d/work"
out="$(RELEASE_CHECK=false bash "$script" 0.1.0 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "nothing was tagged" <<<"$out" && [ -z "$(git tag)" ] && ok failed-check-refuses || fail failed-check-refuses "rc=$rc $out"

d="$(setup noconfirm)"; cd "$d/work"
out="$(YES= bash "$script" 0.1.0 2>&1 </dev/null)"; rc=$?
[ $rc -ne 0 ] && grep -q "YES=1" <<<"$out" && [ -z "$(git tag)" ] && ok no-tty-refuses || fail no-tty-refuses "rc=$rc $out"

exit $((fails > 0))
