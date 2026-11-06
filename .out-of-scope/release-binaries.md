# Shipping release binaries

- Rejected in: #544 (closed as not planned).

## The idea
Build and publish saddle binaries per platform (linux/amd64, darwin/arm64)
in a release pipeline, and test each of them.

## Why not
Saddle doesn't ship binaries. It is installed from source (`make install`,
`go install`), so a release pipeline was untested surface with no users.

## What saddle does instead
Tagged versions and release notes (`saddle changelog`, `docs/VERSIONING.md`);
users build from the tag.
