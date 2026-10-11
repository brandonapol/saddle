#!/usr/bin/env bash
# Tests for scripts/templeak.sh against throwaway modules.
# Run: make test/scripts
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/templeak.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0

ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

# module <name> <test file body>: a module with one package and that test.
module() {
    local d="$tmp/$1"
    mkdir -p "$d"
    printf 'module example.com/%s\n\ngo 1.21\n' "$1" >"$d/go.mod"
    printf 'package p\n' >"$d/p.go"
    printf '%s\n' "$2" >"$d/p_test.go"
    echo "$d"
}

d="$(module leaky 'package p

import (
	"os"
	"testing"
)

func init() { _, _ = os.MkdirTemp("", "leak-") }

func TestX(t *testing.T) {}')"
out="$(cd "$d" && GOFLAGS=-mod=mod bash "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "leak-" <<<"$out" && ok leaky-init-fails || fail leaky-init-fails "rc=$rc $out"

d="$(module clean 'package p

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "tidy-")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestX(t *testing.T) { _ = t.TempDir() }')"
out="$(cd "$d" && GOFLAGS=-mod=mod bash "$script" 2>&1)"; rc=$?
[ $rc -eq 0 ] && ok clean-passes || fail clean-passes "rc=$rc $out"

d="$(module broken 'package p

func broken() { undefined() }')"
out="$(cd "$d" && GOFLAGS=-mod=mod bash "$script" 2>&1)"; rc=$?
[ $rc -ne 0 ] && grep -q "go test failed" <<<"$out" && ok build-error-fails || fail build-error-fails "rc=$rc $out"

exit $((fails > 0))
