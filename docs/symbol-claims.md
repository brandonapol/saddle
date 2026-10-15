# Symbol-level claims

Status: Rules 1 to 3 and the forecast feed (#587-#592) have landed behind
`[claims] granularity = "symbol"`, default file (#329). See
[Sub-issues](#sub-issues).

## Problem

Claims are path globs. Two workers that need different functions in one file
serialize: the second is denied at its first write (`App.CheckWrite`,
`internal/app/claims.go`) and either stalls, works around the file, or asks
for a release (#292). That caps how many workers can usefully run on one
repo, and so how much of the weekly budget gets used.

The goal: let a worker write a file another task claims when the two edits
touch disjoint symbols and cannot conflict textually, and deny only on real
overlap. The rules are adapted from weft (`docs/design.md` §4,
`docs/protocol/wcp-v0.md`) to saddle's local, hook-driven model.

## Building blocks today

| Piece | What it does | Gap for this design |
| --- | --- | --- |
| `internal/gitx/symbols` | tree-sitter (pure Go, gotreesitter v0.55.1) extraction of Go (func, method as `Recv.M`, type, var, const) and Python (class, def, nested as `C.m`). `Symbol{Name, Kind, StartLine, EndLine}`. | No signature span or hash, so it cannot tell a signature change from a body change. No TypeScript, although the module embeds `TypescriptLanguage` and `TsxLanguage`. Does not report syntax-error nodes. |
| `internal/gitx/symbols.go` | `parseUnifiedZero` turns `git diff -U0` into per-file old/new line ranges; `enclosing` maps ranges to innermost symbols; `touchedSymbols` does both for a worktree vs its base. `symbolCache` memoises by sha256(path+content), in memory, up to 4096 entries. | The cache dies with the process, and the hook is a new process per tool call. It needs git to diff, so it can't diff a proposed edit that isn't on disk yet. |
| `internal/gitx/state.go` | `Snapshot` reads a worktree: dirty files, renames, ahead/behind, `Symbols []TouchedSymbol` when given an extractor. | Usable on demand as is. |
| `internal/gitx/delta.go`, `watcher.go` | fsnotify watcher with debounce, emitting `FileDirty`, `Committed`, `Rebased`, `RenameDetected`, `SymbolTouched` deltas. | Nothing runs it. `saddle up` and `saddle plugin engine` host long-lived loops, but neither starts a watcher. |
| `internal/forecast` | Pure pair scorer: same symbol 100, rename 100, same file 70, claimed file 50, import 40, claims overlap 30, co-change 20; levels at 20/40/70/90. | No caller. Scores "same file" 70 (high) even when the symbols are disjoint. |
| `internal/claims` | Glob matching, `Overlap`, `Owner`, `Covering`, carve-outs (`!glob`). | Path-only by design. It stays that way; symbols sit beside it. |
| `internal/hook` | PreToolUse resolves `writePaths` (Write/Edit `file_path`, opencode `apply_patch`) and calls `CheckWrite`; on deny it appends the owner's hunk (`ownerDiff`, #328, 200ms budget). PostToolUse injects queued notices as `additionalContext`. | Sees the tool input (so the proposed edit) but throws it away. |

`docs/ARCHITECTURE.md` calls the watcher and forecaster the "target design".
This design does **not** depend on the watcher; see
[Why not the watcher](#why-not-the-watcher).

## Design

### Vocabulary

- **f**: the repo-relative file being written.
- **B**: the task writing f now. **A**: a task whose path claim covers f
  (the *owner*). **C**: any other live task holding a symbol claim in f.
- **base(T)**: `merge-base(HEAD_T, integration)`, as `ownerDiff` computes it.
- **I**: the integration head.
- **proposed**: f's content after B's tool call, computed in the hook
  before the write (S2).
- **change set** of a task in f: the base-coordinate line ranges it changed
  plus the symbols those ranges touch, from `LineDiff(base blob, its version)`.
  Insertions are zero-width ranges at their base line. Symbols come from the
  base version (edited, deleted) and the new version (added).
- **conflict test** between two change sets on the same base blob: they
  share a symbol (same qualified name and kind, including two *new* symbols
  with one name), or some pair of ranges lies within `margin` base lines of
  each other (`margin = 2`, a constant: git's merge conflicts on touching
  hunks, and the margin also covers small context drift). Lines outside any
  symbol (imports, package clause) count through the range test only.

The change set and the conflict test live in `internal/gitx` (S1), so rules
1 and 2 share one implementation. The conflict test is the heart of both. It is deliberately textual
first: if git's three-way merge of the two edits could conflict, the test
says so.

### Granularity switch

```toml
[claims]
granularity = "file"   # "file" (default until Q4) | "symbol"
```

`config.Claims.Granularity`, validated at load: anything else is an error
naming the two values. In `file` mode every code path below is skipped and
saddle behaves exactly as today; symbol-claim rows in the store are ignored.
That makes `file` the kill switch. The default stays `file` until Q4: two
weeks without symbol-allow conflicts and hook latency under 300ms for 99%
of calls. Path claims, the `claim` tool, autopilot's overlap check and
plan-time ordering stay path-based in both modes; only the edit-time
verdict and the forecast change.

### The edit seam (S2)

Today the hook calls `CheckWrite(task, abs)` per write path. S2 makes it:

1. **Proposed content.** For each write path, the hook computes the file's
   content after the call, from the current file on disk and the tool input:
   - `Write`: `content`.
   - `Edit`: replace `old_string` with `new_string` once, or every match
     with `replace_all`. Not found or ambiguous: unknown.
   - `MultiEdit`: the edits in order, each as above.
   - opencode `apply_patch`: apply the file's `*** Update File` hunks; `Add
     File` is all-new; `Delete File` is empty content.
   - Anything else (`NotebookEdit`, unknown tools): unknown.

   Unknown means the symbol layer falls back for this write (below).
2. **`App.CheckEdit(e Edit) Decision`**, where `Edit{Task, Abs, Rel string;
   Before, After []byte; Known bool}`. It runs `CheckWrite` for the base
   verdict, then, in `symbol` mode, the PreToolUse symbol rules in a fixed
   order (rule 2, then rule 1). `Decision` gains `Owner string`, set when the
   deny is a path-claim deny, so rule 1 can tell a claim deny from a serial or
   outside-worktree deny (which it never overrides).
3. **`App.EditNotes(e Edit) string`** for PostToolUse. In `symbol` mode it
   runs the note rules (rule 3) and returns text for `additionalContext`.

Rules register in their own files (`symstale.go`, `symshare.go`,
`symassume.go`) as values in an ordered table, so S3–S5 never edit the same
file.

### Symbol spans and the span cache (S1)

`gitx.Symbol` gains `SigHash` (sha256 of the normalized signature text) and
the extractor reports `HasErrors bool` per parse. Signature per kind:

| Kind | Signature text |
| --- | --- |
| Go func / method | the declaration up to its `body` field: receiver, name, type params, params, results |
| Go type | the whole `type_spec` / `type_alias` (a field change is a signature change) |
| Go var / const | names and the declared `type` field, never the value |
| Python def | name, parameters, return annotation |
| Python class | name and superclasses |

Normalization collapses whitespace so `gofmt` churn is not a signature
change. This is also the fix for weft's misread: a one-line `func f() int {
return 1 }` or `var h = func() {...}` keeps signature and body on one line,
so a line-based test would call every body edit a signature write. Hashing
the signature text instead of comparing line ranges makes that impossible.

**Cache.** Keyed by (path's language, git blob id). The issue says (path,
blob SHA); the path only matters through its extension, so keying on the
language keeps hits across renames and copies. The blob id is computed in Go
the way git does (`"blob <len>\0" + content`, SHA-1 or SHA-256 per the
repo's `extensions.objectformat`, read once), so base-side lookups use
`git rev-parse <commit>:<path>` without reading content, and worktree or
proposed content is hashed without a subprocess. Entries live at
`<git common dir>/saddle-symbols/v<N>/<lang>/<blob id>.json` (written to a
temp file and renamed), shared by every worktree and every hook process.
`N` is the extractor version: bump it whenever extraction or signature
rules change, so a stale cache can never feed a verdict. The cache prunes
itself: a write that finds the directory over 64 MiB removes the least
recently modified entries until it is under 48 MiB, so no daemon is needed.

**LineDiff.** `gitx.LineDiff(old, new []byte) []Hunk` is an in-process
Myers line diff (no new module dependency: `go.mod` is serial), returning
old and new ranges like `parseUnifiedZero`. The hook needs it because the
proposed content is not on disk.

### Rule 1: symbol-granular writes in a shared file (S4)

Trigger: `symbol` mode, f in a supported language, and either the base
verdict is a path-claim deny (owner A) or another live task holds a symbol
claim in f.

1. Load the candidates: owner A (if any) and every C with a symbol-claim row
   on f, minus B.
2. For each candidate T, compute T's change set in f: `git -C <T worktree>
   merge-base` and `rev-parse base(T):f`, then T's current f (committed and
   dirty, as `worktreeDiff` reads it), then `LineDiff`. A candidate that has
   not changed f contributes an empty set.
3. B's change set: `LineDiff(base(B):f blob, proposed)`. This covers B's
   earlier edits to f as well as this one.
4. If any candidate's `base:f` blob differs from B's, **fall back** (one of
   them synced past a change to f; coordinates don't line up).
5. Run the conflict test against each candidate. On any conflict: deny,
   naming the owner and the symbol (or "edits within 2 lines of"), with the
   owner's hunk via `ownerDiff` (#328).
6. Otherwise allow, and upsert B's symbol-claim rows for f: one row per
   symbol in B's change set (`symbol_claims(task, path, symbol, kind)`,
   released with the task like path claims). Log `symbol_allow` with the
   candidate list.

The rows exist so a later write by A, or by a third task, knows whom to
compare against; they are not the source of truth for what changed. Every
check reads the candidates' live diffs, so a reverted edit stops counting at
once and nothing goes stale.

If A runs on an adapter without saddle's hooks (codex today), A's future
edits are never checked, so rule 1 does not apply to A's files: fall back.

**Decided (Q1):** first writer wins. When A, the path owner, later writes a
symbol B already took, A is denied and gets B's hunk. See
[Owner questions](#owner-questions).

### Rule 2: stale overwrite (S3)

Trigger: `symbol` mode, f supported, base verdict allow (or rule 1 allowed).

1. M = base(B), I = integration head. Skip when M = I or `M:f` = `I:f`.
2. Integration's change set: `LineDiff(M:f, I:f)` with its symbols, cached
   by the blob pair.
3. B's change set: `LineDiff(M:f, proposed)`.
4. Conflict test. On conflict, deny: "`<symbol>` in f changed on
   integration since your base (`<first commit subject from git log M..I --
   f>`). Run `saddle sync`, then redo this edit on the new version."
   Non-overlapping integration changes are left to the train, as today.

### Rule 3: stale assumption (S5)

Trigger: PostToolUse after a write to a supported f in `symbol` mode.

1. **Used names.** Identifiers in the lines B just added (`new_string`, or
   the new side of `LineDiff(before, after)`): bare names, and `x.Name`
   selectors.
2. **Signature changes on live tasks.** For each other live worker T: the
   supported files in `git diff --name-only base(T)` plus its dirty files;
   for each, the symbols whose `SigHash` differs between `base(T):g` and T's
   version, or that were deleted. Cached by blob pair.
3. **Signature changes landed since B's base**: the same over M..I, cached
   by (M, I).
4. A used name matches a changed symbol by its last segment, when the symbol
   lives in f's directory (same package) or the selector's qualifier is the
   last element of the symbol's directory. This over-reports; it only warns.
5. Live match: `additionalContext` note, "t12 is changing the signature of
   `Store.Claim` (internal/store/store.go): old `…` → new `…`. Depend on the
   new shape, stub it, or coordinate through the orchestrator." Each (T,
   symbol, new SigHash) is noted once per task (an event records it).
6. Landed match (Q2, decided): a PreToolUse deny with "run saddle sync
   first", using the rule 2 wording, "`<symbol>` in <path> changed on
   <integration> since your base (`<subject>`). Run `saddle sync`, then redo
   this edit on the new version." The edit will not compile after the next
   sync. Live matches stay warnings.
7. Body-only changes (same `SigHash`): nothing.

Rule 3 has a 300ms budget across all tasks; past it, it emits nothing. A
missed warning costs a red gate later, not a bad merge.

### Rule 4: forecaster feed (S6)

1. `App.Forecast()` builds `forecast.Input`: every active worker with its
   path claims; for each with a worktree, `gitx.Snapshot(worktree,
   {Integration, Symbols: symbols.Default()})` (files changed since base,
   touched symbols, renames), run in parallel with a 2s total budget;
   `History` from `git log -n 200 --name-only I`; `Serial` from config.
   `Imports` stays empty at first.
2. `forecast.Run` with one scoring change: when both tasks report at least
   one touched symbol in f and none in common, the pair gets the new
   `KindFileDisjoint` (weight 20, low) instead of `KindFile` (70, high).
3. `status` (MCP and CLI) gains `forecast`: pairs at medium and above, top
   10, each `{a, b, level, reason}`, plus one line saying how many live pairs
   are low or none, i.e. safe to run side by side. `brief` lists the pairs
   that involve the task being briefed.

This needs no granularity switch: it only informs the orchestrator, which
can then put more disjoint tasks in a wave.

### Fail-open fallback

The symbol layer's fallback is always **today's verdict**, never a blanket
allow: rule 1 falls back to the path-claim deny, rules 2 and 3 to doing
nothing (today's behavior). It falls back, logging `symbol_fallback` with
the reason, when:

- granularity is `file`;
- f's extension is unsupported, or f is over 2 MiB (`maxSymbolFile`);
- the proposed content is unknown (S2);
- a parse reports error nodes that intersect a changed range (half-written
  code, or a grammar gap);
- the bases' blobs of f differ between B and a candidate (rule 1);
- a candidate runs on an adapter without hooks (rule 1);
- git fails, or the check exceeds its budget: 250ms for the PreToolUse rules
  together (on top of `ownerDiff`'s 200ms, which only runs on deny).

### Why not the watcher

The hook is a short-lived process per tool call, so it cannot hold a
watcher; it would read the watcher's results from the store, written by
`saddle up` or the engine. That adds staleness (debounce up to 5s), a second
writer to keep in sync, and a dependency on a daemon being up, all for the
one rule (1) where staleness means a false negative. Reading the
candidates' diffs on demand costs one `merge-base`, one `rev-parse` and one
file read per candidate, and the cache absorbs the parsing. When the reactor
(#44) lands, it can feed the same tables from watcher deltas and the hook
can prefer them when fresh.

## Risks

False negatives (we allow, and the train or CI pays) are the costly side.

| Risk | Mitigation |
| --- | --- |
| Semantic coupling without textual overlap: B changes what a helper does, A's function relies on it. | Out of scope for claims, as it is today for different files. Rule 3 catches signature changes; CI catches the rest. |
| The owner's planned edits are invisible: A claimed f to change `X` later, B takes `X` first. | Q1, decided: first writer wins. The later writer is denied with the other's hunk, so the merge stays clean; A waits when B took `X` first. |
| Writes the hook doesn't see: Bash (`sed -i`, codegen), adapters without hooks. | Same blind spot as path claims today. Rule 1 falls back when the owner has no hooks. Rule 1 always re-reads live diffs, so a Bash edit that already happened is still counted. |
| Parser misreads: one-line declarations, syntax errors mid-edit, grammar gaps. | Signature by hash, not lines; error nodes in a changed range fall back; S1 tests the Go and Python shapes. |
| Insertions next to each other: both append a function at the end of the file, or add an import on adjacent lines. | The range test with `margin` catches touching hunks; new symbols with the same name conflict by name. |
| Base drift between tasks makes coordinates meaningless. | Different `base:f` blobs fall back to file level. |
| Stale cache entry after an extractor change. | Extractor version `N` in the cache path. |
| Hook latency. | On-disk cache by blob, a hard budget, fallback past it. Measure p50/p99 in the dogfood stage. |

False positives (deny or warn needlessly) cost throughput only; the
`margin` and rule 3's name matching err that way on purpose.

## Staged rollout

1. **Foundations, no behavior change.** S1 (spans, `SigHash`, cache,
   `LineDiff`), S2 (seam, config switch defaulting to `file`), S6 (forecast
   feed). Merge in any order.
2. **Dogfood rules 1 and 2.** S3 and S4 behind `granularity = "symbol"`,
   turned on in this repo's `.saddle/config.toml` only. Track `symbol_allow`,
   `symbol_deny`, `symbol_fallback` (by reason), hook latency, and any train
   conflict on a file where a symbol allow happened.
3. **Rule 3** (S5). Live signature changes warn; a use of a signature that
   has landed since the task's base is a PreToolUse deny (Q2, decided: run
   saddle sync first).
4. **Flip the default** to `symbol` once Q4 is met: two weeks without
   symbol-allow conflicts and hook latency under 300ms for 99% of calls.
   `file` stays as the kill switch.
5. **More languages** (Q3): TypeScript/TSX next, with weft's arrow-function
   cases (`const f = (x) => x`, `export const g = async () => {}`).

## Owner questions

- **Q1 (decided): who wins a symbol, the path owner or the first writer?**
  First writer wins. When B took symbol `X` in A's file and A now edits
  `X`, A is denied and gets B's hunk, the same as any claim deny. Two edits
  of one symbol stay exclusive, and the rule matches how path claims work
  today. A, the declared owner, waits when B took the symbol first.
- **Q2 (decided): what does "an error once landed" mean?** Deny with "run
  saddle sync first": a PreToolUse deny of the edit that uses the changed
  signature, with the rule 2 wording ("Run `saddle sync`, then redo this
  edit on the new version."). #592 shipped this deny.
- **Q3 (open; TypeScript, after dogfooding):** TypeScript/TSX. The issue
  asks for TS test shapes, but the extractor has no TS. Add it as a
  follow-up after the dogfood stage, or now as a seventh ticket? Still
  open; no ticket yet.
- **Q4 (decided, stage 4): when does the default flip?** The default flips
  to `symbol` after two weeks without symbol-allow conflicts and hook
  latency under 300ms for 99% of calls. `file` stays the kill switch.

## Sub-issues

| | Issue | Difficulty | `saddle:ready` | After |
| --- | --- | --- | --- | --- |
| S1 | #587 gitx: signature hashes, (lang, blob) span cache, in-process LineDiff | standard | yes | — |
| S2 | #588 Edit seam: proposed content in the hook, `CheckEdit`/`EditNotes`, `[claims] granularity` | standard | yes | — |
| S3 | #590 Rule 2: deny stale overwrites | hard | yes | S1, S2 |
| S4 | #591 Rule 1: share a file by symbols, symbol-claim table | hard | yes (Q1: first writer wins) | S1, S2 |
| S5 | #592 Rule 3: warn on stale assumptions | standard | yes (Q2: deny, run saddle sync first) | S1, S2 |
| S6 | #589 Forecast feed in status/brief; disjoint symbols score low | standard | yes | — |
