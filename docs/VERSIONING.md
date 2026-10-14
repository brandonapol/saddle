# Versioning saddle

Saddle is built from source. A version is an annotated `vX.Y.Z` git tag on
`main`, with a `CHANGELOG.md` section for it. There are no prebuilt binaries:
install with `make install` from a checkout, or with
`go install github.com/brandonapol/saddle/cmd/saddle@vX.Y.Z` (what the plugin's
install check prints).

Only the owner tags. Agents never push tags.

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
| **Major** | Something above is removed or changes incompatibly: a flag or config key is dropped or renamed, a `--json` field changes type, an MCP tool loses a parameter, or a state.db migration can't be read by the previous version. |
| **Minor** | New behaviour that stays compatible: new commands, flags, config keys, MCP tools or optional parameters, and **any state.db schema change** (new migrations). |
| **Patch** | Bug fixes, docs and performance, with no schema change and nothing above added or removed. |

While saddle is at 0.x, a minor bump may also carry breaking changes. They are
always listed under **Breaking changes** in the changelog. Label the PR
`breaking`, or put `!` before the colon in its title (`feat!: ...`).

A schema change is never a patch. An older binary leaves a newer database
alone (`internal/store` doesn't lower `user_version`), so going back to an
older version means restoring a backup (see [Migrations](#migrations)).

## What a binary knows about itself

`make install` stamps the version, commit and commit date with `-ldflags`:

```console
$ saddle version
v0.1.0
$ saddle version --long
saddle v0.1.0 (commit 3f2a9c1d0b4e, built 2026-10-10T12:00:00Z, state.db schema 4)
$ saddle version --json
```

Plain `saddle version` prints the version alone, for scripts and the plugin's
install check. `saddle doctor` includes the long form as its "saddle version"
check. A build from an untagged commit reports a git description such as
`v0.1.0-3-gabc1234`, or the commit when there is no tag. It never claims a
tagged version.

## Tagging a version

You need a clean checkout of `main` that matches `origin/main`.

1. **Write the changelog.**

   ```sh
   make changelog VERSION=0.1.0     # = saddle changelog --version v0.1.0 --write
   ```

   This lists the PRs merged into `main` since the previous `v*` tag (all of
   them for the first version) and groups them by label, falling back to the
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

2. **Tag it** once that PR has merged and `make check` passes on `main`:

   ```sh
   git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0
   ```

   `saddle changelog --extract v0.1.0` prints the tag's section, for notes
   elsewhere. Never move a pushed tag. Tag a patch version instead.

## Upgrading

From a checkout, `make upgrade` fast-forwards `main` from origin and reinstalls.
It refuses on any other branch or with uncommitted changes. Otherwise rerun
`go install` with the newer tag.

### Migrations

`internal/store` applies migrations whenever a binary opens `state.db`. Before
it migrates a database that an older binary wrote, it copies the database to
`state.db.bak-schema<N>-<time>`. `saddle migrate` applies pending migrations
right away and prints the schema change.

To go back to an older binary across a schema change, stop `saddle up` and your
agents, install the older version, and copy that backup over
`.saddle/state.db`. An older binary can't read a newer schema.
`saddle version --long` shows the schema.

### Version skew

A running `saddle up` keeps the binary it started with. It records its version
and pid in `.saddle/up.json`. When that `saddle up` is older than the installed
binary, two places warn and tell you to restart it:
- `saddle version`, on stderr
- `saddle doctor`, as a warning on its "saddle version" check

To restart, quit `saddle up` and run it again. Agents keep working meanwhile.
