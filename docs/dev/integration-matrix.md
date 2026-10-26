# B — Corrective PR integration matrix

> Verified live on 2026-10-26 against `origin/main` = `414b276`.
> **No PR in this document has been merged.** This is the reviewed plan that
> Session A will execute.

## 1. Verified status of every PR

All fifteen open PRs target `main`, all are `MERGEABLE`, and every head SHA
below equals the local branch tip.

| PR | Branch | Head | Mergeable | Subject | CI |
|---|---|---|---|---|---|
| #85 | `fix/hal9001-shred-agent-auth` | `7ae59580` | ✅ | agent-accessible destructive memory/lesson operations | green |
| #86 | `fix/hal9001-confidence-provenance` | `68a8b22c` | ✅ | provenance-only evidence affecting confidence | green |
| #87 | `fix/hal9001-embedding-migration-diagnostics` | `da25efeb` | ✅ | partial embedding migration diagnostics | green |
| #88 | `fix/hal9001-raw-memory-shred-linkage` | `e0a2af94` | ✅ | staged raw-memory links for shred cascading | green |
| #89 | `fix/hal9001-provenance-test-isolation` | `ed9ad9de` | ✅ | provenance-attribution test isolation | green |
| #90 | `fix/hal9001-gc-reporting-honesty` | `0a6b218c` | ✅ | garbage-collection failure reporting | green |
| #91 | `fix/hal9001-ingest-undo-honesty` | `b6bffa8e` | ✅ | honest ingest undo behaviour | green |
| #95 | `fix/hal9001-r2-confidence-decay` | `ac523931` | ✅ | scheduled confidence decay | green |
| #96 | `fix/hal9001-r2-save-evidence` | `49c8e6f3` | ✅ | reject unsupported evidence on memory save | green |
| #97 | `fix/hal9001-r3-directive-retention` | `583d325f` | ✅ | standing directives and retention | green |
| #98 | `fix/hal9001-r3-schema-init-concurrency` | `4fe840d6b` | ✅ | concurrent schema initialization | green |
| #99 | `fix/hal9001-r3-install-next-steps` | `0f67fcbe` | ✅ | installed wake-context command parity | ✅ **green (fixed this round)** |

### New PRs produced by this foundation round

| PR | Branch | Head | Subject |
|---|---|---|---|
| #102 | `fix/hal9001-mpmcli-env-isolation` | `af8a699e` | isolate the `mpmcli` package's environment-sensitive tests |
| #103 | `fix/hal9001-signal-test-determinism` | `5c687d00` | make the signal-interruption test deterministic |
| #104 | `fix/hal9001-ci-gate-integrity` | `4c0bfb0e` | make the full-suite sweep a real gate; cancel superseded runs |

**#104 is stacked on #103** and is green only once #103 is in `main`.

## 2. Files and subsystems touched

| PR | Primary files | Subsystem |
|---|---|---|
| #85 | `cmd/mpm/handlers_memory.go`, `internal/core/tools/handlers.go`, `internal/core/tools/registry_list.go`, `cmd/mpm/call_shred_operator_only_test.go`, `agent_installation/*` | tool surface / destructive operations |
| #86 | `internal/core/evidence_store.go`, `internal/core/db.go`, `internal/core/confidence_provenance_test.go` | confidence scoring |
| #87 | `cmd/mpm/embedding_partial_migration*`, `internal/core/web_db.go` | doctor diagnostics |
| #88 | `internal/core/schema.go`, `internal/core/memory_tools.go`, `internal/core/migrate_md.go`, `internal/core/raw_memory_shred_linkage_test.go` | schema + shred cascade |
| #89 | `cmd/mpm/provenance_env_isolation_test.go`, `cmd/mpm/why_provenance_test.go`, `cmd/mpm/correlation_tool_origin_test.go` | test isolation |
| #90 | `cmd/mpm/ops_gc*` | maintenance |
| #91 | `cmd/mpm/ops_ingest*` | ingestion |
| #95 | `internal/core/schema.go`, `internal/core/evidence_store.go`, `internal/core/confidence_maintenance.go`, `internal/scheduler/confidence_decay.go` | confidence decay |
| #96 | `internal/core/tools/handlers.go`, `internal/core/tools/registry_list.go` | tool surface |
| #97 | `internal/core/directive_retention_test.go`, `docs/SPEC.md` | retention |
| #98 | `internal/core/db.go`, `internal/core/maintenance_lock.go`, `internal/core/schema_init_concurrency_test.go` | schema init concurrency |
| #99 | `install.sh`, `scripts/tests/test_install_sh_contracts.py` | installer |

