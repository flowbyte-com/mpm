# Session D — the doctor → review workflow, and the CLI's option contract

> **Date:** 2026-10-31. **Author:** a successor session.
> **Branch:** `fix/cli-consistency-and-review-workflow` (from `origin/main` = `4f81690f`).
> **Subject:** two related defects in what MPM tells an operator.
>
> **Read this first:** §1. The work was never merged and never deployed. It
> lives on the branch above and in the PR named in §9. Nothing in the live
> `~/.mpm` was touched.

---

## 1. The two defects

Both are the same defect seen from two sides: **MPM told the operator
something that was not true.**

### 1.1 The doctor recommended a command that cleared nothing

`mpm doctor` reported a "Review backlog" row and recommended running
`mpm ops review`. The command **only listed**. Nothing was
acknowledged, so the backlog did not move, so the warning came back —
and the operator was told to "clear the backlog" by a command that could
not.

Underneath that were four smaller lies:

| The lie | The truth |
|---|---|
| "60 memories due" | the length of a **100-candidate sample**, truncated by the query's own ordering |
| the doctor's window was **14 days** | the CLI's window was **30 days** — the operator looked at a different set than the doctor had counted |
| `--limit` was parsed and then **ignored** | the query was called with a hardcoded `20` |
| a never-accessed memory rendered as **"106751d ago"** (292 years) | the NULL timestamp went through a formatter that did not understand the `*int64` shape |

And a fifth, which is the one the brief names: **a memory created a
second ago was reported as stale.** The rule was
`last_accessed_at IS NULL OR < cutoff`, so *every* never-accessed memory
was due immediately — including a brand-new one.

### 1.2 The CLI's help and error messages did not describe the CLI

The audit (`cli_audit.py`, §6) measured three properties per command:
does `--help` exit 0; does `-h` exit 0; does an unknown option fail
visibly; does the help request **mutate** anything.

Fourteen top-level commands silently accepted unknown flags. Four of
them **mutated**:

```
mpm decide --anything     recorded a decision whose choice was "--anything"
mpm theorize --anything   proposed a theory named "--anything"
mpm gc --anything         ran the decay sweep
mpm backup --anything     wrote a SQL dump to a file named "--anything"
```

and `mpm synthesize --anything` ran its real operation. `mpm ops --zzz`
printed the ops help page and **exited 0**.

---

## 2. The review contract (Part 1)

### 2.1 One rule, one query, one place

`internal/core/review.go` is new. It owns the rule, the count, the
ledger and the window. Every operator-facing surface calls it, so they
cannot disagree.

**A memory is DUE FOR REVIEW when all of these hold:**

1. it is not soft-deleted, and it is not a directive
   (`DirectiveExclusionClause` — the canonical predicate);
2. it is durable enough to be worth reviewing: `is_long_term = 1` or
   `weight >= 5`;
3. its **review clock** is older than the window.

**The review clock is:**

```sql
COALESCE(reviewed_at, last_accessed_at, created_at)
```

read from three places, in order of authority:

| Source | What it means |
|---|---|
| `reviewed_at` | an explicit operator review — the strongest signal |
| `last_accessed_at` | the last retrieval — a **proxy**, not a review |
| `created_at` | the fallback for a memory never accessed and never reviewed |

The `created_at` fallback is what stops a brand-new memory from being
instantly due, while a genuinely old never-accessed memory still is.

`last_accessed_at` is deliberately *not* the review signal: an agent
can retrieve a memory a thousand times, and a backlog that an agent's
retrieval can silently clear is not an operator backlog.

### 2.2 The window

`ReviewDueAfterDays = 14` — **one constant**, used by the doctor, the
dashboard's readiness row, the CLI and the MCP `ReviewMemories` tool.
It was 14 in the doctor and 30 in the CLI.

### 2.3 The count is a count

`CountMemoriesDueForReview(days)` returns the TOTAL. The doctor used
`len(candidates)` over a 100-candidate sample and reported it as the
backlog size — so a backlog of 160 was reported as "60 due".

### 2.4 The acknowledgment

`mpm ops review --ack <id>` / `--ack-all`, with
`--decision keep|weaken` and `--note`.

- A review is an explicit operator act (`RecordMemoryReview`). It is
  never a side effect of displaying a memory, of retrieving one, or of
  any background sweep.
