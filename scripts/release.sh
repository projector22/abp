#!/bin/sh
# release.sh - create and push a release tag.
#
# Usage: scripts/release.sh <vX.Y.Z[-pre] | patch | minor | major>
#
# Pushing a tag that starts with "v" is what triggers the GitHub release
# workflow (.github/workflows/release.yml): it runs the tests, cross-compiles,
# and publishes the release. Tags containing a "-" (v0.2.0-rc1) are published
# as pre-releases. This script only has to make that tag safely, so it refuses
# unless everything below holds:
#
#   * the tag looks like vMAJOR.MINOR.PATCH, optionally with a -prerelease
#   * it does not already exist, locally or on the remote (tags are fetched first)
#   * it is not older than the latest stable release (and not a pre-release
#     of one that has already shipped)
#   * the working tree is clean and you are on the release branch
#   * that branch is exactly what is on the remote, so the tag points at code
#     everyone can see
#   * the same checks CI runs (fmt-check, vet, test-race) pass
#
# It then shows what is about to be released, asks for confirmation, creates an
# annotated tag, and pushes it. If the push fails the local tag is removed so
# you can simply run it again.
#
# Environment:
#   DRY_RUN=1       run every check and show the result, but tag and push nothing
#   YES=1           skip the confirmation prompt (needed when not on a terminal)
#   SKIP_CHECKS=1   skip the fmt-check / vet / test-race run
#   REMOTE          remote to push to (default: origin)
#   BRANCH          release branch (default: the remote's default branch)
#   MAKE_CMD        make to run the checks with (default: make)

set -eu

REMOTE=${REMOTE:-origin}
BRANCH=${BRANCH:-}
DRY_RUN=${DRY_RUN:-}
YES=${YES:-}
SKIP_CHECKS=${SKIP_CHECKS:-}
MAKE_CMD=${MAKE_CMD:-make}

# die prints an error to stderr and stops. Nothing has been changed by the time
# any caller reaches it, except for the push-failure cleanup further down.
die() {
	printf 'release: %s\n' "$*" >&2
	exit 1
}

# core strips the leading "v" and any -prerelease suffix: v1.2.3-rc1 -> 1.2.3.
core() {
	printf '%s' "${1#v}" | sed 's/-.*//'
}

[ $# -eq 1 ] || die "usage: release.sh <vX.Y.Z[-pre] | patch | minor | major>"
arg=$1

git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git repository"

# The latest stable release (vX.Y.Z with no suffix). Pre-releases are ignored so
# that "patch" after v1.2.3 gives v1.2.4 even if v1.3.0-rc1 exists.
latest=$(git tag --list 'v[0-9]*' --sort=-v:refname |
	grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1 || true)

# Work out the tag: either given in full, or the next patch/minor/major.
case $arg in
patch | minor | major)
	v=${latest:-v0.0.0}
	v=${v#v}
	major=${v%%.*}
	rest=${v#*.}
	minor=${rest%%.*}
	patch=${rest#*.}
	case $arg in
	major) major=$((major + 1)) minor=0 patch=0 ;;
	minor) minor=$((minor + 1)) patch=0 ;;
	patch) patch=$((patch + 1)) ;;
	esac
	tag="v$major.$minor.$patch"
	;;
*)
	tag=$arg
	;;
esac

printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' ||
	die "'$tag' is not a valid version: use vMAJOR.MINOR.PATCH, optionally with -rc1 style suffix"

git remote get-url "$REMOTE" >/dev/null 2>&1 || die "no git remote named '$REMOTE'"

# Bring in the remote's branches and tags so every check below sees current
# state rather than whatever was last fetched.
git fetch --quiet --tags "$REMOTE" || die "could not fetch from '$REMOTE'"

# The tag must be new. The fetch above brought in the remote's tags, so this
# catches a tag that exists locally or only on the remote.
git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null && die "tag $tag already exists"

# It must move forward: never older than the latest stable release, and not a
# pre-release of a version that is already out.
if [ -n "$latest" ]; then
	newcore=$(core "$tag")
	oldcore=$(core "$latest")
	[ "$newcore" != "$oldcore" ] || die "$latest is already released; $tag would not be a new version"
	top=$(printf '%s\n%s\n' "$newcore" "$oldcore" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
	[ "$top" = "$newcore" ] || die "$tag is older than the latest release, $latest"
fi

# Find the release branch: as given, else the remote's default branch.
if [ -z "$BRANCH" ]; then
	BRANCH=$(git symbolic-ref --quiet --short "refs/remotes/$REMOTE/HEAD" 2>/dev/null |
		sed "s#^$REMOTE/##" || true)
fi
if [ -z "$BRANCH" ]; then
	for b in main master; do
		if git rev-parse --verify --quiet "refs/remotes/$REMOTE/$b" >/dev/null; then
			BRANCH=$b
			break
		fi
	done
fi
[ -n "$BRANCH" ] || die "cannot tell which branch to release from; set BRANCH=<name>"

current=$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)
[ "$current" = "$BRANCH" ] || die "releases are made from '$BRANCH', but you are on '${current:-a detached HEAD}'"

[ -z "$(git status --porcelain)" ] || die "the working tree has uncommitted or untracked changes; commit or stash them first"

# The tag will be pushed on its own, so HEAD must already be on the remote
# branch or the release would contain commits nobody else can see.
remote_ref="refs/remotes/$REMOTE/$BRANCH"
git rev-parse --verify --quiet "$remote_ref" >/dev/null || die "$REMOTE/$BRANCH does not exist"
head=$(git rev-parse HEAD)
if [ "$head" != "$(git rev-parse "$remote_ref")" ]; then
	if git merge-base --is-ancestor HEAD "$remote_ref"; then
		die "$BRANCH is behind $REMOTE/$BRANCH; pull first"
	fi
	die "$BRANCH differs from $REMOTE/$BRANCH (unpushed commits?); push or reconcile first"
fi

# The same checks CI runs before it will publish anything.
if [ -z "$SKIP_CHECKS" ]; then
	echo "Running checks (fmt-check, vet, test-race)..."
	"$MAKE_CMD" fmt-check vet test-race || die "checks failed; nothing was tagged"
fi

# Show what is about to go out.
if [ -n "$latest" ]; then
	range="$latest..HEAD"
	since="since $latest"
else
	range=HEAD
	since="(no previous release)"
fi
count=$(git rev-list --count "$range")
echo
echo "Release $tag from $BRANCH at $(git rev-parse --short HEAD): $count commit(s) $since"
git log --oneline --max-count=15 "$range" | sed 's/^/  /'
[ "$count" -le 15 ] || echo "  ... and $((count - 15)) more"
echo

if [ -n "$DRY_RUN" ]; then
	echo "Dry run: would tag $tag and push it to $REMOTE. Nothing was changed."
	exit 0
fi

if [ -z "$YES" ]; then
	[ -t 0 ] || die "not on a terminal, so cannot ask for confirmation; set YES=1 to proceed"
	printf 'Tag and push %s? [y/N] ' "$tag"
	read -r answer
	case $answer in
	y | Y | yes | YES) ;;
	*) die "cancelled; nothing was tagged" ;;
	esac
fi

git tag -a "$tag" -m "abp $tag"
if ! git push "$REMOTE" "refs/tags/$tag"; then
	git tag -d "$tag" >/dev/null
	die "pushing $tag failed; the local tag was removed, so you can run this again"
fi

echo "Pushed $tag. The release workflow will now test, build and publish it."