## 3. Overlap matrix — empirically verified

Computed with `git merge-tree --write-tree` for every adjacent pair **and**
confirmed by a real twelve-way `git merge` in a disposable worktree.

| Shared file | PRs | Result |
|---|---|---|
| `internal/core/schema.go` | #88, #95 | 🔴 **CONFLICT** |
| `cmd/mpm/handlers_memory.go` | #85, #88 | ✅ clean |
| `docs/SPEC.md` | #85, #88, #95, #96, #97 | ✅ clean (distinct anchors) |
| `internal/core/db.go` | #86, #98 | ✅ clean (~1000 lines apart) |
| `internal/core/evidence_store.go` | #86, #95 | ✅ clean |
| `internal/core/tools/handlers.go` | #85, #96 | ✅ clean |
| `internal/core/tools/registry_list.go` | #85, #96 | ✅ clean |

**Exactly one conflict in the whole set.**

### The conflict, and its resolution

Both PRs insert a `CommonIndexes` line at the *same* anchor in
`internal/core/schema.go`:

```go
`CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id);`
      ├── #88 adds: idx_raw_memories_memory        ON raw_memories(memory_id)
      └── #95 adds: idx_memories_confidence_checked ON memories(confidence_checked_at)
```

The conflict is **symmetric** — it occurs in both orders and **cannot be
avoided by reordering**. It was reproduced in both directions with
`git merge-tree`.

**Resolution: keep BOTH index lines.** The conflict hunk is a single block
inside the `CommonIndexes` var; concatenating both sides resolves it. The
two `SafeMigrations` entries sit at *different* anchors (#88 inserts
`{"raw_memories","memory_id","TEXT"}` mid-list; #95 appends
`{"memories","confidence_checked_at","INTEGER"}` at the end) and never
conflict.

This resolution was applied and verified in the validated integration
checkpoint: both index lines are present at `internal/core/schema.go:1005`
and `:1008`, both migrations at `:1472` and `:1628`, and the merged tree
passes the full race gate.

## 4. Dependencies and interaction risks

| # | Interaction | Assessment |
|---|---|---|
| 1 | **#86 + #95 — confidence semantics** | #86 excludes `source_group = 'auto_capture'` (provenance-only) evidence from scoring; #95 adds a decay pass. Both are coherent **only if the decay path reads the filtered evidence set**. Verified in the merged tree: #86's `scoringEvidenceClause` is applied inside `loadEvidenceForRecompute`, which is the function #95's decay pass reads. Provenance-only evidence is therefore excluded from both scoring *and* the decay anchor. **Merge #86 before #95.** |
| 2 | **#85 + #88 — shred boundary vs shred linkage** | #85 makes the shred operation operator-only at the tool boundary; #88 adds a durable `raw_memories.memory_id` link so a shred can remove the staged copy without matching on text. Different concerns, same handler. Git resolves cleanly; #88's own PR body anticipated "whichever merges second needs a trivial rebase". **Merge #85 first** so #88 is the one rebased. |
| 3 | **#96 + #85 — tool-surface shape** | #96 adds an error path to `handleSaveToMemory` and a registry description; #85 rewrites the memory/lesson handlers and several registry descriptions. Disjoint hunks. **Merge #96 first** so #85's larger registry edits land on the final shape. |
| 4 | **#86 + #98 — `internal/core/db.go`** | #86 edits a comment block near line 3580; #98 adds a schema-init lock near line 2535. ~1000 lines apart. No interaction. |
| 5 | **#98 — concurrent schema initialisation** | Independently important: every CLI/MCP invocation opens its own `DatabaseManager`, so concurrent opens on a fresh database can fail with `SQLITE_BUSY_SNAPSHOT`, which `busy_timeout` does not retry. Its own fixture (`schema_init_concurrency_test.go`) pins the contract. |
| 6 | **#89 → #90/#91** | #90's and #91's PR bodies record that their CI runs were taken with an ambient `MPM_PROVENANCE_FRAMEWORK` set, citing #89. **Merge #89 first** so their gates are representative. |
| 7 | **#97 — retention exemptions** | Interacts with #95's decay only in that both touch `docs/SPEC.md`, at distant anchors. No behavioural coupling. |
| 8 | **#99 — installer** | `install.sh` only; no Go code. It advertises `mpm call mpm_context --payload '{"action":"read_wake_context"}'`. Independent of the other twelve. |

