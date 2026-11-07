#!/usr/bin/env bash
# Tests for scripts/changed-pkgs.sh (#881). Builds a small module in a temp
# dir: m/base is imported by m/mid, m/mid by m/leaf; m/tonly imports m/base
# only from its tests; m/other's test reads docs/GLOSSARY.md.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/changed-pkgs.sh"
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
mod="$work/mod"
mkdir -p "$mod"/{base,mid,leaf,tonly,other,docs/adr,internal/tui,base/testdata}
cd "$mod" || exit 1
printf 'module m\n\ngo 1.22\n' >go.mod
printf 'package base\n\nfunc B() int { return 1 }\n' >base/base.go
printf 'x\n' >base/testdata/golden.txt
printf 'package mid\n\nimport "m/base"\n\nfunc M() int { return base.B() }\n' >mid/mid.go
printf 'package mid\n\nimport "testing"\n\nfunc TestM(t *testing.T) {}\n' >mid/mid_test.go
printf 'package leaf\n\nimport "m/mid"\n\nfunc L() int { return mid.M() }\n' >leaf/leaf.go
printf 'package tonly\n\nfunc T() {}\n' >tonly/tonly.go
printf 'package tonly\n\nimport (\n\t"testing"\n\n\t"m/base"\n)\n\nfunc TestT(t *testing.T) { _ = base.B() }\n' >tonly/tonly_test.go
printf 'package other\n\nfunc O() {}\n' >other/other.go
printf 'package other\n\nimport "testing"\n\n// reads ../docs/GLOSSARY.md\nfunc TestO(t *testing.T) {}\n' >other/other_test.go
printf 'package tui\n\nfunc U() {}\n' >internal/tui/tui.go
printf '# Glossary\n' >docs/GLOSSARY.md
printf '# Notes\n' >docs/NOTES.md
printf 'check:\n' >Makefile
git init -q -b main . && git add -A && git -c user.email=t@t -c user.name=t commit -qm init

# sel FILE... prints the selection for those changed files, space-joined.
sel() { printf '%s\n' "$@" | "$script" --stdin 2>/dev/null | tr '\n' ' ' | sed 's/ $//'; }
e2e() { printf '%s\n' "$@" | "$script" --stdin --e2e 2>/dev/null; }
expect() { # name want got
    [ "$3" = "$2" ] && ok "$1" || fail "$1" "want [$2], got [$3]"
}

expect "leaf change selects only itself" "m/leaf" "$(sel leaf/leaf.go)"
expect "base change selects its dependents" "m/base m/leaf m/mid m/tonly" "$(sel base/base.go)"
expect "mid change selects mid and leaf" "m/leaf m/mid" "$(sel mid/mid.go)"
expect "test-only change selects only its package" "m/mid" "$(sel mid/mid_test.go)"
expect "testdata maps to its package" "m/base m/leaf m/mid m/tonly" "$(sel base/testdata/golden.txt)"
expect "docs-only change selects none" "" "$(sel docs/NOTES.md)"
expect "docs a test reads select that test" "m/other" "$(sel docs/GLOSSARY.md)"
expect "go.mod change selects all" "./..." "$(sel go.mod leaf/leaf.go)"
expect "go.sum change selects all" "./..." "$(sel go.sum)"
expect "Makefile change selects all" "./..." "$(sel Makefile)"
expect "CI config change selects all" "./..." "$(sel .github/workflows/ci.yml)"
expect "unknown non-doc file selects all" "./..." "$(sel some/config.toml)"
expect "Go file in a deleted package selects all" "./..." "$(sel gone/gone.go)"
expect "empty diff falls back to all" "./..." "$(printf '' | "$script" --stdin 2>/dev/null)"
CHECK_FULL=1 expect "CHECK_FULL forces all" "./..." "$(printf 'leaf/leaf.go\n' | CHECK_FULL=1 "$script" --stdin 2>/dev/null)"

expect "e2e skipped for a leaf change" "no" "$(e2e leaf/leaf.go)"
expect "e2e runs for a tui change" "yes" "$(e2e internal/tui/tui.go)"
expect "e2e runs on the full suite" "yes" "$(e2e go.mod)"
expect "e2e skipped for docs" "no" "$(e2e docs/NOTES.md)"

# git mode: the diff against the merge base with CHECK_BASE.
git -c user.email=t@t -c user.name=t checkout -qb feature
printf 'package leaf\n\nfunc L2() {}\n' >leaf/more.go
git add -A && git -c user.email=t@t -c user.name=t commit -qm leaf
expect "git diff against the base" "m/leaf" "$(CHECK_BASE=main "$script" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
printf '# more\n' >>docs/NOTES.md
expect "uncommitted docs edits add nothing" "m/leaf" "$(CHECK_BASE=main "$script" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
printf 'package mid\n\nfunc M2() {}\n' >mid/new.go
expect "untracked files count" "m/leaf m/mid" "$(CHECK_BASE=main "$script" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
rm mid/new.go; git checkout -q docs/NOTES.md
expect "unknown base falls back to all" "./..." "$(CHECK_BASE=nosuchref "$script" 2>/dev/null)"
git checkout -q main
expect "no diff against the base falls back to all" "./..." "$(CHECK_BASE=main "$script" 2>/dev/null)"
expect "reason goes to stderr" "1" "$(CHECK_BASE=nosuchref "$script" 2>&1 >/dev/null | grep -c 'full suite')"

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