- `keep` is the honest default: the memory was reviewed and stands. It
  does **not** touch the weight, the reinforcement count or the
  content.
- `weaken` routes through the **existing** `WeakenMemory` path — the
  separately-audited operation `mpm weaken` performs — so the weight
  change is not a second implementation with its own rules.
- There is deliberately **no "delete" decision**. Deletion already has
  its own command, its own confirmation semantics and its own
  irreversibility. A second, quieter path to a destructive act is the
  kind of duplication this repository treats as a defect.

### 2.5 The data model

One new relation, `memory_reviews`:

```sql
CREATE TABLE IF NOT EXISTS memory_reviews (
    memory_id   TEXT PRIMARY KEY,
    reviewed_at INTEGER NOT NULL,
    decision    TEXT NOT NULL CHECK (decision IN ('keep','weaken')),
    note        TEXT,
    FOREIGN KEY(memory_id) REFERENCES memories(id) ON DELETE CASCADE
);
```

**Why a relation and not a column on `memories`:** the same reason
`memory_settling_baselines` is a relation. A review is written by the
operator on demand, never by the memory creation path, so a column
would widen the 19-site memory `INSERT` fan-out for a value that is
NULL on almost every row.

**Boundary:** the relation holds the **latest** review per memory (an
UPSERT), not a review history. The current review state is the
product; the history of reviews is not.

`CREATE TABLE IF NOT EXISTS` means no migration sentinel is needed, and
the table is auto-mirrored into the `shared` schema by the existing
mirror machinery.

---

## 3. The CLI contract (Part 2)

### 3.1 One declaration registers the flags AND renders the help

`cmd/mpm/clihelp.go` is new. An `OptionSpec` declares an option once;
`RegisterOptions` binds it to the parser and `RenderOptions` prints it.
The advertised set therefore **cannot drift** from the accepted set —
they are the same declaration.

### 3.2 The router's option guard

`cmd/mpm/cli_options_registry.go` is new. The router consults a
per-command option vocabulary before dispatch, so an option-looking
token that the CLI does not know is rejected **before** a handler can
mistake it for data.

The registry's direction of error is deliberate:

- an entry that lists an option the command does **not** support is the
  **safe** direction — the token reaches the leaf, which rejects it
  precisely (or ignores it, exactly as before);
- an entry that **misses** an option the command does support is the
  **dangerous** direction — a valid invocation is rejected.

So the registry is deliberately generous, and it is pinned by
`TestCLIOptionRegistryCoversEverySourceVocabulary`, which **derives**
the vocabulary from the source (every `--xxx` literal and every declared
`OptionSpec`) and fails when the registry drifts. That test found two
real breakages during this work (§5).

### 3.3 What the router does NOT do

- It does **not** preempt a command that rejects unknown options
  itself. `mpm doctor --nonsense` is rejected by the doctor with the
  full option block and a typo suggestion; `mpm provenance --bogus`
  names the supported flags; `mpm onboard --install-everything` says
  "unknown argument". The router's generic message would replace all
  three with a shorter one that carries less. The deferred set is in
  `cliOptionDeferred`, and it was established **empirically**: every
  command was invoked with an unrecognised option with the router's
  guard disabled, and the commands that still rejected it — naming the
  offending flag — are the ones listed.
- It does **not** treat a free-text positional as an option. `mpm
  remember Warning: don't forget --flags; ok?` stores five tokens of
  content, one of which begins with a dash. For the free-text commands
  (`add`, `remember`, `hint`) the scan stops at the first positional,
  which is the CLI's own convention: **options come before
  positionals**. Every other command keeps the strict scan, so `mpm rm
  <id> --bogus` is rejected rather than silently ignoring the flag.

### 3.4 Help

- `mpm <cmd> --help`, `mpm <cmd> -h` and `mpm help <cmd>` render the
  same page.
- A supported command's help exits **0**.
- `-h`/`--help` are **router-owned**: the router consumes them before
  dispatch, so a command's own flag set never registers them. They are
  still advertised.
- `-help` (single dash) is rejected with `Tip: Did you mean --help?`.
- `mpm help <known-command>` now falls back to the command's own help
  page instead of "no help available".
