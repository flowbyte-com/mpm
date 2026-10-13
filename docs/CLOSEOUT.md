# MPM Closeout Ledger

Scope: every defect reported against MPM across the 2026-09-13 through
2026-10-13 audit passes, resolved to one of four outcomes and classified.
This exists so the next agent does not have to re-derive any of it from
conversation history.

Baseline revision: `6737f2e` on `fix/retrieval-benchmark-findings`
(pushed, not merged). Installed environment `~/.mpm` is on the same
revision.

**Classification key**

| Outcome | Meaning |
|---|---|
| **FIXED** | Defect was real; code changed and a regression test pins it. |
| **WORKING** | Investigated; no defect exists. The concern is refuted with evidence. |
| **LIMITATION** | Real behaviour that is a deliberate trade-off. Not changed; documented so it is not re-reported as a bug. |
| **DEFERRED** | Real, but does not block AgentShell development. |

---

## 1. Confirmed unresolved defects

### 1.1 `mpm onboard` host inspection depends on the working directory — FIXED (`e27d593`)

**Severity:** medium. The headline onboarding command reports hosts as
"inspection unavailable" unless it is run from inside a source checkout.

`cmd/mpm/onboard_cmds.go:1026` builds a repo-relative path to
`agent_installation/scripts/check_installed_managed_blocks.py`, and
`resolveRepoRelative` searches `MPM_REPO_ROOT`, then up to 8 levels from
the executable, then up to 8 levels from the CWD. An installed binary at
`~/.mpm/bin/mpm` satisfies none of those unless invoked from
`/home/v/src/mpm`.