### Does combining the PRs invalidate anything tested in isolation?

**Checked, and the answer is no** — with one caveat that the checkpoint
harness already exercised:

- The twelve-way merge was performed for real in a disposable worktree with
  the single conflict resolved by keeping both lines.
- `make build` → ✅ all five binaries.
- `make test-race` → ✅ **24/24 Go packages, 0 data races** (including
  `cmd/mpm` 397 s and `mpm-core` 238 s).
- `make test-scripts` → 1 failure, which was **proven pre-existing** by
  reproducing it identically on pristine `origin/main`. It is the
  signal-interruption flake that **PR #103 fixes**.

**Caveat:** the checkpoint (`integration/pre-release-12`) is a *validation
artifact*, not a baseline. Its content was assembled from the PR heads as
they stood; it must be **rebuilt from the reviewed, merged heads** before the
final integration gate. It must not be deployed.

### Security, schema-migration and backward-compatibility implications

| PR | Implication |
|---|---|
| #85 | **Security-relevant.** Restricts a destructive operation (permanent memory/lesson deletion with no restore path) to operator level. Widening this to agents would be a privilege escalation; narrowing it is a deliberate reduction. |
| #86 | Behaviour change to confidence scoring. Existing stored confidences are not recomputed; the effect is on the next recompute. No schema change. |
| #88 | **Schema migration.** Adds `raw_memories.memory_id` (TEXT, nullable). Backward compatible: NULL for rows that predate it and for rows never promoted. Adds a `SafeMigrations` entry and an index. |
| #95 | **Schema migration.** Adds `memories.confidence_checked_at` (INTEGER, nullable) plus an index. Nullable and defaulted, so backward compatible. |
| #97 | Retention policy change: standing directives are exempt from access-based pruning. Deliberate, documented. |
| #98 | Concurrency safety. Adds a file lock around schema initialisation. **Must not** be merged in a way that reintroduces a `db.SetMaxOpenConns(1)` deadlock — see the repo's own single-connection discipline note. |
| #96 | Behaviour change: `mpm_memory action=save` now refuses to silently discard an `evidence` parameter. Existing callers that relied on the silent discard will now see an error — a deliberate honesty fix. |

## 5. Dependency-aware classification

### 5.1 Independent — suitable for early integration

`#87` (doctor diagnostics), `#89` (test isolation), `#90` (gc reporting),
`#91` (ingest undo), `#97` (retention), `#98` (schema-init lock),
`#99` (installer).

Each touches a disjoint set of files from every other PR. `#99` touches only
`install.sh` and one Python contract test. `#98` is the exception in
*importance*: it must be integrated before any concurrent-initialisation
testing is meaningful.

### 5.2 Ordered behind prerequisites

| PR | Behind | Why |
|---|---|---|
| `#90`, `#91` | `#89` | their CI was recorded under an ambient env that #89 removes |
| `#95` | `#86` | the decay pass must read provenance-filtered evidence |
| `#88` | `#85` | #88's body names #85's handler rewrite |
| `#85` | `#96` | #85's larger registry edits land on #96's final shape |

### 5.3 Requiring conflict resolution

**`#95`** — one hunk in `internal/core/schema.go`, resolved by keeping both
index lines. No additional test needed: the resolution is additive (two
independent `CREATE INDEX IF NOT EXISTS` statements) and the merged tree was
built and race-tested.

### 5.4 Requiring security or data-integrity review

**`#85`** — it narrows the blast radius of a destructive operation. The
review must confirm that the operator-only boundary is enforced at the
*transport* layer and not merely in the CLI handler, and that no agent path
can reach the delete.

**`#98`** — it introduces a file lock in the schema-initialisation path.
The review must confirm the lock is released on every exit path (including
SIGINT), that it cannot deadlock against the SQLite connection pool, and
that it does not reintroduce the `SetMaxOpenConns(1)` pattern the codebase
explicitly forbids.