- A help request mutates **nothing** — proven against the database
  file's digest, every table's row count and the workspace's file list.

---

## 3A. The two corrections — this supersedes §2 and §3 where they differ

> **§3A is the SECOND cut.** Two review-workflow defects were raised
> against this branch after it went CI-green, and both changed the
> implementation. They are in **§3A**, and **§3A supersedes §2 and §3**
> where they differ. The measurements in §1 still hold.

### 3A.1 The review and the weakening were two writes

§2 made the review reachable and honest, and §3 made the CLI's option
contract truthful. But the *acknowledgment* itself was still two
separate writes:

```go
dm.RecordMemoryReview(id, decision, note)   // write 1
if decision == "weaken" {
    dm.WeakenMemory(id, 1)                  // write 2
}
```

If write 2 failed, write 1 had already landed. The memory had **left
the doctor's backlog**, and the weight the operator asked to reduce was
**untouched**. The operator's decision was half applied, and the error
message named only the half that worked:

> *"review: recorded the review of %s but could not weaken it"*

The reverse order has the mirror defect — the weight reduced and the
memory still in the backlog, so the next review would ask again.

**THE CORRECTION.** The decision and the mutation are one operator act,
so they are now **one transaction**:

```go
dm.RecordMemoryReviewsAndApply(targets, decision, note)
```

`--ack-all` is atomic as a **unit**: every review and every weakening,
or none of them. A partially applied batch is never reported as
complete success, and it is never left behind.

**THE WEIGHT CHANGE IS NOT A SECOND IMPLEMENTATION.** `WeakenMemory` was
refactored into `weakenMemoryOn(ex memExecer, ...)` and
`RecordMemoryReview` into `recordMemoryReviewOn(ex memExecer, ...)`.
Both `*sql.DB` and `*sql.Tx` satisfy `memExecer`, so the **exact same**
floor (`MAX(weight - ?, 1)`), validation, busy-error mapping and
read-back assertion run inside the transaction and outside it. The
atomic path and the standalone path are one implementation and cannot
drift apart.

### 3A.2 The self-maintenance path used a SECOND definition of eligibility

§2 established ONE canonical rule — `internal/core/review.go`'s
`reviewCandidateQuery`, with the review clock
`COALESCE(reviewed_at, last_accessed_at, created_at)` — and moved the
doctor, the dashboard and the CLI onto it. But one path was left behind:

```go
forReview, err := s.SpacedReinforcementReview(14, 100)
stats.ForReview = len(forReview)
```

That was wrong three ways at once:

| | the OLD rule | the CANONICAL rule |
|---|---|---|
| a never-accessed memory | due **immediately**, however new | due only once it is itself older than the window |
| a memory the operator **reviewed** | still counted — the `memory_reviews` ledger is ignored | not due — an operator's review is the strongest signal |
| the count | the length of a **100-candidate sample** | the **total** |

And a fourth: the hard-coded `14` was a second copy of the window the
canonical constant already holds.

**THE CORRECTION.** `RunSelfMaintenance` now calls
`CountMemoriesDueForReview(ReviewDueAfterDays)` — the same query the
operator-facing surfaces use, with the total rather than a capped
slice. There is now **one** definition of review eligibility in the
substrate. The legacy `SpacedReinforcementReview` function is kept for
the surfaces that ask for a *list* rather than a count; its only
active production caller of the old *rule* was this path.

### 3A.3 The evidence

The corrections are pinned by eleven tests, and each one was run
against the **pre-fix** code to prove it pins the defect rather than
merely describing the fix.

| test | on the pre-fix code (`4b420452`) |
|---|---|
| `TestReviewWeakenIsAtomic_AFailedWeakeningLeavesNoReview` | **FAILS** — *"the review was recorded even though the weakening failed: 1 review row(s)"* |
| `TestReviewAckAllIsAtomic_AFailureLeavesNothingApplied` | **FAILS** — *"the batch left 2 review row(s) behind after a failure"*; *"batch-a changed weight even though the batch failed: 7 -> 6"* |
| `TestSelfMaintenanceReviewCount_MatchesTheCanonicalRule` | **FAILS** — *"reported 2 memories due for review; the canonical rule says 1"* |
| `TestSelfMaintenanceReviewCount_AReviewedMemoryIsNotDue` | **FAILS** — *"counted a REVIEWED memory: 1; the canonical rule says 0"* |
| `TestSelfMaintenanceReviewCount_IsTheTotalNotASample` | **FAILS** — *"reported 100 memories due for review; the backlog is 150"* |

