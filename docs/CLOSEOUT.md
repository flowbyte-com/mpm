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

### 1.1 `mpm onboard` host inspection depends on the working directory — FIXED in this closeout

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

Regression coverage: `resolveRepoRelative` and `locateHostInspector` had
no tests at all before this closeout.

### 1.2 Memory embeddings carry no model fingerprint — FIXED in this closeout

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
refusing to blend vectors from different models. See §5 for the
deliberate limitation that remains.

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

`HybridSearch` calls `ChallengeMemory()` at cosine ≥ 0.85
(`docs/RECALL-MUTATION.md`) and **no env var disables it**. A "read-only"
`--semantic` recall can durably write challenge state. All benchmarking
therefore runs against a snapshot copy with a before/after digest. Any
future agent benchmarking MPM must do the same.

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
| **Legacy memories predate the fingerprint.** The 149 existing memory embeddings were written before §1.2 and carry no model identity; only `embedding_dimension` exists and it is NULL for all of them. They cannot be retroactively fingerprinted with confidence. New writes are fingerprinted; existing rows are backfilled on next re-embed. | Backfilling requires a full re-embed against a chosen model — which is a separate, explicitly-authorised operation, not a code fix. |
| **Reference-chunk embeddings are model-aware, memory embeddings were not.** `reference_db.go` filters by model; `db.go` did not. §1.2 closes the memory side. | Resolved by §1.2. |
| **A load-receipt mechanism for Pi / OpenCode** (§3.1) — the adapter would need to emit a positive signal at session init. | Requires host-side cooperation in two external projects. |
| **Lexical path still sorts by `created_at DESC` after ordering by BM25** (`recall.go`), discarding the lexical ranking. The preserved `lexicalRank` is surfaced by `--why` so the divergence is visible. | Primary-ordering change is a policy decision, not a correctness fix. |
| **`norerelevant` queries still return 10 results** under `--semantic`. | Consequence of §4.1, deliberately accepted. |

---

## 6. Onboarding / deployment state at closeout

| Item | Value |
|---|---|
| Source revision | `6737f2e` on `fix/retrieval-benchmark-findings` (pushed, not merged) |
| Installed binary | `~/.mpm`, sha256 `6e673287b489763d` — matches source build |
| Database | integrity `ok`; 149 memories, 149 embeddings, 692 scheduled wakes preserved |
| Embedding model | `ollama:all-minilm`, 384-dim — **unchanged** |
| MCP | `initialize` responds |
| Scheduler | restarted 14:17:08 on the new binary |
| Managed blocks | 4 hosts reconciled idempotently |
| Pi `-builtin:mcp` | byte-identical, not touched |
| Rollback | `~/.mpm/backups/pre-retrieval-fix-20261013-141648` |

No embedding backfill or model migration occurred.
