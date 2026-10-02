#!/usr/bin/env bash
# Bring the local saddle harness up to date with origin/main, then reinstall.
# Never discards local work: any problem prints what is wrong and exits 1.
#
# Overrides (used by tests): UPGRADE_REMOTE (origin), UPGRADE_BRANCH (main),
# UPGRADE_INSTALL (make install), UPGRADE_SADDLE (installed binary to query).
set -euo pipefail

remote="${UPGRADE_REMOTE:-origin}"
branch="${UPGRADE_BRANCH:-main}"
install_cmd="${UPGRADE_INSTALL:-make install}"

die() { echo "upgrade: $*" >&2; exit 1; }

git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git checkout"

git fetch "$remote" || die "git fetch $remote failed"

old="$(git rev-parse HEAD)"
cur="$(git symbolic-ref --short -q HEAD || echo "(detached HEAD)")"
if [ "$cur" != "$branch" ]; then
    die "checkout is on '$cur', not '$branch'. Switch with 'git switch $branch' (commit or stash any work first), then rerun."
fi

dirty="$(git status --porcelain)"
if [ -n "$dirty" ]; then
    echo "upgrade: working tree has uncommitted changes:" >&2
    echo "$dirty" | sed 's/^/  /' >&2
    die "commit or stash them, then rerun. Nothing was changed."
fi

if ! git merge --ff-only "$remote/$branch" >/dev/null 2>&1; then
    die "cannot fast-forward '$branch' to $remote/$branch (local commits not on the remote?). Resolve by hand; nothing was changed."
fi

new="$(git rev-parse HEAD)"
if [ "$old" = "$new" ]; then
    echo "Already up to date at $(git rev-parse --short "$new")."
else
    echo "Fast-forwarded $(git rev-parse --short "$old") -> $(git rev-parse --short "$new")."
fi

$install_cmd

gobin="$(go env GOBIN 2>/dev/null || true)"
bin="${UPGRADE_SADDLE:-${gobin:-$(go env GOPATH 2>/dev/null || true)/bin}/saddle}"
echo "Old commit: $old"
echo "New commit: $new"
if [ -x "$bin" ]; then
    echo "Installed:  $bin ($("$bin" version 2>/dev/null || echo unknown))"
fi

# Warn, never kill: running processes keep the old binary until restarted.
running="$(pgrep -x saddle 2>/dev/null | grep -vx "$$" || true)"
if [ -n "$running" ]; then
    echo "WARNING: saddle is still running from the old binary (pid: $(echo $running | tr '\n' ' '))."
    echo "         Restart the daemon/TUI to pick up the new version."
fi