The remaining six pass on both — they are regression guards for
behaviour that was already correct (a `keep` changes nothing, a
successful weakening records both halves, SQLite contention leaves
nothing behind, a failed operation recovers, a successful batch records
every review, and the maintenance path agrees with the doctor).

---

## 4. What was NOT changed (and why)

| Item | State |
|---|---|
| `mpm ops review --promoted` | kept as a **different** view with its own query; the page says so. It is never the answer to the doctor's backlog. |
| Per-leaf option sets inside a family | the router's union guards the family; the **leaf** still rejects what it does not support only where it has its own guard (doctor, review, decide, theorize, gc, backup, synthesize). **Deferred** — see §7. |
| `mpm decide --created-at` | was a **dead flag** (accepted, ignored). It is now rejected. The regression test that used it seeds the legacy epoch row directly instead. |
| `mpm theorize -h` | unreachable — the router takes `-h` for help. Advertised as `--h` (a long alias), which the handler has always accepted. |
| The MCP `ReviewMemories` envelope | shape preserved (`success`, `items`, `count`, `days`, `limit`); the per-item records gain the honest fields. |

---

## 5. Breakages this work found in itself

The AST-derived registry coverage test earned its keep twice:

1. **`mpm gc --max-age=24` and `mpm gc --dry-run` stopped working** the
   moment the router guard was switched on: `gc` was ABSENT from the
   registry, and an absent command accepts no options. Twenty-four
   commands were missing for the same reason.
2. **`mpm ops review --stale` stopped working** when the family
   entries were hand-maintained: `stale` belonged to the review leaf's
   vocabulary, not to the family's. The families' entries are now
   **derived** as the union of every other entry, in `init()`, so the
   class of bug cannot recur.

---

## 6. The CLI audit matrix

`cli_audit.py` measures, per command, in an isolated workspace:
`--help` exit code and mutation; `-h` exit code and mutation; an
unknown option's exit code and whether it names the flag.

**Before (measured on `4f81690f`):**

| Property | Result |
|---|---|
| `--help` exits 0 | 58 / 58 commands |
| `--help` mutates nothing | 58 / 58 commands |
| an unknown option fails visibly | **44 / 58** — 14 commands exited 0 |
| an unknown option that MUTATED | **4** — `decide`, `theorize`, `gc`, `backup` (+ `synthesize` ran its operation) |
| `mpm ops <unknown-subcommand>` | exited **0** and printed the help page |

**After (measured on this branch):**

| Property | Result |
|---|---|
| `--help` exits 0 | 58 / 58 |
| `-h` exits 0 | 58 / 58 |
| `--help` / `-h` mutate nothing | 58 / 58 |
| an unknown option fails visibly | 58 / 58 |
| `mpm ops <unknown-subcommand>` | exits **1** and names the subcommand |

The 58-command matrix is now a **table-driven test**
(`cmd/mpm/cli_help_contract_test.go`), not a one-off script: help exits
0, the unknown option fails, the help request mutates nothing, and the
advertised options are the accepted options.

---

## 7. Deferred

1. **Per-leaf option precision inside a family.** `mpm ops stats
   --bogus` is rejected by the router's union (because `--bogus` is
   unknown to the whole CLI), but `mpm ops stats --json` would be
   accepted by the router and ignored by the leaf if `--json` belongs
   to a *different* leaf. Giving every leaf its own vocabulary is a
   larger change than this PR's boundary; the router's union is the
   safe direction and the leaves with a precise guard are the
   mutating ones.
2. **`mpm audit` is unreachable.** It is in the router's dispatch switch
   but not in the command table, so `mpm audit` reports "unknown
   command". Pre-existing; not this PR's scope.
3. **The `memory_reviews` ledger has no history.** Only the latest
   review per memory is kept. A review *history* would be a different
   product.

---

## 8. Test evidence

