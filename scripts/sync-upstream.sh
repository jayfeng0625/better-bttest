#!/usr/bin/env bash
# Merge a google-cloud-go bigtable release into better-bttest.
#
# Usage: scripts/sync-upstream.sh [bigtable/vX.Y.Z]
# Without an argument, it takes the latest bigtable release.
#
# The upstream branch holds upstream's bttest and emulator files unmodified, one
# commit per imported release. The script imports the release there, merges it
# into a sync branch off main, and moves cloud.google.com/go/bigtable in go.mod
# to the same release. When the build and tests pass, it fast-forwards main and
# deletes the sync branch. On a conflict or a failure, it stops on the sync
# branch. It pushes nothing.
set -euo pipefail

UPSTREAM_URL=https://github.com/googleapis/google-cloud-go.git
MODULE=cloud.google.com/go/bigtable

cd "$(git rev-parse --show-toplevel)"

if [ -n "$(git status --porcelain)" ]; then
    echo "The working tree has changes. Commit or stash them first." >&2
    exit 1
fi

current="bigtable/$(go list -m -f '{{.Version}}' "$MODULE")"
target="${1:-}"
if [ -z "$target" ]; then
    target=$(git ls-remote --tags --refs "$UPSTREAM_URL" 'refs/tags/bigtable/v*' |
        sed 's#.*refs/tags/##' | grep -E '^bigtable/v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)
fi
version="${target#bigtable/}"
if [ "$target" = "$current" ]; then
    echo "Already at $current."
    exit 0
fi

branch="sync/bigtable-$version"
if git show-ref --quiet "refs/heads/$branch"; then
    echo "Branch $branch exists. Finish or delete it first." >&2
    exit 1
fi

# The upstream branch starts at main's root commit, which is the first import.
if ! git show-ref --quiet refs/heads/upstream; then
    if git show-ref --quiet refs/remotes/origin/upstream; then
        git branch -q upstream origin/upstream
    else
        git branch -q upstream "$(git rev-list --max-parents=0 main)"
    fi
fi

tmp=$(mktemp -d)
cleanup() {
    git worktree remove --force "$tmp/upstream" 2>/dev/null || true
    rm -rf "$tmp"
}
trap cleanup EXIT

echo "Fetching $target from google-cloud-go ($current is current)."
git init -q "$tmp/src"
git -C "$tmp/src" remote add origin "$UPSTREAM_URL"
git -C "$tmp/src" fetch -q --depth=1 --filter=blob:none origin "refs/tags/$target:refs/tags/$target"
git -C "$tmp/src" sparse-checkout set bigtable/bttest bigtable/cmd/emulator
git -C "$tmp/src" checkout -q "$target"
commit=$(git -C "$tmp/src" rev-parse "$target^{commit}")

echo "Importing $target onto the upstream branch."
git worktree add -q "$tmp/upstream" upstream
(
    cd "$tmp/upstream"
    rm -rf bttest cmd/emulator
    mkdir -p bttest cmd/emulator
    cp -R "$tmp/src/bigtable/bttest/." bttest/
    cp -R "$tmp/src/bigtable/cmd/emulator/." cmd/emulator/
    cp "$tmp/src/LICENSE" LICENSE
    git add -A bttest cmd LICENSE
    if git diff --cached --quiet; then
        echo "$target changes nothing in bttest or the emulator."
    else
        git commit -q -m "Import bttest and the emulator from google-cloud-go $target" -m "Copies bigtable/bttest and bigtable/cmd/emulator from
googleapis/google-cloud-go at tag $target ($commit),
byte for byte, with the repository's LICENSE."
    fi
)
git worktree remove "$tmp/upstream"

echo "Merging into $branch."
git switch -q -c "$branch" main
if ! git merge -q --no-ff --no-commit upstream; then
    echo >&2
    echo "The merge stopped on conflicts in:" >&2
    git diff --name-only --diff-filter=U | sed 's/^/  /' >&2
    echo "Resolve them, run 'go get $MODULE@$version && go mod tidy', test, and commit on $branch." >&2
    echo "Then land it: git switch main && git merge --ff-only $branch" >&2
    exit 1
fi

go get "$MODULE@$version"
go mod tidy

echo "Building and testing."
if ! { go build ./... && go vet ./... && go test -count=1 ./...; }; then
    echo >&2
    echo "The build or tests failed on $branch, with the merge uncommitted. Fix them, and commit." >&2
    echo "Then land it: git switch main && git merge --ff-only $branch" >&2
    exit 1
fi

git add go.mod go.sum
if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
    git commit -q -m "Merge google-cloud-go $target" -m "Merges the upstream branch's import of $target and requires
$MODULE $version to match it."
else
    git commit -q -m "Require $MODULE $version" -m "$target changes nothing in bttest or the emulator."
fi

git switch -q main
git merge -q --ff-only "$branch"
git branch -q -d "$branch"
echo
echo "main is at google-cloud-go $target. Push it with: git push origin main upstream"
