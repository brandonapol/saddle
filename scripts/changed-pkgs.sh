#!/usr/bin/env bash
# Picks the Go packages a change needs tested (#881), so the done gate and
# `make check` don't run the whole suite for a one-package change.
#
# usage: changed-pkgs.sh [--stdin] [--e2e]
#
# Prints, one per line, the import paths of the changed packages and every
# package that imports them (reverse dependencies, through tests too); `./...`
# for the full suite; or nothing when no Go test is affected (docs only).
# With --e2e it prints `yes` or `no`: whether the tmux e2e journeys should run.
# The reason for a full-suite fallback goes to stderr.
#
# Changed files come from git: the diff from the merge base of HEAD and the
# base ref, plus uncommitted and untracked files. The base is $CHECK_BASE, else
# the first of saddle/integration, origin/main, main that exists. --stdin reads
# the changed files (repo-relative, one per line) instead.
#
# Falls back to the full suite when it can't be sure: CHECK_FULL=1, no base,
# an empty diff, go.mod/go.sum/Makefile/CI config changed, a .go file outside
# any known package (a deleted package), or a non-Markdown file it can't map
# to a package. A Markdown file outside packages selects only the packages
# whose Go files mention its path (the glossary and ADR checks), else nothing.
# GO_TAGS (space-separated build tags) is passed to `go list`.
set -euo pipefail
GO="${GO:-go}"
stdin=0 e2e=0
for a in "$@"; do
    case "$a" in
        --stdin) stdin=1 ;;
        --e2e) e2e=1 ;;
        *) echo "usage: $0 [--stdin] [--e2e]" >&2; exit 2 ;;
    esac
done

# Paths whose change means the journeys should run: the TUI, the hook, agent
# launch code, the binary's entry point and the journeys themselves.
e2e_prefixes=(internal/tui/ internal/hook/ internal/agent/ internal/tmux/ internal/termpane/ cmd/ internal/e2e/ test/e2e/ internal/bench/)

full() {
    echo "changed-pkgs: full suite: $1" >&2
    if ((e2e)); then echo yes; else echo ./...; fi
    exit 0
}

[ "${CHECK_FULL:-}" = 1 ] && full "CHECK_FULL=1"

files=()
if ((stdin)); then
    mapfile -t files
else
    base="${CHECK_BASE:-}"
    if [ -z "$base" ]; then
        for b in saddle/integration origin/main main; do
            if git rev-parse -q --verify "$b^{commit}" >/dev/null; then base="$b"; break; fi
        done
    fi
    [ -n "$base" ] || full "no base ref (set CHECK_BASE)"
    mb="$(git merge-base HEAD "$base" 2>/dev/null)" || full "no merge base with $base"
    mapfile -t files < <({
        git diff --name-only "$mb" HEAD
        git diff --name-only HEAD
        git ls-files --others --exclude-standard
    } | sort -u)
fi
clean=()
for f in "${files[@]}"; do [ -n "$f" ] && clean+=("$f"); done
files=("${clean[@]+"${clean[@]}"}")
((${#files[@]} > 0)) || full "empty diff"

for f in "${files[@]}"; do
    case "$f" in
        go.mod | go.sum | Makefile | .github/* | .golangci*) full "$f changed" ;;
    esac
done

if ((e2e)); then
    for f in "${files[@]}"; do
        for p in "${e2e_prefixes[@]}"; do
            if [[ "$f" == "$p"* ]]; then echo yes; exit 0; fi
        done
    done
fi

# One line per package: import path, directory (relative to the module root),
# whether it has anything to test, then its imports, then "|" and its tests'
# imports.
root="$(pwd)"
declare -A dir2pkg testable rdeps trdeps
while read -r pkg dir has rest; do
    dir="${dir#"$root"}"; dir="${dir#/}"; dir="${dir:-.}"
    dir2pkg["$dir"]="$pkg"
    testable["$pkg"]="$has"
    imports="${rest%%|*}" timports="${rest#*|}"
    for i in $imports; do rdeps["$i"]+=" $pkg"; done
    for i in $timports; do [ "$i" = "$pkg" ] || trdeps["$i"]+=" $pkg"; done
done < <("$GO" list -e ${GO_TAGS:+-tags "$GO_TAGS"} -f '{{.ImportPath}} {{.Dir}} {{if or .GoFiles .TestGoFiles .XTestGoFiles}}1{{else}}0{{end}} {{join .Imports " "}} | {{join .TestImports " "}} {{join .XTestImports " "}}' ./...)

# pkgof DIR prints the package in DIR or its nearest ancestor below the
# module root (a root package doesn't own docs/ or scripts/).
pkgof() {
    local d="$1"
    while [ "$d" != . ]; do
        if [ -n "${dir2pkg[$d]+x}" ]; then echo "${dir2pkg[$d]}"; return 0; fi
        d="$(dirname "$d")"
    done
    return 1
}

declare -A changed testonly picked
mentioned=()
for f in "${files[@]}"; do
    d="$(dirname "$f")"
    if [[ "$f" == *.go ]]; then
        # The file's own directory must be a package: a .go file elsewhere is
        # a package that went away or isn't built, so play safe.
        [ -n "${dir2pkg[$d]+x}" ] || full "$f is in no package"
        p="${dir2pkg[$d]}"
        if [[ "$f" == *_test.go ]]; then
            [ -n "${changed[$p]+x}" ] || testonly["$p"]=1
        else
            changed["$p"]=1; unset 'testonly[$p]'
        fi
    elif [[ "$d" != . ]] && p="$(pkgof "$d")"; then
        changed["$p"]=1; unset 'testonly[$p]'
    elif [[ "$f" == *.md ]]; then
        mentioned+=("$f")
        parent="$(dirname "$f")"
        [[ "$parent" == */* ]] && mentioned+=("$parent")
    elif [[ "$f" == scripts/* ]]; then
        : # make test/scripts covers the scripts
    else
        full "can't map $f to a package"
    fi
done

if ((e2e)); then echo no; exit 0; fi

# Packages whose Go files name a changed doc (the glossary test, ADR checks).
if ((${#mentioned[@]} > 0)); then
    args=()
    for m in "${mentioned[@]}"; do args+=(-e "$m"); done
    while read -r gf; do
        d="$(dirname "$gf")"
        [ -n "${dir2pkg[$d]+x}" ] && testonly["${dir2pkg[$d]}"]=1
    done < <(git ls-files -co --exclude-standard -- '*.go' | xargs -r grep -lF "${args[@]}" -- 2>/dev/null || true)
fi

# The changed packages and everything that imports them, transitively; plus
# packages whose tests import any of those.
queue=("${!changed[@]}")
while ((${#queue[@]} > 0)); do
    p="${queue[0]}"; queue=("${queue[@]:1}")
    [ -n "${picked[$p]+x}" ] && continue
    picked["$p"]=1
    for r in ${rdeps[$p]:-}; do [ -n "${picked[$r]+x}" ] || queue+=("$r"); done
done
for p in "${!picked[@]}"; do
    for r in ${trdeps[$p]:-}; do testonly["$r"]=1; done
done
for p in "${!testonly[@]}"; do picked["$p"]=1; done

for p in "${!picked[@]}"; do
    # Keep packages with code or tests; a package go list can't build (all
    # files behind a tag) has neither and go test would reject it.
    [ "${testable[$p]:-0}" = 1 ] && echo "$p"
done | sort
