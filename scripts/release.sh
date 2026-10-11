#!/usr/bin/env bash
# Cut a saddle release: the one command the owner runs (make release
# VERSION=X.Y.Z). It checks a clean, current main with a CHANGELOG.md section
# for the version, runs make check, asks, then creates the annotated tag and
# pushes it. The tag push starts .github/workflows/release.yml, which builds
# and publishes the binaries. Any problem prints what is wrong and exits 1
# with nothing tagged or pushed. See docs/RELEASING.md.
#
# Agents never run this: tagging and publishing are the owner's call.
#
# Overrides (used by tests): RELEASE_REMOTE (origin), RELEASE_BRANCH (main),
# RELEASE_CHECK (make check), YES=1 skips the confirmation prompt.
set -euo pipefail

remote="${RELEASE_REMOTE:-origin}"
branch="${RELEASE_BRANCH:-main}"
check_cmd="${RELEASE_CHECK:-make check}"

die() { echo "release: $*" >&2; exit 1; }

[ -z "${SADDLE_TASK:-}" ] || die "refusing inside a saddle agent (SADDLE_TASK=$SADDLE_TASK): only the owner cuts releases."

version="${1:-${VERSION:-}}"
[ -n "$version" ] || die "usage: make release VERSION=X.Y.Z"
tag="v${version#v}"
[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || die "'$version' is not a release version (X.Y.Z, no prerelease suffix)."

git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git checkout"
cur="$(git symbolic-ref --short -q HEAD || echo "(detached HEAD)")"
[ "$cur" = "$branch" ] || die "checkout is on '$cur', not '$branch'. Releases are cut from $branch."
dirty="$(git status --porcelain)"
[ -z "$dirty" ] || die "working tree has uncommitted changes:
$(echo "$dirty" | sed 's/^/  /')"

git fetch -q --tags "$remote" "$branch" || die "git fetch $remote failed"
[ "$(git rev-parse HEAD)" = "$(git rev-parse "$remote/$branch")" ] \
    || die "$branch is not at $remote/$branch. Run make upgrade (or git pull --ff-only) first."

git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "tag $tag already exists."
if git ls-remote --exit-code --tags "$remote" "refs/tags/$tag" >/dev/null 2>&1; then
    die "tag $tag already exists on $remote."
fi
last="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)"
if [ -n "$last" ] && [ "$(printf '%s\n%s\n' "$last" "$tag" | sort -V | tail -n1)" != "$tag" ]; then
    die "$tag is not newer than the latest release $last."
fi

grep -qE "^## $tag( |$)" CHANGELOG.md 2>/dev/null || die "CHANGELOG.md has no '## $tag' section. Generate it with
  saddle changelog --version $tag --write
review it, land it on $branch through a PR, then rerun."

echo "Running $check_cmd before tagging $tag..."
$check_cmd || die "$check_cmd failed; nothing was tagged."

echo
echo "Release notes for $tag:"
awk -v h="## $tag" 'index($0, h) == 1 {p = 1; next} /^## / {p = 0} p' CHANGELOG.md | sed 's/^/  /'
echo
if [ "${YES:-}" != 1 ]; then
    [ -t 0 ] || die "no terminal to confirm on; rerun with YES=1 to tag and push $tag."
    read -r -p "Tag $(git rev-parse --short HEAD) as $tag and push it to $remote? [y/N] " ans
    case "$ans" in y|Y|yes) ;; *) die "not confirmed; nothing was tagged." ;; esac
fi

git tag -a "$tag" -m "saddle $tag"
if ! git push "$remote" "refs/tags/$tag"; then
    git tag -d "$tag" >/dev/null
    die "pushing $tag failed; the local tag was removed."
fi
echo "Pushed $tag. The release workflow builds and publishes it:"
echo "  https://github.com/brandonapol/saddle/actions/workflows/release.yml"
