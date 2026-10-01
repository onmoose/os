#!/usr/bin/env bash
# Decide whether this push to main cuts a release on one version line.
#
#   decide.sh <version-file> <tag-prefix> <before-sha>
#
# moose has two version lines (BUILD.md # Versioning, DECISIONS.md 2026-10-01),
# and .github/workflows/release.yml calls this once for each:
#
#   decide.sh VERSION               v               "$before"   # the OS release
#   decide.sh CONTROL_PLANE_VERSION control-plane-v "$before"   # the control plane
#
# It runs in the repo's working tree, which must hold full history and tags
# (release.yml checks out with fetch-depth: 0). It writes key=value lines to
# stdout, ready to append to $GITHUB_OUTPUT, and explains itself on stderr:
#
#   created=true|false   this commit releases this line: publish what it ships
#   tag_exists=true|false the git tag is already there (at this commit). Then
#                        release.yml does not tag again, creates the GitHub
#                        Release only if it is missing, and the publish steps
#                        add only what is missing.
#   tag=<prefix><X.Y.Z>  the tag the file names
#   version=<X.Y.Z>      the file's contents
#   prev_tag=<tag>       the newest earlier tag on this line, or empty. The
#                        Release notes start from it, so a control-plane Release
#                        never lists commits since the last OS tag.
#
# FOUR outcomes:
#
#   - no tag for the file's version           -> cut the release.
#   - tag exists at THIS commit                -> resume: the release was cut
#                                                 here already, so carry on and
#                                                 publish what is still missing.
#   - tag exists elsewhere, file unchanged     -> ordinary merge; clean no-op.
#   - tag exists elsewhere, file bumped        -> hard error (exit 1).
#
# The resume case is what makes a half-done run recoverable. A run that tags the
# OS line and then fails on the control-plane tag, or on either GitHub Release,
# leaves a tag at this commit; re-running the workflow must finish the job, not
# trip over the tag it made itself. A tag at another commit is never resumed.
#
# The last case is what a plain "does the tag exist?" check gets wrong.
# release.yml tags BEFORE building, so a release whose image build fails leaves
# the tag behind. If the fix is then merged without deleting that tag, a bare
# existence check sees the tag, exits green, and the release silently never
# happens. Bumping a version file to a version that is already tagged is always
# a real release that cannot be cut, so say so loudly and say what to do.
#
# One more refusal, for a line that publishes images under a tag shape it shares
# with an older line. The control plane pushes ghcr images tagged v<X.Y.Z>, the
# shape every release before the split used (the private control plane resolves
# digests by that tag). If RELEASE_IMAGE_CHECK is set, it is run with v<X.Y.Z>
# as its last argument when the git tag is missing: exit 0 means an image tag
# already exists (release.yml checks the brain and the UI in one call), and
# the release is refused rather than overwrite released images with new bytes
# under the same number. Exit 1 means it does not exist. Anything else is an
# error, never "missing".
#
# RELEASE_REMOTE names the remote to read tags from (default: origin), and
# RELEASE_SHA the commit this run releases (default: HEAD; release.yml checks out
# GITHUB_SHA, so the two agree there).
set -euo pipefail

file="${1:?usage: decide.sh <version-file> <tag-prefix> <before-sha>}"
prefix="${2:?usage: decide.sh <version-file> <tag-prefix> <before-sha>}"
before="${3:-}"
remote="${RELEASE_REMOTE:-origin}"
sha="$(git rev-parse "${RELEASE_SHA:-HEAD}^{commit}")"

err() { echo "::error::$*" >&2; exit 1; }

[ -f "$file" ] || err "$file is missing at the repo root"
cur="$(tr -d '[:space:]' < "$file")"
[[ "$cur" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || err "$file holds '$cur', not a plain X.Y.Z version"
want="${prefix}${cur}"

echo "tag=$want"
echo "version=$cur"

# The newest earlier tag on this line. The pattern is anchored on the full tag
# shape, so the OS prefix "v" never picks up a "control-plane-v" tag.
prev_tag="$(git ls-remote --tags --refs "$remote" "refs/tags/${prefix}*" \
    | sed 's|.*refs/tags/||' \
    | grep -E "^${prefix}[0-9]+\.[0-9]+\.[0-9]+$" \
    | grep -vxF "$want" \
    | sort -V \
    | tail -n1 || true)"
echo "prev_tag=$prev_tag"

# The commit the tag points at, peeled, so an annotated tag compares as its
# commit. ls-remote lists the peeled "^{}" line after the tag's own line.
tag_commit="$(git ls-remote --tags "$remote" "refs/tags/$want" "refs/tags/$want^{}" | tail -n1 | cut -f1)"

if [ -z "$tag_commit" ]; then
    if [ -n "${RELEASE_IMAGE_CHECK:-}" ]; then
        rc=0
        $RELEASE_IMAGE_CHECK "v${cur}" || rc=$?
        case "$rc" in
            0) err "$file names $cur, and there is no git tag $want, but the image tag v${cur} is already published. Releasing it would overwrite released images with new bytes under the same number. Bump $file, or, if those images really are this release, tag $want at the commit they were built from." ;;
            1) ;;
            *) err "could not tell whether the image tag v${cur} is already published (check exited $rc). Refusing to release rather than risk overwriting it; re-run once the registry answers." ;;
        esac
    fi
    echo "created=true"
    echo "tag_exists=false"
    echo "release: tag $want does not exist yet; cutting a release" >&2
    exit 0
fi

if [ "$tag_commit" = "$sha" ]; then
    echo "created=true"
    echo "tag_exists=true"
    echo "resume: tag $want already points at this commit; carrying on with what is not yet published" >&2
    exit 0
fi

# The tag exists. Did THIS push change the file? `before` is main's previous
# head; on the very first push, a force-push, or a commit where the file didn't
# yet exist it may be absent/unreadable, in which case we fall back to the
# skip behaviour but say so.
prev=""
if [ -n "$before" ] \
    && [ "$before" != "0000000000000000000000000000000000000000" ] \
    && git cat-file -e "${before}^{commit}" 2>/dev/null; then
    prev="$(git show "${before}:${file}" 2>/dev/null | tr -d '[:space:]')" || prev=""
fi

if [ -n "$prev" ] && [ "$prev" != "$cur" ]; then
    at="$(git rev-parse --short "refs/tags/$want" 2>/dev/null || echo unknown)"
    err "$file was bumped ($prev -> $cur) but tag $want already exists (at $at). Refusing to skip a real release silently. Delete the stale tag and its GitHub Release and re-run this workflow, or bump $file again."
fi

if [ -z "$prev" ]; then
    echo "::warning::could not read $file at ${before:-<unknown>} to confirm whether this merge bumped it; assuming it did not, since tag $want already exists." >&2
fi

echo "created=false"
echo "tag_exists=true"
echo "skip: tag $want already exists and $file was not bumped by this merge; nothing to release on this line" >&2