| Suite | Result |
|---|---|
| `go test ./cmd/mpm/...` | **ok** (403 s) |
| `go test -race ./cmd/...` | **ok** (410 s) |
| `go test -race ./internal/...` (the root module) | **ok** |
| `go test -race ./...` (the `mpm-core` module) | **ok** (345 s) |
| `make build` | ok — 5 binaries |
| `make test-scripts` | **ok** — 682 tests |
| `make docs-check` | **ok** — 13 checks, 0 errors |
| `mpm-lint --gate` | **PASS** (ctx, go, mutex, sql, fd, imports) |
| `render_managed_blocks.py --check` | parity |

One caveat worth recording, because it looks like a defect and is not:
`TestBuildProvenance_TheCanonicalScriptStampIsEmbeddedInTheBinary` fails
when the binary under test is OLDER than HEAD — the stamp is baked in at
build time. Run the race suite after a commit without rebuilding and it
fails with "the canonically stamped binary does not report this checkout's
HEAD commit". Rebuild and it passes.

New test files:

- `cmd/mpm/cli_help_contract_test.go` — the 58-command matrix.
- `cmd/mpm/cli_options_registry_test.go` — the AST-derived registry
  coverage, the phantom-name check and the precise-set check.
- `cmd/mpm/review_workflow_test.go` — the review eligibility, the
  honest rendering, the honoured options, the acknowledgment semantics,
  the negative controls (listing does not acknowledge; a `keep` review
  does not change the weight) and the doctor → review → doctor journey.

Existing tests updated, with the intent preserved and the contract
corrected:

| Test | What changed |
|---|---|
| `doctor_flag_truthfulness_test.go` | the wording moved from "unknown flag" to "Unknown option" (the rejection gained the option block and a typo suggestion); the usage-block extraction now covers the options block; the `--fix` pairing is expressed on the option's own line. |
| `doctor_bootstrap_directives_test.go` | the fixtures are **backdated**: a memory created now is not due, so a fixture that wants one due has to make it old. |
| `release_pass_20260914_dashboard_attention_test.go` | same backdating, for the same reason. |
| `list_migration_test.go` | the legacy epoch row is seeded directly, because `--created-at` was a dead flag and is now rejected. |
| `internal/core/call_helpers_test.go` | the default review window is now the substrate's constant (14), not 30. |
| `internal/core/sql_dump_validator.go` | `memory_reviews` added to `CanonicalMPMSchema` — the `restore-db` allow-list. |

---

## 9. The doctor → review → doctor journey

Measured in an isolated workspace with three seeded memories: two old
(`journey-a`, 90 days; `journey-b`, 120 days) and one created a moment
ago (`journey-new`).

**1. The doctor reports the backlog, and recommends the two steps:**

```
⚠  Review backlog         2 memories due for spaced review
        → Run 'mpm ops review --stale' to list them (window: 14d).
        → Then 'mpm ops review --ack <id>' (or '--ack-all') records the review and clears it.
```

The **new** memory is not in the count. That is the defect the brief
names, fixed.

**2. The operator lists them:**

```
Spaced review — 2 memories due (window: 14 days)
A memory is due when it is long-term or its weight is 5 or more, and it has not been
reviewed, accessed, or — failing both — created within the window.

1. [journey-]  weight 10  reinforcement count 0
   never accessed (created 120d ago) — 120 days past the window
   another durable fact
   → review: mpm ops review --ack journey-b

2. [journey-]  weight 10  reinforcement count 0
   never accessed (created 90d ago) — 90 days past the window
   a durable fact worth reviewing
   → review: mpm ops review --ack journey-a

2 memories due for review. Record each review with:
  mpm ops review --ack <id>            one memory
  mpm ops review --ack-all             every memory due now
```

"never accessed", the **stored** weight (10, not a delta), a labelled
reinforcement count, and the per-item review command.

**3. The operator records the reviews:**

```
✓ Reviewed 2 memories (keep)
```

**4. The doctor reports the backlog CLEARED:**

```
✓  Review backlog         no memories due for review (window: 14d)
```

That last step is the one that never happened before: the doctor said
"run this to clear the backlog" and the backlog did not move.

---

## 10. The PR

- **Branch:** `fix/cli-consistency-and-review-workflow`
- **Base:** `main`
- **PR:** linked from the branch's head commit.
- **Status at the time of writing:** open, **not merged**, not
  deployed. The live installation still runs the previously deployed
  revision.
