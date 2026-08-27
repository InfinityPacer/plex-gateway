# Release process

`VERSION` is the single author-maintained version source. It contains a stable
semantic version without the Git tag's `v` prefix. `CHANGELOG.md` contains the
matching dated Chinese release section, and `CHANGELOG.en.md` provides its
English counterpart.

## Publish a release

1. Update `VERSION` and move the release notes from `Unreleased` into matching
   `X.Y.Z` sections in both changelog files.
2. Merge the release preparation to `main` after CI passes.
3. Create and push `vX.Y.Z` at that commit.

The tag workflow verifies that the tag equals `v${VERSION}`, runs the Go and
container checks, publishes `linux/amd64` and `linux/arm64` images with only
`X.Y.Z` and `latest` tags, and creates the GitHub Release from the matching
Chinese section in `CHANGELOG.md`.

Published Git tags are immutable. Do not delete or move a version tag to change
released source.

## Rebuild images

Run the CI workflow manually without custom inputs. Manual publication reads
`VERSION` from `main`, rebuilds the matching immutable `vX.Y.Z` Git tag, and
overwrites the corresponding GHCR version tag and `latest`. It does not create
or move a Git tag or edit a GitHub Release.

GitHub always displays its built-in workflow ref selector. The workflow reads
`main` and then checks out the matching release tag for manual runs, so that
selector does not change the image source.
