# Release process

`VERSION` is the single author-maintained version source. It contains a stable
semantic version without the Git tag's `v` prefix. `CHANGELOG.md` contains the
matching dated Chinese release section, and `CHANGELOG.en.md` provides its
English counterpart.

## Publish a release

1. Update `VERSION` and move the release notes from `Unreleased` into matching
   `X.Y.Z` sections in both changelog files.
2. Merge the release preparation to `main` after CI passes.
3. The Release workflow creates `vX.Y.Z` at that commit and publishes the
   release automatically.

The Release workflow runs only when `VERSION` changes on `main`. It validates
the version and both changelogs, runs the Go checks, creates the matching tag,
publishes `linux/amd64` and `linux/arm64` images with only `X.Y.Z` and `latest`
tags, and creates the GitHub Release from the matching Chinese section in
`CHANGELOG.md`.

## Rebuild a release

Run the Release workflow manually without custom inputs. Manual publication
reads `VERSION` from the latest `main`, replaces the matching `vX.Y.Z` tag at
that commit, overwrites the corresponding GHCR version tag and `latest`, and
updates the existing GitHub Release.

GitHub always displays its built-in workflow ref selector. The workflow reads
the latest `main` for manual runs, so that selector does not change the release
source. Rebuilding a published version changes the source attached to that tag
and may change the image digest; use it only for an intentional same-version
correction.

Git tags, GHCR images, and GitHub Releases cannot be updated as one transaction.
If a Release run fails after creating or moving the tag, rerun that same
workflow. The tag step is idempotent for the selected source and the remaining
steps will rebuild the images and repair the Release.