**`#88` + `#95`** — the two schema migrations. Together they add two
nullable columns and two indexes. The review must confirm the migrations are
idempotent and safe on a database created by an older build.

### 5.5 Possibly superseded or obsolete

**None of the twelve.** Every one is `OPEN`, `MERGEABLE`, and its head SHA
matches its local branch. `#99` was CI-red at `18dd9f1`; it is now green at
`0f67fcb` with a corrected contract test.

The only *near*-obsolete artefacts are the two integration checkpoints
(`hal9001/r3-integration` at `cbe6af7`, and `integration/pre-release-12` at
`a8c3b60`), which will be superseded by an integration rebuilt from the
merged heads. Both are preserved as evidence until then.

## 6. Recommended merge sequence

```
PRs to merge first (unblock the gates):
  #103  signal-test determinism      ← removes a flake that makes ANY PR's
                                       "Build + core tests" job red
  #104  CI gate integrity            ← stacked on #103; makes full-suite real
  #102  mpmcli environment isolation

Then the twelve corrective PRs, in this order:

  1. #89   provenance test isolation   (independent; unblocks #90/#91's gates)
  2. #90   gc reporting honesty        (independent)
  3. #91   ingest undo honesty         (independent)
  4. #87   embedding-migration doctor  (independent)
  5. #96   reject evidence on save     (before #85: smaller tool-surface edit)
  6. #85   operator-only shred         (before #88: #88 rebases onto it)
  7. #88   staged-row shred linkage    (before #95: conflict is resolved here)
  8. #86   provenance-only evidence    (before #95: decay reads filtered evidence)
  9. #95   confidence decay            ← THE ONE CONFLICT, with #88
 10. #97   directive retention         (independent)
 11. #98   schema-init serialisation   (independent; concurrency-critical)
 12. #99   installer next steps        (independent; green)
```

### Rationale, stage by stage

| Stage | Why here | Verification gate |
|---|---|---|
| #103 → #104 → #102 | #104 is stacked on #103. #103 removes a **real, 4-of-4 reproducible** flake from `origin/main` that makes the "Build + core tests" job intermittently red for *any* PR. Merging it first stops the whole queue from having false failures. #104 then restores the full-suite gate. | `make test-race` on `main`; the "Full test suite" job must be observed failing on a deliberately broken test (PR #104's own regression test covers the property) |
| 1–4 | No shared files with each other; #89 first because #90/#91's recorded CI depends on it | `make test-scripts` + `go test -race ./cmd/...` |
| 5–9 | The dependency-ordered chain. #96→#85→#88→#86→#95 is the only ordering with any semantic weight. | **#95's merge is the only one needing human resolution** |
| 10–12 | #97 and #98 touch disjoint files; #98's concurrency fixture is more meaningful once #96/#97 are in. #99 is independent. | full `make test-race` at the end |

### The one conflict, precisely

When merging **#95** (after #88), `internal/core/schema.go` will conflict in
the `CommonIndexes` block. Resolve it by keeping **both** new index lines:

```go
`CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id);`,
`CREATE INDEX IF NOT EXISTS idx_raw_memories_memory ON raw_memories(memory_id);`,          // #88
// Supports the confidence-decay pass's due-artifact page query
// (ordered by the watermark, bounded by LIMIT).
`CREATE INDEX IF NOT EXISTS idx_memories_confidence_checked ON memories(confidence_checked_at);`,  // #95
```

Do **not** drop either line, and do **not** reorder the `SafeMigrations`
entries — they are at different anchors and need no change.

## 7. Verification gate after every merge

1. `git fetch origin && git rebase <pr-branch> origin/main` — a rebased head
   must be re-verified, not assumed.
2. `make build` — must produce all five binaries.
3. `make test-race` — the canonical pre-merge gate. All Go packages pass,
   zero data races.
4. `make test-scripts` — the installer-contract suite.
5. `make test-agent-installation` — reached by `make test-race`.
6. Confirm the CI "Build + core tests", "Full test suite" and "Release gate"
   checks are green **on the PR's actual head SHA** — a green check on an
   older head is not evidence.
7. Only after the twelfth merge: rebuild the integration from the merged
   heads, re-run the full gate on the rebuilt tree, and only then consider
   milestone promotion (see `dogfooding-runbook.md` §6).
