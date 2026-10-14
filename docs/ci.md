# CI

Two workflows live in `.github/workflows`. `ci.yml` runs on every pull
request and on pushes to `main`. `e2e-nightly.yml` runs once a night and on
demand. Locally, `make check` covers the same ground as `ci.yml` except the
plugin tests (`make test/plugin`, which need the claude CLI) and govulncheck
(`make check/vuln`, which needs the network).

## `ci.yml`

A newer push to a PR branch cancels that PR's older run (#540). Pushes to
`main` always run to completion. The jobs run in parallel except coverage,
which waits for the test shards.

| Job | What it runs |
| --- | --- |
| `lint` | `make setup/golangci-lint`, then `make check/format check/tidy check/vet check/lint`. |
| `test (race shard N/3)` | `make test/race/shard SHARD=N TOTAL=3`, with a coverage profile. |
| `coverage` | Merges the shards' profiles and gates on a drop. |
| `govulncheck` | `make setup/govulncheck check/vuln`. |
| `scripts` | `make test/scripts check/templeak`. |
| `plugin` | `make test/plugin` against a pinned claude CLI, then checks that `plugin_test.go` did not skip. |
| `e2e` | `make test/e2e` (needs tmux). |

### lint

Gives fast feedback while the tests run (#539). `make setup/golangci-lint`
builds golangci-lint at the version the Makefile pins, with the Makefile's
build tags, so CI and `make check/lint` agree (#541).

### Race tests, 3 shards

`scripts/ci-race-shard.sh SHARD TOTAL` picks each shard's packages from `go
list ./...`. `internal/app`, the slowest package, gets shard 1 to itself. The
other packages are dealt round-robin across shards 2 and 3, so every package
lands in exactly one shard. The matrix does not fail fast, so one failing
shard does not cancel the others. Each shard uploads its profile as the
`cover-N` artifact and writes its 10 slowest packages to the job summary.

To run one shard locally: `make test/race/shard SHARD=2 TOTAL=3`.

### coverage

`scripts/cover-report.sh` merges the `cover-*` profiles. A block seen in
several profiles counts once, and it counts as covered if any profile covered
it. The script writes the total and per-package coverage to the job summary.
On a PR it fails the job if the total is more than 1 point below `main`'s
baseline (#543). A push to `main` uploads its total as the
`coverage-baseline` artifact.

**Baseline caveat.** The baseline is that artifact from the latest successful
push run on `main`. When there is none, the gate is skipped and the job still
passes. That happens on the first run, after the artifact expires under the
repo's artifact retention, and when no recent `main` push run succeeded. So a
coverage drop can pass unchecked until `main` records a new baseline. The
fetch step is `continue-on-error`, and a missing or non-numeric baseline file
never fails the report.

### govulncheck

Fails on reachable known vulnerabilities (#542). It runs on the latest Go
1.27.x, not `go.mod`'s 1.27.0, because govulncheck flags the stdlib issues
fixed in later patch releases, so the scan uses the patched toolchain.
Dependabot (`.github/dependabot.yml`) opens weekly grouped updates for Go
modules and GitHub Actions.

### scripts

Runs the shell scripts' tests (`scripts/*_test.sh`: upgrade, templeak, the CI
sharding and coverage scripts) and `check/templeak`, which fails if a test
binary leaves files in an empty `TMPDIR` just by starting.

### plugin

The plugin's TypeScript tests run inside the claude engine (they import
`claude-code/testing`), so the job installs `@anthropic-ai/claude-code` at a
pinned version (2.1.287, the minimum that loads mods). Bump the pin in
`ci.yml` and the `test/plugin` help text in the Makefile together.
`make test/plugin` fails, rather than skips, when claude is missing. A second
step runs `go test -v ./plugin/` and fails if any test printed `--- SKIP`.

### e2e

Runs the end-to-end journeys once per PR. The test binaries are built with
`-race`, but saddle itself is not race-built here. The nightly does that.

## `e2e-nightly.yml`

Runs at 06:17 UTC daily, and by hand through `workflow_dispatch`.

### e2e-race, 6 shards

With `E2E_RACE=1`, saddle itself is race-built too, and the full suite no
longer fits one 60-minute job (#537). `scripts/e2e-nightly-shard.sh SHARD 6`
sorts every top-level `Test` function in `internal/e2e` and `test/e2e` by name
and deals them round-robin. It prints an anchored `-run` regex, so a shard is
stable between runs and a new test lands in exactly one shard. Each shard runs
`make test/e2e RUN=<regex>` with `E2E_GO_TIMEOUT=40m` inside a 50-minute job.
It copies the slowest-journeys report into the job summary, even when the run
fails. The matrix does not fail fast.

To run one shard locally: `make test/e2e RUN="$(scripts/e2e-nightly-shard.sh 3 6)"`.

### govulncheck

The same check as in `ci.yml`, run nightly so new advisories surface without
a code change. This one uses `go.mod`'s Go version.

### report-failure

Runs when `e2e-race` or `govulncheck` failed. It keeps one tracking issue
titled `e2e-nightly is failing` with the `e2e-nightly` label, creating the
label if needed. If an open issue with that exact title exists, the job
comments on it with the run URL and date. Otherwise it opens a new one. Close
the issue once the nightly is green; the next failure opens a fresh one.
