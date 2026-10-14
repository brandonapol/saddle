#!/usr/bin/env bash
# Print the go test -run regex for one shard of the nightly e2e suite.
# Usage: e2e-nightly-shard.sh <shard 1..N> <N> [dir...]
#
# Every top-level Test function in the e2e packages (default internal/e2e and
# test/e2e) is sorted by name and dealt round-robin, so a shard is stable
# between runs and a new test lands in exactly one shard. Names are anchored.
set -euo pipefail
shard="${1:?usage: $0 <shard> <total> [dir...]}"
total="${2:?usage: $0 <shard> <total> [dir...]}"
shift 2
case "$shard$total" in *[!0-9]*) echo "shard and total must be integers" >&2; exit 2 ;; esac
if [ "$shard" -lt 1 ] || [ "$shard" -gt "$total" ]; then
    echo "shard $shard is outside 1..$total" >&2; exit 2
fi
[ $# -gt 0 ] || set -- internal/e2e test/e2e

names="$(for d in "$@"; do cat "$d"/*_test.go; done | sed -n 's/^func \(Test[A-Za-z0-9_]*\)(.*/\1/p' | sort -u)"
[ -n "$names" ] || { echo "no tests found in $*" >&2; exit 1; }
picked="$(printf '%s\n' "$names" | awk -v s="$shard" -v n="$total" '(NR-1)%n==s-1' | paste -sd'|')"
[ -n "$picked" ] || { echo "shard $shard of $total has no tests" >&2; exit 1; }
printf '^(%s)$\n' "$picked"