Root cause is a **packaging gap, not a resolution bug**. The runtime root
`~/.mpm/agent_installation/scripts/` contains only `__pycache__`; the
four `*.py` files and `stage_runtime_package.sh` are never staged there.
`stage_runtime_package.sh` stages only *per-adapter* payloads — each
adapter declares its own list (e.g. `PI_RUNTIME_MANIFEST` in
`agent_installation/mpm-pi/install.sh:113`, listing `index.ts`,
`src/workspace.ts`, `src/resolve-mpm-binary.ts`) — and
`install_runtime_assets.py`'s `ASSET_DIRS` covers only `{mode, persona,
drills}`.

The fix must stage **two** things, not one:

1. `agent_installation/scripts/*.py` → `<runtime-root>/agent_installation/scripts/`
2. each adapter's `reconcile.json` → `<runtime-root>/agent_installation/<adapter>/reconcile.json`

(2) is easy to miss and fails silently in a way that looks worse than the
original bug. `check_installed_managed_blocks.py:238-249` reads
`AGENT_INSTALLATION/<adapter>/reconcile.json` and **fails closed to
ABSENT** when the manifest or the reconciler module is missing. Staging
`scripts/` alone would therefore convert "inspection unavailable" into
"zero hosts installed" — a false negative on every host. Verified: no
`reconcile.json` is currently present anywhere under `~/.mpm`.

Regression coverage: `scripts/tests/test_onboard_runtime_assets.py` (8
tests). Six of them fail against the pre-fix code; verified by stashing.

**Fix shipped in `e27d593`.** `install_runtime_assets.py` now stages the
whole transitive set. Per-adapter manifests, templates and installer modules
are discovered by **scan** rather than enumerated, so adding an adapter
needs no edit there — a static list is what made the gap invisible.

Verified end-to-end: run from `/`, all four hosts report the managed block
present and byte-matching canonical render, where all four previously
reported unavailable:

```
✓ Claude Code    ✓ OpenClaw    ✓ OpenCode    ✓ Pi
```

That run also independently corroborates §3.1 — Pi's and OpenCode's managed
blocks are byte-current on disk. It still says nothing about whether those
hosts *load* the extension at runtime, which is the axis that remains
unverified.

### 1.2 Memory embeddings carry no model fingerprint — FIXED

**Severity:** high for data integrity, latent until a model switch.

The `memories` table records `embedding_dimension` but never writes it —
**0 of 149 rows are populated**. `reference_chunks` does it correctly:
all 232 embedded chunks carry `embedding_model='ollama:all-minilm'` and
`embedding_dimension=384`.

Worse, the memory load path filters on neither field.
`internal/core/db.go:4234`:

```sql
SELECT id, content, embedding, created_at FROM memories WHERE embedding IS NOT NULL
```

Compare `internal/core/reference_db.go:588`, which at least coalesces
`embedding_dimension`.

This is exactly the failure the request named — **a dimension match
alone cannot establish embedding-model compatibility**. Two models can
share a dimension while producing incomparable vector spaces, and a
dimension-only check would pass them as interchangeable. Worse, the
current path performs *no* check: mixing models silently corrupts every
cosine similarity rather than failing loudly.

Fixed by recording the generating model on every memory embedding and
filtering retrieval on it. See §5 for the deliberate limitation that
remains.

**What changed:**

- `memories.embedding_model` added by migration, mirroring
  `reference_chunks.embedding_model`.
- All three memory write paths now record the fingerprint:
  `MemoryStore.UpdateMemory` (`internal/core/memory.go`), the backfill
  (`cmd/mpm/backfill_embeddings.go` — this was the site that left all 149
  rows NULL, since it wrote only the vector), and the insert path.
- `VectorSearch` filters `memories` on the active model
  (`internal/core/db.go`). `sessions` and `topics` have no such column and
  are unfiltered, which is stated in the code rather than left implicit.
- Doctor reports the **unlabelled-row count**, not just column presence
  (`service_doctor_embedding_integrity.go`). Column-exists and rows-are-
  labelled are different facts, and on the live database the column existed
  while all 149 rows were still unlabelled.

Regression coverage: `internal/core/embedding_model_compatibility_test.go`.
Three of its tests fail against the pre-fix code — verified by stashing.
The most important is `TestVectorSearchExcludesUnfingerprintedRows`, which
demonstrates the defect behaviourally: pre-fix, an unfingerprinted vector
**was** returned by `VectorSearch`.

### 1.3 The operational consequence — READ THIS BEFORE DEPLOYING

**After this change, semantic retrieval over `memories` returns nothing on
the live store until a re-embed runs.**

All existing memory rows predate the column and carry NULL
`embedding_model`. The compatibility filter excludes them — correctly,
since an unrecorded model cannot be proven comparable.

Measured on a `sqlite3 .backup` snapshot of the live database with the
migration applied:

| | count |
|---|---|
| `memories` rows (total) | 149 |
| soft-deleted (`deleted_at IS NOT NULL`) | 3 |
| **live** | **146** |
| live rows carrying a vector | 146 |
| live rows labelled with the active model, post-migration | **0** |
| rows the new pending predicate selects for backfill | 146 |

(The 3 deleted rows are also the 3 holding a literal `'null'` vector, so
they were already excluded before this change.)

The filter therefore empties the candidate set completely, and the WARN's
`labelled == 0` guard fires as designed:

```
$ mpm recall --semantic "how do I stop the attachments from breaking"
WARN VectorMatch: every embedded memory is excluded by the embedding-model
     compatibility filter  active_model=ollama:all-minilm unlabelled_rows=146
     remedy="run `mpm ops backfill-embeddings` to re-embed them under the
     active model and record the fingerprint"
No memories found for: how do I stop the attachments from breaking
```

Lexical (`mpm recall` without `--semantic`) is unaffected and still works.

This is surfaced as a **WARN with a remedy** rather than a silent empty
result, because the silent version reads like an empty store and sends the
operator looking in entirely the wrong place. The doctor reports the same
count via `mpm doctor`.

Two things make the transition safe:

- The filter only applies when a provider is configured. With no active
  provider (`activeModel == ""`) nothing is filtered, so a store is never
  locked out of retrieval by a missing profile.
- New writes are fingerprinted at INSERT (`saveMemoryRow`), so the
  unlabelled population only shrinks from here.

**Required follow-up:** run `mpm ops backfill-embeddings` against the live
store. That has **not** been done — it is a full re-embed and was
explicitly outside the authorisation for this work. Until it runs,
semantic recall is degraded on the live store. **Lexical recall, MCP, and
all other substrate functions are unaffected.**

### 1.4 The backfill could not have fixed it — a gap the filter exposed

**Severity:** high, and it would have shipped as a permanent silent outage.

Adding the column and the filter is only half the change. The other half is
making the existing rows *recoverable*, and they were not: no command could
ever fingerprint them.

`countMemoriesWithoutEmbedding` selected only
`embedding IS NULL OR embedding = 'null'`. Every pre-existing row already
has a vector, so on the live store that predicate matches **0 of 149** — the
command prints `Total memories missing embeddings: 0` and `Nothing to do.`,
forever. The rows the filter excludes are precisely the rows the only
remedy skips. Semantic recall over memories would have been filtered down to
nothing with no way back, and the WARN would have pointed at a command that
reports nothing to do.

There was a second defect in the same place: the documented remedy,
`mpm ops backfill-embeddings --force`, names a flag that **has never
existed**. The real flag is `--force-reembed`, and it was scoped
"references pass only" — so neither spelling of the remedy would have
worked. The same wrong flag was referenced from `onboard_cmds.go:801` and
the doctor's no-column branch, meaning the mistake predates this change and
had simply never been executed.

Fixed by making the memories pass model-aware, symmetric with the references
pass that was made model-aware on 2026-09-28:

- `memoryNeedsEmbedding(force, hasModelCol)` is now the single definition of
  "pending": a row needs embedding if its vector is null **or** it carries
  no model fingerprint. A vector with no recorded model is not comparable,
  and the only honest way to make it comparable is to re-embed it under a
  known provider.
- `--force-reembed` now applies to both passes.
- `hasModelCol` falls back to the legacy null-only predicate on a schema
  predating the migration, so an old fixture degrades instead of erroring.
- The command converges: a fully fingerprinted store reports 0 pending, so
  it does not re-embed on every run.

### 1.5 The backfill loop skipped a third of every run

**Severity:** high, and silently. Found only by reading the loop rather
than the predicate.

`runMemoryBackfill` paginated with `LIMIT ? OFFSET ?`. Every successful
`UPDATE` makes the row **stop matching the pending predicate**, so the
result set shrinks underneath the cursor while the offset keeps
advancing — one batch's worth of rows is skipped per iteration, with no
error raised.

Replaying the exact old loop in a test over 12 rows at batch size 4:

```
OFFSET pagination visited 8 of 12 rows (skipped: of00004 of00005 of00006 of00007)
```

Scaled to the live store's 149 rows at the default batch size of 50:

| Iteration | Fetched | Note |
|---|---|---|
| 1 | rows 1–50 | stamped; 99 remain |
| 2 | rows 101–149 (49) | `OFFSET 50` over 99 remaining — rows 51–100 never fetched |
| 3 | — | `OFFSET 100` over 50 remaining returns nothing; loop ends |

**99 of 149 fingerprinted, 50 skipped, exit status 0.** The command would
have reported success, written a correct-looking completion line, and left
50 memories filtered out of semantic retrieval.

This bug predates the fingerprint change — it is only reachable now because
the predicate finally matches rows. That is worth stating plainly: the
fingerprint work did not create it, but it converted a latent defect into
a live one, and shipping the column without fixing the loop would have
activated it.

Fixed with keyset pagination on `(created_at, id)`, which is stable under
mutation — already-processed rows fall out of the result set without moving
the cursor. The loop also now reports a count mismatch if the pending set
changed while it ran, so a future divergence surfaces instead of passing.

Regression coverage: `TestBackfillPaginationVisitsEveryRowOnce` (every row
visited exactly once, and the run converges), plus
`TestBackfillOffsetPaginationWouldSkipRows`, which replays the old loop and
**fails if OFFSET ever stops skipping** — so if a future change makes the
keyset cursor unnecessary, the test says so rather than leaving a stale
justification in place.

Regression coverage for §1.4: `cmd/mpm/backfill_embeddings_selection_test.go`.
It demonstrates the predicate regression rather than asserting it — it runs
the *old* predicate against the same rows and fails if that old predicate
already covered the case, because then the test would be pinning nothing.
Measured: **the legacy predicate selects 1 of 3 pending rows in the fixture,
and would select 0 of 149 on the live database.**

> A third latent bug in the same area, also fixed: `saveMemoryRow` — the
> dominant memory write path — recorded no fingerprint either, so every
> *newly created* memory would have been invisible to semantic retrieval the
> moment it was written. Caught by
> `TestAgentShellQuery_SemanticOnlyRowsAreLabelled`, whose fixture seeds
> vectors through a raw `UPDATE`.
>
> And a fourth, found by reviewing the filter's blast radius rather than
> only its behaviour: it was initially applied to `VectorSearch`, which is
> **not** the path `mpm recall --semantic` takes. `HybridSearch` calls
> `VectorMatch`, which falls back to `IVFSearch`. Filtering only
> `VectorSearch` left the real path unprotected while the unit test still
> passed — the defect would have been reported as fixed and shipped live.

One pre-existing test asserted the *absence* of `memories.embedding_model`
— it had pinned the defect rather than the requirement. It now asserts the
per-row fact, which holds on both old and new schemas, and a companion test
guards the new count against over-reporting.

### 1.6 The suite could report green while these fixes were never tested

**Severity:** medium, and it invalidates the Phase 4 evidence — which is
precisely how it was caught.

The first clean-worktree `make test` passed (exit 0, 23 packages). But
reading the log rather than the exit code showed **five of the new tests
had SKIPPED**:

```
--- SKIP: TestVectorMatchExcludesUnlabelledRows
    embedding_model_compatibility_test.go:333: no embedding provider configured
    in this test environment
```

Five skips — the entire behavioural core of the fingerprint fix — and the
suite still reported success.

Cause, in two layers:

1. `DefaultEmbeddingConfig()` caches the resolved provider in a
   process-wide `sync.Once` (`embeddings.go:562`) with **no reset hook** —
   unlike `ResetActiveEmbeddingDimensionForTest` for the dimension cache.
   Whichever test resolves config first decides the provider for the entire
   process.
2. Tests in this package run under a temporary `MPM_WORKSPACE` containing
   no `mpm_config.json` (`active_state_test.go:38` sets one, and
   `aux_log_isolation_test.go:65` unsets it outright with no restore). Under
   such a workspace the config resolves to `NullProvider`, the fingerprint
   comes back empty, and the tests skip. Reproduced directly:

   ```
   $ MPM_WORKSPACE=/tmp/emptyroot go test -run TestEmbeddingFingerprintNamesTheModel
     active fingerprint: model="" dimension=0        ← provider gone
   ```

Fixed:

- Added `ResetDefaultEmbeddingConfigForTest()`, mirroring the existing
  `ResetEmbedConfigForTest`. Pair it with
  `ResetActiveEmbeddingDimensionForTest`, since the dimension cache is
  populated from the config being replaced.
- `useActiveFingerprint` now pins `MPM_WORKSPACE` to default resolution
  (unset, restored on exit) so ambient leakage cannot decide whether these
  tests run.
- `TestEmbeddingFingerprintNamesTheModel` no longer passes vacuously. It
  previously asserted only that `model` and `dimension` were mutually
  consistent, which an absent provider satisfied — so it passed with
  `model="" dimension=0` while asserting nothing. It now requires a real
  provider and fails loudly if the reset helper is not effective.

**The general lesson, and the reason this is filed as a defect rather than
an observation:** ordering-dependent coverage is the same defect as missing
coverage. A green suite is not evidence that the fix is tested; only
reading the test log for `SKIP` is. Every `SKIP` in the canonical run was
inspected — the others (`TestCriticSubprocess_*`, `TestTelemetrySubprocess_*`,
`TestDrillE2E_ClaudeCode`, `TestHelpTopics_*`) are pre-existing and
unrelated to this work.

The same blind spot has a second face: §1.8, where the filter itself broke
six provider-gated tests that the canonical suite never ran.

### 1.7 The embedding config is resolved once per process — known limitation

`DefaultEmbeddingConfig()` is cached in a `sync.Once` for the lifetime of
the process. For short-lived CLI invocations that is invisible. For the
long-lived daemons (`mpm-scheduler`, `mpm-mcp`) a change to the embedding
profile does not take effect until the process restarts.

This is **documented intent**, not an accident — the resolution order is
spelled out in the function's doc comment and cites a design spec. It is
recorded here so it is not re-reported as a bug, and because it interacts
with §1.6: the cache is why test ordering could silently disable the
provider.

### 1.8 Every test touching memory vectors skips without a provider — the filter's blast radius was invisible

**Severity:** medium. Caught by running the affected tests, not by the suite.

Enabling the compatibility filter (§1.2) broke **six tests** across
`internal/core/hybrid_search_vector_only_test.go`:

```
--- FAIL: TestHybridSearch_VectorOnlyCandidatePreserved (0.07s)
        vector-only candidate MUST be returned after the merge fix
--- FAIL: TestHybridSearch_VectorOnlyAtVWOne (0.14s)
--- FAIL: TestHybridSearch_BothChannelsMerge (0.14s)
--- FAIL: TestHybridSearch_VectorOnlyThresholdFilter (0.13s)
--- FAIL: TestHybridSearch_VectorOnlyFieldsPopulated (0.13s)
--- FAIL: TestHybridSearch_VectorOnlyChallengeSignalPopulated (0.11s)
```

Confirmed caused by this change, not pre-existing: the same seven tests
all PASS on `6737f2e` (the commit before `12ce499`) with the provider
enabled.

Each of those tests seeded its vector with a raw

```sql
UPDATE memories SET embedding = ? WHERE id = ?
```

which leaves `embedding_model` NULL — so the filter correctly excluded
the corpus and the merge had nothing to merge. The failure message points
at the merge, not at the filter, which is what makes this class expensive:
it reads like a regression in the thing under test.

The reason the canonical suite reported green throughout is the same one
as §1.6: **every one of these tests skips when no embedding provider is
configured** (`if provider.Name() == "null" { t.Skip(...) }`). `make test`
runs without `OLLAMA_ENDPOINT`, so none of them ran. The canonical suite
has never exercised them, and never will, unless the runner provides a
provider.

This is now the fourth file to need the same treatment. The pattern was:

| File | Fixed |
|---|---|
| `hybrid_search_test.go` | `12ce499` |
| `hybrid_search_federated_test.go` | `12ce499` |
| `agentshell_query_fixture_test.go` | `12ce499` |
| `hybrid_search_vector_only_test.go` | this commit |

Each now carries an identical file-level contract note stating that rows
expected to be **returned** must carry `embedding_model =
CurrentEmbeddingFingerprint().Model`, that rows expected to be **excluded**
must say which reason they exercise (`unlabelled` and `hash`-sourced are
different exclusions), and that the seeding goes through a single
`stampTestEmbedding` helper rather than a hand-written `UPDATE` per site —
so the next person adding a fixture cannot silently re-introduce the gap.

**Deliberately not changed:** the `t.Skip` guards themselves stay. Requiring
a live provider for the canonical suite would make `make test` depend on a
running Ollama, which is worse. The gap is recorded rather than papered
over, and the affected tests are now verified in an explicit provider-backed
run (`OLLAMA_ENDPOINT` + `OLLAMA_MODEL=all-minilm`) as part of closeout
verification. Note the model name is `all-minilm`, not `ollama:all-minilm`;
the qualified form 404s against the Ollama API.

**The transferable lesson:** a filter added for correctness will silently
invalidate every fixture that hand-writes rows around the schema, and those
fixtures are usually the ones guarded by environment-dependent skips. The
affected tests must be run *with the dependency present* before the change
can be called verified — a passing suite that skipped them says nothing.

#### Verification: both modes

| Run | Provider | Result |
|---|---|---|
| `internal/core`, canonical | none | 1959 PASS / 9 SKIP / 0 FAIL |
| `internal/core`, provider-backed | `OLLAMA_MODEL=all-minilm` | **1964 PASS / 4 SKIP / 0 FAIL**, 153.7s |
| `internal/core`, `-run TestHybridSearch_` | provider | **7 PASS / 0 SKIP** |
| `cmd/mpm`, provider-backed | `OLLAMA_MODEL=all-minilm` | **1053 PASS / 3 SKIP / 0 FAIL** |
| `cmd/mpm`, canonical | none | ok, 332s (exit 0) |

The two modes are complementary, and the delta is exactly accounted for:
the six `TestHybridSearch_*` tests move from SKIP to PASS, and one test —
`TestReferenceHybridSearch_NoEmbeddingsFallback` — moves the other way,
because it asserts the *absence* of embeddings and therefore cannot run in
a process where a provider has already been resolved (the `sync.Once`
cache of §1.7 makes that unavoidable). It is covered by the canonical
run. Neither mode is a superset of the other, which is why both are
recorded rather than just the green one.

The 4 remaining provider-mode skips are `TestDrillE2E_ClaudeCode`,
`TestHITLStream_AppendNeverFailsTheCaller`,
`TestAffinityRebuild_RunsAgainstRealOnDiskDB` and the reference-fallback
test above — all pre-existing and unrelated to this work.

### 1.9 Onboarding reported a schema fact that had become false — FIXED

**Severity:** low functionally, high in trust. Found only by *running the
deployed binary*, after both suites were green.

The post-deploy `mpm onboard --check`, run from `/`, printed:

```
Not verified by onboarding
  · embedding model identity of already-stored memory vectors: `memories`
    has no embedding_model column, so changing the model leaves
    mixed-generation vectors undetectable.
```

The column existed. The migration had been applied by that very deploy —
`SELECT COUNT(*) FROM pragma_table_info('memories') WHERE
name='embedding_model'` returned 1. The sentence was a **hardcoded
constant** in `cmd/mpm/onboard_cmds.go`, true when written and false after
`12ce499`, and nothing compared it against the store it described.

Two things make this worth more than a cosmetic string fix:

1. **It misdirects.** The remedy it names —
   `backfill-embeddings --force-reembed` — is the wrong one for the state
   the store is actually in. The live database has the column and 146 live
   rows with no label, so the correct command is plain
   `mpm ops backfill-embeddings`. An operator following the report would
   have run the `--force-reembed` variant, which re-embeds every row
   regardless of fingerprint — strictly more work, and it re-embeds the
   3 already-correct rows too.

2. **It hides the actual state.** The per-row situation — 146 of 146
   unlabelled, meaning semantic recall over `memories` returns nothing
   right now (§1.3) — was nowhere in the report. That is the single most
   operationally important fact about the store post-deploy, and the one
   line that should have carried it was asserting something else.

A report that misstates the substrate is worse than one that stays silent,
because the operator has no way to tell which lines to trust.

**Fixed:** the text is now derived from the live store via
`memoryVectorFingerprintState`, covering the three states that need
different responses — no column (pre-migration), column present with
unlabelled rows (migrated, backfill outstanding — names the count and the
correct remedy), and fully labelled (nothing outstanding). An unavailable
database reports "not checked" rather than reading as healthy.

**Regression coverage:** `cmd/mpm/onboard_fingerprint_report_test.go`. The
original was invisible to the suite precisely because it was a constant, so
the tests assert against seeded stores in each state, and
`TestOnboardFingerprintLineNeverClaimsAMissingColumnOnAMigratedStore`
pins the exact regression. Verified by substituting the old constant text
back into the function: **all three state subtests fail**, each naming the
false claim. A test that cannot fail against the code it replaces is not
covering anything.

**The transferable lesson, and why this is filed as a defect rather than
an observation:** user-facing text describing a *schema or data* fact is
itself a correctness surface, and it needs the same discipline as code —
derive it from the store, and assert on it. A comment in prose silently
becomes a lie the moment a migration lands, and nothing in a green test
suite will notice.

---

## 2. Previously reported defects already fixed

All landed on `fix/retrieval-benchmark-findings` before this closeout.
Full write-up with measurements: [`RETRIEVAL-BENCHMARK.md`](./RETRIEVAL-BENCHMARK.md).

| # | Defect | Commit | Status |
|---|---|---|---|
| 1 | BM25 sign inversion in `hybridScore` — the lexical leg was inverted inside the blend | `5e0d24d` | FIXED + tested |
| 2 | `fts5`-only rows scored on raw unbounded BM25, incomparable to the 0–1 hybrid scale | `5e0d24d` | FIXED + tested |
| 3 | `recall --why` printed reinforcement/weight arithmetic nothing computes | `1865bc0` | FIXED + tested |
| 4 | Shared-scope weight tie-break unreachable behind an early return | `9167350` | FIXED + tested |
| 5 | Semantic false positives; `RetrievalThreshold: -3.0` unreachable for 0–1 cosine | `6737f2e` | LIMITATION (§4.1) |
| 6 | Lexical AND semantics drop recall when a correct term is added | `6737f2e` | LIMITATION (§4.2) |

Defect 3 was measured, not merely read: neutralising `weight=1` and
`reinforcement_count=0` across every row changed **0 of 8** orderings,
byte-identical.

---

## 3. Verification gaps

These are things nobody has proven either way. They stay open on purpose.

### 3.1 Direct host verification of Pi and OpenCode runtime loading

**This is the reconciliation of the conflicting reports.** An earlier
pass reported 925 invocations stamped `pi` and read that as proof the
extension loads. A later pass reported runtime loading as unverified.
Both were partly right, because they measured different things. Three
axes, kept separate:

| Axis | Pi | OpenCode |
|---|---|---|
| **Historical provenance** | **Verified.** 947 `tool_invocations` rows carry `framework_name='pi'` (of 2994 total; span 2026-09-17 → 2026-10-13, 162 in the last day alone). | Verified at the same level: 228 rows. |
| **Present installation state** | **Verified.** `~/.pi/agent/settings.json` registers the extension path; `index.ts` + `src/` are staged; all three staged files are **byte-identical to source** (sha256 `809fd7c73532`, `0baee0ba90b0`, `543430781b4d`). | Payload staged; a `file://` URL entry is present. |
| **Direct host verification** | **ABSENT.** | **ABSENT.** OpenCode additionally has no hermetic surface to test against. |

The distinction that resolves the conflict: `framework_name` is
**self-reported**. The adapter sets `MPM_PROVENANCE_FRAMEWORK` (see
`agent_installation/mpm-pi/src/workspace.ts:70`, which sets it from
`MPM_FRAMEWORK_ID`) and MPM records whatever the environment says. So 947
rows prove *the adapter's code path executes and reaches the binary* —
they say nothing about whether Pi loaded the extension into a session. A
load receipt is the only positive proof, and none exists.

A live Pi check during this closeout reached a 401. **That proves
nothing about loading** — per the recorded constraint that "no error
cannot distinguish loaded from silently-absent", an auth failure is
exactly the ambiguous outcome.

Neither host's runtime loading should be reported as verified until a
load receipt exists. Pi's `-builtin:mcp` setting is byte-identical to
before and was not touched.

### 3.2 Semantic recall is a mutation vector

**Resolved on 2026-10-13.** `HybridSearch` used to call `ChallengeMemory()`
at cosine ≥ 0.85, so a "read-only" `--semantic` recall could durably write
challenge state with no opt-out. All three automatic calls have been
removed; see [`RECALL-MUTATION.md`](./RECALL-MUTATION.md) for the current
contract and [`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md)
for the model-migration preconditions this unblocks.

**Further resolved on 2026-10-13.** The three paired `EnqueueContradiction`
calls went the same way, for the same underlying reason: cosine similarity
cannot distinguish a contradiction from a near-duplicate, and filing one
automatically on every read asserted a fact MPM does not have. Candidates
are now added only by `mpm ops resolve-contradictions --propose`, and are
labelled *unverified similarity candidates*. Semantic retrieval is
write-free.

The CLI still applies lexical-path access reinforcement (§4.4), so the
snapshot-copy discipline below still applies to any future agent
benchmarking MPM.

---

## 4. Deliberate architectural decisions, not bugs

### 4.1 Semantic false positives — the `-3.0` threshold stays

`RetrievalThreshold: -3.0` can never fire against a 0–1 cosine
similarity, so nonsense queries return 10 confident results. Raising the
default was **measured and rejected**, not ignored. Across 160 result
rows:

| | n | cosine similarity |
|---|---|---|
| relevant | 20 | 0.159 – 0.828 |
| noise | 140 | 0.030 – 0.307 |

The distributions **overlap** — the strongest noise (0.307) outranks the
weakest legitimate hit (0.159). A floor of 0.20 keeps 19/20 relevant and
filters 96% of noise, but every legitimate hit below 0.25 is a
**paraphrase**: "how do I stop the attachments from breaking" (0.244),
"what model does the system use for writing" (0.212), "why is my hard
drive no longer protected" (0.159) — all of which retrieve the actual
correct answer.

A floor trades confident-nonsense for paraphrase-recall failure, which is
the worse failure mode. Default unchanged; calibration recorded at
`DefaultHybridConfig` so the next agent does not re-litigate it from
scratch.

### 4.2 Lexical AND semantics — preserved

`BuildFTS5Query` joins prefix-wildcard tokens with spaces, so FTS5's
implicit AND applies. This measurably drops recall — "pi.dev upstream
catalog does not contain flash" returns 0 hits, "catalog" returns 2.
Real cost, but changing it would be a retrieval-policy redesign, not a
correctness fix. No independent evidence supports changing it.

### 4.3 Source-bucket policy — preserved

`sourceRank` (hybrid 3 > fts5 2 > vector 1) is unchanged, including its
restriction to `--semantic` only. A sign-corrected pure blend reproduced
the shipped ordering on **22/22** benchmark cases, which is why the
benchmark metrics are identical before and after the ranking fixes: the
bucket was already masking the inverted score in every measured query.
Both mechanisms existed; only the broken one was load-bearing.

### 4.4 Recall mutates memory state — by design

Plain `mpm recall` increments the recalled memory's weight by 0.5 and
stamps `last_accessed_at`. This is intentional retrieval tracking, pinned
by `internal/core/call_helpers_test.go:440`. It is distinct from the
`ChallengeMemory` hazard in §3.2.

**Operational consequence:** a "verify the live database is unchanged"
check must scope itself to rows other than those deliberately recalled, or
it will report expected tracking as drift.

### 4.5 FTS5 build discipline is centralised — WORKING

FTS5 flags live in exactly one place (`Makefile:75`, applied at
`:221-225` and `:461+`); `make test` carries them into every `go test`
invocation, including a dedicated `./internal/telemetry/...` pass (8 test
files).

A bare `go test ./...` without `-tags fts5` fails 11 tests with
`no such module: fts5`. This was confirmed **pre-existing** by stashing
all changes and re-running. It is not a regression from this work, and
`make test` is the canonical entry point.

Note: `internal/core` is a **separate Go module** — run it as
`cd internal/core && go test .`, not from the repo root.

### 4.6 Temporary-directory cleanup — WORKING

All 15 `mktemp` sites were audited. 12 clean up via `trap ... EXIT`
(stranger-test, smoke_shared, work_acceptance, smoke_telemetry,
verify-alpha-blocker, dogfood-skill-workshop, audit-feature-freeze,
session_start_hook, stage_runtime_package and others); 3 clean up
explicitly (`install.sh:786` `service_tmp`, `uninstall.sh:791`
`stage_dir`, `verify-wishlist-fixes.sh:75`). No leak.

---

## 5. Deferred enhancements

Not blockers for AgentShell work.

| Item | Why deferred |
|---|---|
| **Migrating the embedding model** — measured across four models. `nomic-embed-text` is the leading candidate (MRR 0.832, nDCG@10 0.789, paraphrase MRR 0.529 vs all-minilm's 0.260); `qwen3-embedding:0.6b` is second at ~6× the embedding cost. Original two-model findings in [`EMBEDDING-MODEL-BENCHMARK.md`](./EMBEDDING-MODEL-BENCHMARK.md), four-model comparison in [`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md). **Still recommended against, for now** — the original blocker was that `ChallengeMemory` fired at cosine ≥ 0.85 with no opt-out, so a stronger model's tighter relevant/noise distribution multiplied spurious challenges (qwen3: 27/30 queries mutating; gemma: −301 weight). That blocker is **removed** as of 2026-10-13. The remaining work is re-running the benchmark against the separated write path, then deciding. | The gain is real and the original objection is answered, but a model switch still requires an explicit decision plus a separate, backed-up full re-embedding. Neither has been authorised, and **the live store was not touched**. |
| **Backfilling the 149 existing memory embeddings with a fingerprint.** The new column is NULL for all of them; retrieval excludes them until they are re-embedded. §1.4 made this *possible* — before that fix no command could do it. | A full re-embed is an explicitly-authorised operation on live data, not a code fix. The live store is currently in this state: semantic retrieval over `memories` returns nothing until `mpm ops backfill-embeddings` runs. |
| **Legacy memories predate the fingerprint.** The 149 existing memory embeddings were written before §1.2 and carry no model identity; only `embedding_dimension` exists and it is NULL for all of them. They cannot be retroactively fingerprinted with confidence — the vector itself does not say what produced it. New writes are fingerprinted; existing rows are backfilled on next re-embed. | Backfilling requires a full re-embed against a chosen model — which is a separate, explicitly-authorised operation, not a code fix. |
| **Reference-chunk embeddings are model-aware, memory embeddings were not.** `reference_db.go` filters by model; `db.go` did not. §1.2 closes the memory side. | Resolved by §1.2. |
| **A load-receipt mechanism for Pi / OpenCode** (§3.1) — the adapter would need to emit a positive signal at session init. | Requires host-side cooperation in two external projects. |
| **Lexical path still sorts by `created_at DESC` after ordering by BM25** (`recall.go`), discarding the lexical ranking. The preserved `lexicalRank` is surfaced by `--why` so the divergence is visible. | Primary-ordering change is a policy decision, not a correctness fix. |
| **`norerelevant` queries still return 10 results** under `--semantic`. | Consequence of §4.1, deliberately accepted. |

---

## 6. Onboarding / deployment state at closeout

### 6.1 Source vs installed runtime — verified separately

Source tests passing says nothing about what is installed under `~/.mpm`.
Both were checked independently, because the fixes in §1.1 and §1.2 are
only real once the installer has staged them.

**Before deploy** (installed binary, sha256 `6e673287b489763d`, built
14:17 from `6737f2e`), running `mpm onboard report` from `/`:

```
! Host inspection  unavailable: agent_installation/scripts/ not found near
                     the binary or working directory
· memories has no embedding_model column, so changing the model leaves
  mixed-generation vectors undetectable. Re-embed with
  `mpm ops backfill-embeddings --force` after a change.
```

All three defects are present and reproducible in the installed runtime —
and the second line is the installed binary **naming the flag that does not
exist** (§1.4). That message ships to users as onboarding advice.

Staged-asset inventory, also absent pre-deploy:

| Asset | Installed | Source |
|---|---|---|
| `agent_installation/scripts/*.py` | absent | present |
| `agent_installation/*/reconcile.json` | absent (none anywhere) | present per adapter |
| `agent_installation/*/templates/*.snippet` | absent | present |
| `agent_installation/*/scripts/*.py` | absent | present |

**After deploy**, the same command from `/` must report all four hosts
present and byte-matching (§1.1 already verified this against the source
tree), and must no longer print the "no embedding_model column" line.

### 6.2 Baseline state to preserve across deploy

| Item | Value |
|---|---|
| Source revision at deploy | `eb6cf87` on `fix/retrieval-benchmark-findings` |
| Database | integrity `ok`; 149 memories (146 live, 3 soft-deleted), 232 reference chunks, 692 scheduled wakes |
| Embedding model | `ollama:all-minilm`, 384-dim — **unchanged** throughout |
| MCP | `initialize` responds |
| Scheduler | user unit `active` + `enabled` |
| Pi `-builtin:mcp` | byte-identical, not touched |
| Rollback | `~/.mpm/backups/pre-retrieval-fix-20261013-141648` |

No embedding backfill and no model migration occurred during the closeout.
The live database was confirmed byte-unchanged against a snapshot
(`sqlite3 .backup`) at every verification point: 149 memories, 232
reference chunks, 0 rows with `embedding_dimension`, and **no
`embedding_model` column at all** — the migration had not been applied.
