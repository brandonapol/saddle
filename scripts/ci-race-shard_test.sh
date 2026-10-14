#!/usr/bin/env bash
# Tests for scripts/ci-race-shard.sh.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/ci-race-shard.sh"
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

pk=(m/a m/internal/app m/b m/c m/d)
[ "$("$script" 1 3 "${pk[@]}")" = "m/internal/app" ] && ok "shard 1 isolates internal/app" || fail "shard 1" "$("$script" 1 3 "${pk[@]}")"
[ "$("$script" 2 3 "${pk[@]}" | tr '\n' ' ')" = "m/a m/c " ] && ok "shard 2 of 3" || fail "shard 2" "$("$script" 2 3 "${pk[@]}")"
[ "$("$script" 3 3 "${pk[@]}" | tr '\n' ' ')" = "m/b m/d " ] && ok "shard 3 of 3" || fail "shard 3" "$("$script" 3 3 "${pk[@]}")"

all="$(for i in 1 2 3; do "$script" "$i" 3 "${pk[@]}"; done | sort | tr '\n' ' ')"
[ "$all" = "m/a m/b m/c m/d m/internal/app " ] && ok "shards partition the packages" || fail "partition" "$all"

"$script" 4 3 "${pk[@]}" >/dev/null 2>&1 && fail "out of range" "accepted" || ok "rejects shard > total"
"$script" 1 1 "${pk[@]}" >/dev/null 2>&1 && fail "total 1" "accepted" || ok "rejects total < 2"
"$script" x 3 "${pk[@]}" >/dev/null 2>&1 && fail "non-numeric" "accepted" || ok "rejects non-numeric"
"$script" 3 3 m/internal/app m/a >/dev/null 2>&1 && fail "empty shard" "accepted" || ok "rejects an empty shard"

# the real tree: the shards together cover every package go list reports
root="$(cd "$(dirname "$script")/.." && pwd)"
want="$(cd "$root" && go list ./... | sort)"
got="$(cd "$root" && for i in 1 2 3; do "$script" "$i" 3; done | sort)"
[ "$want" = "$got" ] && ok "real tree covered" || fail "real tree" "shards differ from go list"
[ "$(cd "$root" && "$script" 1 3)" = "$(cd "$root" && go list ./internal/app)" ] && ok "real shard 1 is internal/app" || fail "real shard 1" "not internal/app"

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
