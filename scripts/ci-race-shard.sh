#!/usr/bin/env bash
# Prints the packages for one shard of the race suite (#539), one per line.
# usage: ci-race-shard.sh SHARD TOTAL [PKG...]   (PKG defaults to `go list ./...`)
# Shard 1 is internal/app alone, the slowest package; the others are dealt
# round-robin shards 2..TOTAL, so every package lands in exactly one shard.
# GO_TAGS (space-separated build tags) is passed to `go list`.
set -euo pipefail
shard="${1:-}" total="${2:-}"
[[ "$shard" =~ ^[0-9]+$ && "$total" =~ ^[0-9]+$ ]] || { echo "usage: $0 SHARD TOTAL [PKG...]" >&2; exit 2; }
if ((total < 2 || shard < 1 || shard > total)); then
    echo "shard $shard of $total is out of range (need 1 <= shard <= total, total >= 2)" >&2
    exit 2
fi
shift 2
if (($# > 0)); then
    pkgs=("$@")
else
    mapfile -t pkgs < <(${GO:-go} list ${GO_TAGS:+-tags "$GO_TAGS"} ./...)
fi
heavy='internal/app'
out=()
i=0
for p in "${pkgs[@]}"; do
    if [[ "$p" == */"$heavy" || "$p" == "$heavy" ]]; then
        ((shard == 1)) && out+=("$p")
        continue
    fi
    if ((shard > 1 && i % (total - 1) == shard - 2)); then out+=("$p"); fi
    i=$((i + 1))
done
((${#out[@]} > 0)) || { echo "shard $shard of $total has no packages" >&2; exit 1; }
printf '%s\n' "${out[@]}"
