# Releasing saddle

Saddle ships as tagged GitHub Releases: prebuilt binaries for linux and darwin
(amd64 and arm64), a `checksums.txt` of their SHA-256 sums, and notes taken from
`CHANGELOG.md`. This page covers the versioning policy, how the owner cuts a
release, and how users install, upgrade and roll back.

Only the owner tags and publishes. Agents never push tags or create releases:
`scripts/release.sh` refuses to run inside a saddle agent (`SADDLE_TASK` set),
and the release workflow runs only on a pushed `vX.Y.Z` tag.

## Versioning policy

Saddle uses [SemVer](https://semver.org), starting at **v0.1.0**. Tags are
`vMAJOR.MINOR.PATCH`, with no prerelease suffix.

These are saddle's public interface, so they decide the bump:

| Surface | Examples |
| --- | --- |
| CLI | command names, flags, exit codes, `--json` output fields |
| state.db schema | `PRAGMA user_version`, the number of migrations in `internal/store` |
| Config keys | `.saddle/config.toml` sections and keys, their defaults |
| MCP tool schemas | tool names, parameters and results in `internal/mcpserver` |
| Hook protocol | what the Claude Code hook reads and prints (`internal/hook`) |

| Bump | When |
| --- | --- |
| **Major** | Something above is removed or changes incompatibly: a flag or config key is dropped or renamed, a `--json` field changes type, an MCP tool loses a parameter, or a state.db migration can't be read by the previous release. |
| **Minor** | New behaviour that stays compatible: new commands, flags, config keys, MCP tools or optional parameters, and **any state.db schema change** (new migrations). |
| **Patch** | Bug fixes, docs and performance, with no schema change and nothing above added or removed. |

While saddle is at 0.x, a minor bump may also carry breaking changes. They are
always listed under **Breaking changes** in the changelog. Label the PR
`breaking`, or put `!` before the colon in its title (`feat!: ...`).

A schema change is never a patch. An older binary leaves a newer database
alone (`internal/store` doesn't lower `user_version`), so rolling back means
restoring the backup that the upgrade made (see [Rollback](#rollback)).

## What a binary knows about itself

Release builds stamp the version, commit and commit date with `-ldflags`, using
the same three variables `make install` sets:

```console
$ saddle version
v0.1.0
$ saddle version --long
saddle v0.1.0 (commit 3f2a9c1d0b4e, built 2026-10-10T12:00:00Z, state.db schema 4)
$ saddle version --json
```

Plain `saddle version` prints the version alone, for scripts and the plugin's
install check. `saddle doctor` includes the long form as its "saddle version"
check. Builds from source report a git description such as `v0.1.0-3-gabc1234`,
or the commit when there is no tag. They never claim a released version.

## Cutting a release

You need a clean checkout of `main` that matches `origin/main`, `gh` logged in,
and `main` protected.

1. **Write the changelog.**

   ```sh
   make changelog VERSION=0.1.0     # = saddle changelog --version v0.1.0 --write
   ```

   This lists the PRs merged into `main` since the previous `v*` tag (all of
   them for the first release) and groups them by label, falling back to the
   conventional prefix in the title:

   | Section | Labels | Prefixes |
   | --- | --- | --- |
   | Breaking changes | `breaking` | `type!:` |
   | Features | `enhancement`, `feature` | `feat:` |
   | Fixes | `bug`, `fix` | `fix:` |
   | Performance | `performance`, `perf` | `perf:` |
   | Documentation | `documentation`, `docs` | `docs:` |
   | Build and CI | `ci`, `build`, `dependencies` | `ci:`, `build:` |
   | Other changes | anything else | |

   Prefixes like `fix(hook):` or `store:` are removed from each entry. AI
   co-author trailers and "Generated with" lines never appear in the notes.
   Edit the section if it needs editing, then land it on `main` through a
   normal PR.

2. **Tag it.**

   ```sh
   make release VERSION=0.1.0
   ```

   `scripts/release.sh` refuses to continue unless all of these hold:
   - it is not running inside an agent
   - you are on `main` with a clean tree that matches `origin/main`
   - the tag is new and newer than the latest release
   - `CHANGELOG.md` has a `## v0.1.0` section
   - `make check` passes

   It then prints the notes, asks for confirmation (`YES=1` skips the
   prompt), and creates and pushes the annotated tag `v0.1.0`. Nothing is
   tagged if any step fails.

3. **Watch the workflow.** The tag push starts `.github/workflows/release.yml`:
   - `check` verifies the tag is on `main` and runs `make check` (the same
     gate as CI and the merge train).
   - `publish` extracts the tag's section from `CHANGELOG.md`
     (`saddle changelog --extract v0.1.0`). goreleaser (`.goreleaser.yaml`)
     then builds the four binaries with `CGO_ENABLED=0`, packs
     `saddle_<version>_<os>_<arch>.tar.gz`, writes `checksums.txt`, and
     creates the GitHub Release.

4. **Check it.** Install the release with the one-liner below on a clean
   machine and run `saddle version --long`.

If the workflow fails before publishing, fix the problem on `main`, then delete
the tag (`git push origin :refs/tags/v0.1.0 && git tag -d v0.1.0`) and cut the
release again. Never move a tag that has already been published. Cut a patch
release instead.

### The dry run

Every pull request and every push to `main` runs the `snapshot` job in
`release.yml`. It runs `goreleaser release --snapshot --skip=publish`, checks
`checksums.txt` against the archives, and runs the linux/amd64 binary to
confirm the stamped commit. A broken release config therefore fails in review,
not on release day. To run the same build locally, install
[goreleaser](https://goreleaser.com/install/) and run `make release/snapshot`.
The output lands in `dist/`.

## Installing

```sh
curl -fsSL https://raw.githubusercontent.com/brandonapol/saddle/main/scripts/install.sh | sh
```

The script picks the archive for your OS and architecture, downloads it with
`checksums.txt`, and checks the SHA-256. It installs nothing if the sum doesn't
match or the archive isn't listed. It then installs `saddle` into
`~/.local/bin`. You can change its behaviour with these variables:
- `SADDLE_VERSION=v0.1.0` pins a release.
- `SADDLE_INSTALL_DIR` picks another directory.

To check by hand, download the archive and `checksums.txt` from the release
page and run `sha256sum -c --ignore-missing checksums.txt`. On macOS, use
`shasum -a 256 -c`.

Building from source still works: `make install`, or
`go install github.com/brandonapol/saddle/cmd/saddle@v0.1.0`.

## Upgrading

```sh
saddle upgrade --check   # show the latest release and its changelog
saddle upgrade           # install it
```

`saddle upgrade` replaces the binary it runs from:

1. It shows the new release's changelog.
2. It refuses while this repo has running agents (running, idle or waiting on
   you). Let them finish or run `saddle down` first. `--force` upgrades anyway,
   and running agents keep the old binary until they restart.
3. It downloads the archive and `checksums.txt` and verifies the SHA-256. A
   mismatch installs nothing.
4. It backs up `.saddle/state.db` to `state.db.bak-<old version>-<time>` with
   SQLite's `VACUUM INTO`, which is safe while other processes use the
   database.
5. It swaps the binary atomically and keeps the old one at `<binary>.prev`.
6. It runs the new binary's `saddle migrate` to apply any new schema
   migrations.

If you built from source, use `make upgrade` instead. It fast-forwards `main`
and reinstalls.

### Migrations

`internal/store` applies migrations whenever a binary opens `state.db`. Before
it migrates a database that an older binary wrote, it copies the database to
`state.db.bak-schema<N>-<time>`. This also covers upgrades through
`make install` or `go install`. `saddle migrate` applies pending migrations
right away and prints the schema change.

### Rollback

```sh
mv "$(command -v saddle).prev" "$(command -v saddle)"    # the previous binary
cp .saddle/state.db.bak-v0.1.0-<time> .saddle/state.db   # only if the schema changed
```

Stop `saddle up` and your agents before you restore the database. An older
binary can't read a newer schema, so restore the backup whenever the release
you are leaving changed the schema. `saddle version --long` shows the schema.
You can also reinstall any release with
`SADDLE_VERSION=vX.Y.Z` and the install one-liner.

### Version skew

A running `saddle up` keeps the binary it started with. It records its version
and pid in `.saddle/up.json`. When that `saddle up` is older than the installed
binary, three places warn and tell you to restart it:
- `saddle version`, on stderr
- `saddle doctor`, as a warning on its "saddle version" check
- `saddle upgrade`, after it installs

To restart, quit `saddle up` and run it again. Agents keep working meanwhile.

## First release

v0.1.0 is cut once this machinery has landed. The owner needs to:

1. protect `main`
2. run `make changelog VERSION=0.1.0` and land the result
3. run `make release VERSION=0.1.0`
