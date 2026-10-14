#!/usr/bin/env bash
# Tests for scripts/e2e-nightly-shard.sh against throwaway test dirs.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/e2e-nightly-shard.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

mkdir -p "$tmp/a" "$tmp/b"
printf 'package a\nfunc TestJourneyOne(t *testing.T) {}\nfunc TestJourneyTwo(t *testing.T) {}\nfunc helper() {}\n' >"$tmp/a/x_test.go"
printf 'package b\nfunc TestJourneyThree(t *testing.T) {}\nfunc TestFour(t *testing.T) {}\nfunc TestJourneyOne(t *testing.T) {}\n' >"$tmp/b/y_test.go"

# 4 unique tests sorted: Four JourneyOne JourneyThree JourneyTwo; 2 shards deal alternately.
got="$("$script" 1 2 "$tmp/a" "$tmp/b")"
[ "$got" = '^(TestFour|TestJourneyThree)$' ] && ok "shard 1 of 2" || fail "shard 1 of 2" "$got"
got="$("$script" 2 2 "$tmp/a" "$tmp/b")"
[ "$got" = '^(TestJourneyOne|TestJourneyTwo)$' ] && ok "shard 2 of 2" || fail "shard 2 of 2" "$got"

# every test lands in exactly one shard
all="$(for i in 1 2 3; do "$script" "$i" 3 "$tmp/a" "$tmp/b" | tr -d '^()$' | tr '|' '\n'; done | sort)"
[ "$all" = "$(printf 'TestFour\nTestJourneyOne\nTestJourneyThree\nTestJourneyTwo')" ] && ok "shards partition the tests" || fail "partition" "$all"

"$script" 3 2 "$tmp/a" >/dev/null 2>&1 && fail "out of range" "accepted" || ok "rejects shard > total"
"$script" x 2 "$tmp/a" >/dev/null 2>&1 && fail "non-numeric" "accepted" || ok "rejects non-numeric"
"$script" 4 4 "$tmp/a" >/dev/null 2>&1 && fail "empty shard" "accepted" || ok "rejects an empty shard"

# the real tree: all shards together cover every Test func
root="$(cd "$(dirname "$script")/.." && pwd)"
want="$(cd "$root" && cat internal/e2e/*_test.go test/e2e/*_test.go | sed -n 's/^func \(Test[A-Za-z0-9_]*\)(.*/\1/p' | sort -u | wc -l)"
have="$(cd "$root" && for i in 1 2 3 4 5 6; do "$script" "$i" 6 | tr -d '^()$' | tr '|' '\n'; done | sort -u | wc -l)"
[ "$want" = "$have" ] && ok "real tree covered ($have tests)" || fail "real tree" "want $want have $have"

[ "$fails" -eq 0 ]
