# Issue #21 — Phase B implementation and verification report

**Scope.** Implement the bounded, transactional contradiction-proposal design
established in Phases A, A.1 and A.2, per the HAL authorisation for issue #21.

**Status.** Implemented and verified in an isolated local worktree. The full
validation gate is green — `make build`, `make test`, `make test-race` and
the three precommit targets all exit 0. **Not committed** at the time of
writing — a commit requires further HAL approval. No push, no PR, no merge,
no issue closure.

**Baseline.** `main` at `9dba5e99654b10dbee1692244206ca06a6572395`.

**Worktree.** `.claude/worktrees/issue-21-bounded-proposals` (this worktree).
The primary checkout and every other worktree are untouched.

---

## 1. What was built

### 1.1 Bounded candidate discovery — `internal/core/similarity_candidates.go`

`FindSimilarityCandidates` is preserved verbatim, with its existing callers and
its existing semantics. A new sibling was added:

```go
func FindSimilarityCandidatesBounded(entries []ScanEntry, candidateCap, compareBudget int) (
    []SimilarityCandidate, CandidateDiscoveryStats, error)
```

It applies the same threshold (`ContradictionCandidateThreshold = 0.85`), the
same structural-prefix discount, the same classification and the same
deterministic ordering. What it adds is a boundary:

| Budget | Kind | Checked |
|---|---|---|
| `compareBudget` | time bound | **once, before any pair is evaluated** — against `C(n,2)` over the entries offered |
| `candidateCap` | allocation bound | after the loop; the loop keeps running so the statistics stay truthful |

The distinction is deliberate and is stated in the code, because a cap on
retained allocations says nothing about execution time. **The candidate cap
limits retained allocations. It does not independently guarantee short
execution time.** The comparison budget is what bounds the work, and it is
checked first.

On either refusal the returned slice is `nil`. There is no way for a caller to
receive a partial discovery, so no caller can persist candidates from an
incomplete pass.

Two typed errors make the refusals distinguishable by the caller:

- `ErrCompareBudgetExceeded{Entries, EstimatedPairs, CompareBudget}`
- `ErrCandidateCapReached{CandidatesFound, CandidateCap, PairsExamined}`

`CandidateDiscoveryStats` is internally consistent by construction:

```
PairsExamined   == PairsSkippedInvalid + PairsBelowThreshold + PairsAboveThreshold
PairsAboveThreshold == CandidatesFound + PairsDeclined
```

Pair arithmetic is `int64` (`comparePairCount`, `comparePairCountForSources`),
so a 2,000-per-source window — a 4,000-entry union, 7,998,000 pairs — cannot
overflow.

### 1.2 Atomic queue insertion — `internal/core/similarity_candidates_store.go`

New: `EnqueueSimilarityCandidatesTransactional(ctx, candidates, writeBudget)`.

The transaction:

1. Takes a **pinned** `*sql.Conn` (`db.Conn(ctx)`), not the pool.
2. Confirms the `shared` database is attached **on that connection**
   (`ensureSharedAttachedOnConn`). ATTACH is per-connection in SQLite, so
   checking it on some other handle would prove nothing.
3. Opens the transaction with **`BEGIN IMMEDIATE`**, taking SQLite's write lock
   at transaction start rather than at first write. A deferred BEGIN would let
   two transactions both take a read snapshot, both see the pair as absent, and
   both insert.
4. Takes the unresolved-pair snapshot **inside** the transaction.
5. Canonicalises, validates and deduplicates the batch.
6. **Refuses an over-budget batch before the first INSERT.**
7. Inserts the whole eligible batch, then commits.

`TransactionResult` distinguishes four dispositions, and the distinction is
load-bearing:

| State | Meaning |
|---|---|
| `refused` | declined before any write — the queue is provably unchanged |
| `rolled_back` | an attempt was made and undone — the ROLLBACK statement itself succeeded |
| `uncertain` | the outcome is unknown — a COMMIT or a ROLLBACK failed |
| `confirmed` | committed |

`Inserted` counts **confirmed committed** rows. A rolled-back transaction
reports zero however many INSERTs it issued. An uncertain outcome is reported
as uncertain — never as a confirmed success, and never as a confirmed
rollback.

A successful ROLLBACK after a failed COMMIT is treated as *proof* that the
commit did not take effect: SQLite would have refused the rollback with "no
transaction is active" had the commit succeeded.

**No schema change.** The table's `UNIQUE(memory_id_a, memory_id_b, detected_at)`
is not what enforces unresolved-pair uniqueness — that constraint includes a
one-second-resolution timestamp and therefore cannot. Uniqueness comes from the
in-transaction snapshot. The inserts use `INSERT OR IGNORE` so that the table's
own constraint suppressing a row (a resolved pair re-proposed within the same
second as its original detection) keeps the batch atomic; the suppression is
counted as `SkippedConstrained` so the report explains itself.

The scan side lives in the same file. `LoadSimilarityScanEntriesFederated(limit)`
applies the limit **per side** — local and shared each get the full window —
and the union is then deduplicated by memory ID, so a memory present in both
stores is scanned and compared exactly once. `LoadSimilarityScanEntries` takes
a schema prefix that is interpolated into SQL; the prefix is validated against
a strict allowlist before it reaches the query
(`TestLoadSimilarityScanEntries_RejectsUnsafeSchemaPrefix`), so a caller
cannot turn that parameter into a query-injection point.

### 1.3 Shared validation — `internal/core/contradiction_log.go`

`validateAndNormaliseContradictionEvidence` is now the **single** validation
path for every writer of `shared.contradiction_log`. Both the original per-row
writer (`EnqueueContradictionReport`) and the transactional batch writer call
it, so the two cannot drift apart and a new caller cannot bypass it by
accident. It enforces:

1. both memory IDs non-empty;
2. not a self-pair;
3. canonical (lexicographic) pair ordering;
4. a producer-supplied `adjudicated:` segment is **stripped**.

Property 4 is the one that matters most for safety: an `adjudicated:` segment
is a record of an operator having read both memories and concluded the claims
conflict. Creating a queue row is not that action, so a producer-supplied
adjudication is unauthorised no matter what the producer claims.
`AdjudicateContradiction` remains the only way a row becomes eligible to
penalise a memory.

`TestValidateAndNormaliseContradictionEvidence_IsSharedByBothWriters` pins
the sharing: it runs the same adversarial inputs through both writers and
requires identical outcomes, so a future change to one writer that is not
mirrored in the other fails the build rather than silently diverging.

Extracting the check changed one thing about the *original* writer, and it is
deliberate. The two rejections used to be bare `fmt.Errorf` values; they are
now the typed `ErrMissingMemoryID` and `ErrSelfContradiction`, so the
transactional writer can tell the two failure kinds apart with `errors.As`
instead of matching on message text. **The error message strings are
byte-identical to the originals** ("EnqueueContradiction: both memory IDs
required", "EnqueueContradiction: a memory cannot contradict itself"), so no
caller that matches on the message — and no existing test — is affected. The
rest of the check is unchanged: same order of tests, same canonicalisation,
same stripping.

### 1.4 CLI and JSON contract — `cmd/mpm/ops_resolve_contradictions.go`

**One JSON document per `--json` invocation.** The command previously emitted
the proposal statistics as a *separate* JSON line, so a consumer reading stdout
had to handle two documents and had no way to know whether a proposal had even
run. There is now exactly one envelope, with one stable key set, on every
path: empty queue, populated queue, and failure.

The old key set was not merely split — it was *inconsistent between branches*.
The empty-queue branch printed
`{"queue_size":0,...,"candidates_proposed":%d,"decisions":[]}`, while the
populated branch marshalled a map that had **no** `candidates_proposed` key at
all. So `candidates_proposed` existed precisely when there was nothing to
report and vanished precisely when there was. A consumer that read it
unconditionally would have crashed on every run that found something.

```
queue_size               rows reviewed in this batch
queue_total_unresolved   unresolved rows in the queue in total, or null
                         when the queue could not be counted
applied                  resolutions applied (0 unless --apply)
close_calls              near-tie arbitrations proposed
needs_adjudication       rows awaiting an operator's judgement
needs_adjudication_rows  those rows, with their queue ids
dry_run                  true unless --apply
decisions                per-row resolution decisions
propose                  nested proposal statistics (always present)
error                    why this invocation could not do what was asked,
                         or null when it did
```

`queue_total_unresolved` is new and is distinct from `queue_size`:
`queue_size` is the batch this invocation reviewed, `queue_total_unresolved`
is how many unresolved rows the queue holds in total.

It is a **pointer** in the Go struct, so a count that failed serialises as
`null` rather than `0`. Zero is a claim — "the queue is empty" — and it is
not the claim an unreachable queue supports. `null` says "not known", and
the `error` field says why.

The nested `propose` object reports `requested`, `scan_limit_per_source`,
`entries_scanned`, `pairs_examined`, `candidates_found_in_scan`,
`scan_complete`, `candidates_materialised`, `candidates_proposed`,
`candidates_skipped_pending`, `candidates_skipped_duplicate`,
`candidates_skipped_constrained`, `compare_budget`, `candidate_cap`,
`write_budget`, `completion`, `commit_state` and `error`.

`completion` names the outcome precisely: `not_run`, `complete`,
`compare_budget`, `candidate_cap`, `write_budget`, `invalid_candidate`,
`db_unavailable`, `db_busy`, `db_insert`, `db_error`, `rollback`,
`uncertain_commit`, `scan_failed`.

**Flag contracts.**

| Combination | Behaviour |
|---|---|
| `--propose --apply` | refused before any database access |
| `--propose --adjudicate` | refused before any database access |
| `--propose-limit` / `--compare-budget` / `--candidate-cap` / `--write-budget` without `--propose` | refused before any database access |
| `--propose-limit=all` | rejected explicitly |
| `--propose --dry-run` | **still inserts proposal rows** — `--dry-run` governs the resolution phase |

The four proposal-scoped budgets default to the approved values and are
overridable within their ranges:

| Flag | Default | Range |
|---|---|---|
| `--propose-limit` | 100 per source | 1..2000 |
| `--compare-budget` | 100,000 | 1,000..10,000,000 |
| `--candidate-cap` | 10,000 | 1..10,000 |
| `--write-budget` | 250 | 1..250 |

Raising `--propose-limit` never bypasses the other three budgets: a wider
window implies more pairs, and the comparison budget is enforced **inside the
discovery pass**, against the pairs the pass actually evaluates. The CLI does
not check the budget against the `2*limit` upper bound the union could
imply — that bound over-counts whenever local and shared overlap, and a
check made there would refuse a scan the pass could have completed.

On a proposal failure the command exits **non-zero** and stops **before** the
queue is reviewed or resolved. A half-proposal followed by a half-review would
leave the operator unable to tell which part of the output describes durable
state.

**Nothing is described as a corpus total.** `candidates_found_in_scan` is the
count of what the bounded pass found *inside the window it actually compared*.
`scan_complete` says whether the pass finished. The human output says "Scanned
N memories" and never claims a corpus-wide total.

### 1.5 Documentation

- `docs/RECALL-MUTATION.md` — new **"Bounded proposals (issue #21)"** section:
  the five budgets, the override rules, the JSON output, the transactional
  semantics, oldest-first review, the two queue counts, and the re-queue
  semantics. The pre-existing idempotency section was updated so it describes
  the transactional writer rather than contradicting it.
- `cmd/mpm/ops_resolve_contradictions.go` — `printResolveHelp` rewritten with
  `PROPOSAL BUDGETS`, `TRANSACTIONAL SEMANTICS`, `JSON OUTPUT`, `REVIEW ORDER`
  and `REJECTED COMBINATIONS` sections.
- `docs/SPEC.md` — a dated supersession note in the Arc 1 section, the command
  signature, and five new rows in the CLI limits table.
- `docs/ISSUE-21-PHASE-B-REPORT.md` — this document.

### 1.6 Independent review: findings and the fixes they caused

The patch was reviewed twice before this commit — once against the discovery
and transaction code, once against the CLI reporting and failure paths.
Every confirmed defect below was fixed, and every fix carries a regression
test that fails without it.

**Candidate budgets**

| Finding | Defect | Fix |
|---|---|---|
| A1 | a non-positive comparison budget was treated as *unlimited* (`compareBudget > 0 && …`), so a budget of `0` ran an unbounded scan | the budget check is unconditional; a non-positive budget refuses the pass |
| A2 | a non-positive candidate cap was treated as *unlimited* | the cap check is unconditional |
| A3 | `comparePairCountForSources` could overflow `int32` on a wide union | clamped to `math.MaxInt32` |
| A4 | the doc comment on `comparePairCount` implied the CLI used it to enforce the budget | the comment now says the budget is enforced inside the pass, against the entries the pass was given |
| A5 | `CandidateDiscoveryStats` described the materialised count as if a truncated slice were returned | the doc now says the pass *refuses* rather than truncating, because a partial slice beside a count reads as a complete answer |

**Transaction behaviour and validation**

| Finding | Defect | Fix |
|---|---|---|
| A6 | after a rollback, `res.Inserted` and `res.SkippedConstrained` still counted statements that had been undone | the deferred cleanup zeroes both; `TransactionResult` counts only what HAPPENED |
| A7 | a marshal failure and an invalid candidate were reported as database failures | both are typed (`ErrInvalidCandidate`) and the batch is reported as REFUSED, not failed |
| A8 | a non-positive write budget was reported as "0 pairs exceed the budget of 0" — a sentence that describes nothing | `ErrInvalidWriteBudget` reports the budget supplied and the number of candidates offered against it |

**CLI reporting and failure paths**

| Finding | Defect | Fix |
|---|---|---|
| B1 | the newly mandatory queue count could abort the invocation **before** the envelope was written, so `--json` emitted **zero** documents — breaking the command's own "exactly one JSON object per invocation" contract | the envelope is emitted on every path: proposal failure, count failure, load failure, empty queue, populated queue |
| B2 | a count failure was swallowed whenever a proposal had also failed, and the envelope then reported `queue_total_unresolved: 0` — a claim that the operator's entire worklist is empty | the total is a `*int`; a failed count serialises as `null`, and the new top-level `error` field carries the reason |
| B3 | the human path said "Nothing was written." for *every* non-complete completion, including the ones whose outcome is unknown | the sentence is driven by the commit state: `uncertain` says the outcome could not be established and the queue must be verified; `rolled_back` says nothing was written |
| B4 | the insert-phase error message always said "and was rolled back" — at a point where the rollback had not yet run, and wrongly when the ROLLBACK itself failed | the message points at the transaction's commit state instead of asserting an outcome it cannot yet know |
| B5 | every untyped failure was classified `db_insert`, so a queue that could not be *reached* was reported as a failed INSERT | `ErrSharedUnavailable` and `ErrDatabaseUnavailable` are typed and classified `db_unavailable`; the catch-all is now `db_error` |
| B6 | the comment on `EnqueueContradiction` named `EnqueueSimilarityCandidates` as the production insert path | corrected to `EnqueueSimilarityCandidatesTransactional` — verified: `git grep` over non-test files shows it is the only production insert |
| B7 | the CLI comment described a comparison-budget check against the `2*limit` upper bound that the implementation does not perform | the comment now describes what the code does |
| B8 | the report contained a machine-specific absolute path | replaced with a placeholder |

**The root cause behind B1 and B2 — and the defect it exposed.**

SQLite attaches a database **per connection**. MPM attaches the `shared`
schema on ONE pooled connection at startup (`attachShared`, `db.go`); the
pool is deliberately *not* limited to one connection, because
`SetMaxOpenConns(1)` deadlocks against a concurrent transaction (the note
in `db.go`); and there is no connect hook that attaches `shared` to each new
connection. Any statement against `shared.*` handed to the pool can therefore
land on a connection with no attachment and fail with *"no such table:
shared.contradiction_log"* — even though the table exists and is populated.

The transactional writer already handled this (`ensureSharedAttachedOnConn`).
The queue **count** did not: it ran as a pooled query, so the number the CLI
was required to report could fail for a reason that has nothing to do with the
queue's contents. The count now pins a connection and verifies the attachment,
exactly as the writer does. The same fix was applied to
`LoadContradictionQueue` — the one read the review path depends on — which had
the identical fragility; a spurious failure there aborts the operator's review
with an error claiming the table does not exist.

The test that pins this is `TestCountContradictionQueue_SucceedsOnAFreshPooledConnection`:
it checks out every idle connection the pool holds, forcing the count onto a
brand-new connection with no attachment. With the pooled implementation
temporarily restored, the test fails with *"no such table:
shared.contradiction_log"* — verified, not assumed.

**A note on the scale of these fixes.** Every finding above is inside the
code this patch introduces or the contract this patch introduces. None of
them required a change to a pre-existing behaviour that the directive named
as fixed — except the `LoadContradictionQueue` pinning, which was pre-existing
but sits directly under the CLI contract this patch is required to honour,
and whose failure mode (a spurious "no such table" aborting the review) is
one the count's own fix would otherwise have made more visible.

---

## 2. Verification

All verification used **synthetic fixtures and disposable databases**
(`t.TempDir()`, in-memory or temp-file SQLite). The live MPM database was
never opened, the 41 challenged memories were never touched, and the live
`mpm ops resolve-contradictions --propose` command was **never executed**.

**91 new test functions** were added across four files:

| File | Tests | Covers |
|---|---:|---|
| `internal/core/similarity_candidates_bounded_test.go` | 17 | bounded discovery, budgets, statistics, arithmetic |
| `internal/core/similarity_candidates_transactional_test.go` | 32 | transactional writer, faults, concurrency, shared validation, the read path |
| `cmd/mpm/ops_propose_candidates_test.go` | 14 | the `runBoundedProposal` orchestration path |
| `cmd/mpm/ops_resolve_propose_cli_test.go` | 28 | CLI flags, JSON envelope, refusal ordering, failure reporting |

### 2.1 Test coverage against the mandated areas

| # | Mandated area | Where it is pinned |
|---|---|---|
| 1 | Default and overridden scan limits; local/shared overlap and deduplication | `similarity_candidates_bounded_test.go`, `ops_propose_candidates_test.go` (`_LimitIsAppliedPerSide`, `_DeduplicatesIDsAcrossSources`), `ops_resolve_propose_cli_test.go` (`_ScanLimitDefaultsAndOverride`) |
| 2 | Comparison-budget and candidate-cap boundaries | `similarity_candidates_bounded_test.go` (`_CompareBudgetRefusesBeforeAnyPair`, `_CompareBudgetBoundary`, `_CandidateCapRefusesAndReturnsNothing`, `_CandidateCapBoundary`), `ops_propose_candidates_test.go` (`_CompareBudgetRefusalIsReported`, `_CandidateCapRefusalIsReported`) |
| 3 | Dense 75-row duplicate cluster | `similarity_candidates_bounded_test.go` (`_Dense75RowCluster`, `_DenseClusterHitsCompareBudgetFirst`), `ops_propose_candidates_test.go` (`_DuplicateHeavyClusterIsStableAfterFirstRun`) |
| 4 | Fresh queue and partially populated queue | `similarity_candidates_transactional_test.go` (`_FreshQueue`, `_PartiallyPopulatedQueue`) |
| 5 | Repeated scans and zero-insertion success | `similarity_candidates_transactional_test.go` (`_EmptyBatchSucceeds`, `_RepeatedScanWritesNothing`), `ops_propose_candidates_test.go` (`_RepeatWritesNothing`), `ops_resolve_propose_cli_test.go` (`_RepeatProposalReportsZero`) |
| 6 | Already-resolved pair re-queueing | `similarity_candidates_transactional_test.go` (`_ResolvedPairMayBeQueuedAgain`, `_SameSecondRequeueIsCountedAsConstrained`) |
| 7 | Write-budget refusal with zero new rows | `similarity_candidates_transactional_test.go` (`_WriteBudgetRefusesBeforeAnyInsert`, `_WriteBudgetCountsNewPairsOnly`, `_ZeroBudgetIsRefused`), `ops_propose_candidates_test.go` (`_WriteBudgetRefusesAndWritesNothing`), `ops_resolve_propose_cli_test.go` (`_ProposalFailureExitsNonZeroAndReportsZero`) |
| 8 | Mid-insertion fault and confirmed rollback | `similarity_candidates_transactional_test.go` (`_MidInsertionFaultRollsBackEverything`) — a real SQLite `RAISE(ABORT)` trigger |
| 9 | Commit failure and uncertain-outcome reporting | `similarity_candidates_transactional_test.go` (`_CommitFailureIsUncertain`, `_CommitAndRollbackFailureIsUncertain`, `_InsertAndRollbackFailureIsUncertain`) |
| 10 | Concurrent writers, lock contention, different-timestamp deduplication | `similarity_candidates_transactional_test.go` (`_ConcurrentWritersProduceOneRow`, `_DifferentTimestampDuplicateIsStillOneRow`, `_RepeatAcrossTimestampBoundaryIsOneRow`, `_LockContentionIsReportedAsBusy`) |
| 11 | Malformed, reversed and forged-adjudication candidates | `similarity_candidates_transactional_test.go` (`_SelfPairRefusesTheBatch`, `_EmptyMemoryIDRefusesTheBatch`, `_ReversedPairIsCanonicalised`, `_ForgedAdjudicationIsStripped`, `_RowCarriesProvenanceAndEvidence`) |
| 12 | JSON envelope shape for empty, populated and failure cases | `ops_resolve_propose_cli_test.go` (`_JSONEnvelopeHasOneStableKeySet`, `_ProposalFailureExitsNonZeroAndReportsZero`, `_ProposalFailureStopsBeforeReview`, `_JSONEmitsOneEnvelopeWhenTheQueueCannotBeCounted`, `_JSONDoesNotReportAFailedCountAsZero`) |
| 13 | Disallowed flag combinations and parser boundaries | `ops_resolve_propose_cli_test.go` (`_RefusedCombinationsHappenBeforeAnyDatabaseAccess`, `_ParserBoundaries`, `_LegalBudgetBoundariesAreAccepted`) |
| 14 | Existing contradiction-verification, adjudication and retrieval non-mutation regressions | the pre-existing suites, re-run unchanged — see §2.3 |

Beyond the fourteen mandated areas, the review-driven fixes in §1.6 are pinned
by these tests:

| Fix | Test |
|---|---|
| A1 — a non-positive comparison budget refuses the pass | `similarity_candidates_bounded_test.go` (`_NonPositiveCompareBudgetRefuses`) |
| A2 — a non-positive candidate cap refuses the pass | `similarity_candidates_bounded_test.go` (`_NonPositiveCandidateCapRefuses`) |
| A6 — counters do not survive a rollback | `similarity_candidates_transactional_test.go` (`_ConstrainedCountDoesNotSurviveARollback`) |
| A8 — an unusable write budget is a typed refusal | `similarity_candidates_transactional_test.go` (`_ZeroBudgetIsRefused`, `_NegativeBudgetIsRefused`) |
| B1/B2 — one envelope, and an unknown total is null | `ops_resolve_propose_cli_test.go` (`_JSONEmitsOneEnvelopeWhenTheQueueCannotBeCounted`, `_JSONDoesNotReportAFailedCountAsZero`) |
| B3 — the human path does not claim a known outcome it cannot know | `ops_resolve_propose_cli_test.go` (`_UncertainOutcomeDoesNotClaimNothingWasWritten`, `_ConfirmedRollbackSaysNothingWasWritten`) |
| B4 — the error message does not assert a rollback that has not happened | `similarity_candidates_transactional_test.go` (`_InsertFailureDoesNotClaimARollback`, `_CommitFailureDoesNotAssertAnOutcome`) |
| B5 — an unreachable queue is not an insert failure | `ops_resolve_propose_cli_test.go` (`_UnavailableQueueIsNotAnInsertFailure`, `_UnavailableDatabaseIsNotAnInsertFailure`, `_UntypedFailureIsNotAnInsertFailure`) |
| the per-connection ATTACH root cause | `similarity_candidates_transactional_test.go` (`_CountContradictionQueue_SucceedsOnAFreshPooledConnection`, `_LoadContradictionQueue_SucceedsOnAFreshPooledConnection`) |

### 2.2 Measured performance

**These are measurements, not estimates.** The pair counts are mathematical and
exact; the elapsed times are one machine at one moment, are not guarantees,
and are not portable. The measured figures below are reproduced verbatim from
the test log in §2.5.

| Workload | Shape | Pairs (mathematical) | Measured |
|---|---|---|---|
| Normal | default window (100/source ⇒ 200 entries), mostly-unrelated corpus | C(200,2) = 19,900 | 19,900 pairs compared in **7.19 ms**, 0 candidates |
| Dense | 75 near-identical memories, every pair above threshold | C(75,2) = 2,775 | 2,775 pairs compared in **1.22 ms**, 2,775 candidates materialised |
| Dense, refused | same corpus, comparison budget 1,000 | 2,775 implied | refused in **501 ns**, **0 pairs evaluated** |

Two observations follow from these numbers, and they are the point of the
measurement rather than decoration:

**The dense-refusal figure is the load-bearing one.** 501 ns for 2,775
implied pairs means the budget was checked *before* the work, not after it.
Had the check happened per-pair, the refusal would have cost at least the
1.22 ms the dense pass costs — three orders of magnitude more.

**The dense pass is faster than the normal pass, and that is expected.** The
dense corpus is 75 entries (2,775 pairs) and the normal corpus is 200 entries
(19,900 pairs) — roughly 7× more pairs. The dense pass is nonetheless ~6×
*faster*, because every dense pair is a hit that gets materialised as a
candidate, while every normal pair is a miss that is scored and discarded.
The two numbers are not comparable as "fast vs slow"; they are two different
shapes of the same O(n²) work.

**Headroom.** The default comparison budget (100,000) exceeds the default
window's implied pair count (19,900) by ~5×, so the default configuration
never refuses for size reasons. This is pinned by
`TestFindSimilarityCandidatesBounded_DefaultBudgetHasHeadroomForTheDefaultWindow`.
The refusal path therefore triggers only when an operator explicitly raises
`--propose-limit` — which is the correct behaviour: an explicit override
should be the thing that hits the wall.

**The 10,000,000-pair ceiling has NOT been exercised at that scale, and no
runtime figure is claimed for it.** The measurements above are two orders of
magnitude below it (2,775 and 19,900 pairs). Ten million pairs is roughly
7.7e10 elementary operations at 768 dimensions — that is an *arithmetic*
estimate, not a measurement, and it is not quoted here as one. Nothing in
this report asserts that a 10,000,000-pair budget completes "in seconds";
the budget exists to make the scan **bounded**, not to promise a duration.
An operator who needs to know how long a given budget takes should measure
it on their own corpus. The same statement is recorded next to the constant
in `internal/core/similarity_candidates.go`.

### 2.3 Validation gate

All commands were run inside the worktree
`.claude/worktrees/issue-21-bounded-proposals` — except the baseline comparison
in §2.4, which ran in a temporary scratch worktree that was removed
afterwards.

| Check | Result |
|---|---|
| `make build` | **PASS** — exit 0; all five binaries built to `.build/bin/` |
| `make test` | **PASS** — exit 0, zero `--- FAIL` lines across every package |
| `make test-race` | **PASS** — exit 0; 4 569 PASS, 26 SKIP, **0 FAIL**, **0 `DATA RACE`** |
| `make test-core-precommit` | **PASS** — exit 0 |
| `make test-tools-precommit` | **PASS** — exit 0 |
| `make test-testenv-precommit` | **PASS** — exit 0 |
| `gofmt -l` on all modified/created Go files | **clean** — no output |
| `go vet -tags fts5 ./...` from `internal/core/` | **PASS** — exit 0, no output |
| `go vet -tags fts5 ./...` from the repository root | **PASS** — exit 0, no output |

Package timings from the `make test` run:

```
ok  github.com/flowbyte-com/mpm/cmd/mpm                    341.271s
ok  github.com/flowbyte-com/mpm-core                        (cached — identical source)
ok  github.com/flowbyte-com/mpm-core/tools                   29.808s
ok  github.com/flowbyte-com/mpm/internal/testenv               1.604s
ok  github.com/flowbyte-com/mpm/cmd/mpm-mcp                   3.400s
[test-agent-adapters] Python: 281 tests (floor 150)
[test-agent-adapters] JS: 165 passing tests (floor 100)
[test-agent-adapters] TS: 17 passing tests (floor 15)
[test-agent-adapters] all adapter suites green
[test-agent-installation] collected 427 tests (floor 300)
```

Package timings from the `make test-race` run:

```
ok  github.com/flowbyte-com/mpm/cmd/mpm                    350.815s
ok  github.com/flowbyte-com/mpm-core                        (cached — identical source)
ok  github.com/flowbyte-com/mpm-core/tools                   39.230s
ok  github.com/flowbyte-com/mpm/internal/testenv               6.819s
ok  github.com/flowbyte-com/mpm/cmd/mpm-mcp                   4.702s
[test-agent-adapters] all adapter suites green
[test-agent-installation] collected 427 tests (floor 300)
```

A package reported as `(cached)` was run by an earlier invocation of the
same command against the same source and passed there. The cache key
includes the compiler flags, the build tags and the package's source, so a
cached `ok` is a pass on this source — not an untested package.

**One environmental note, recorded honestly.** The first `make test` run
failed with `exit 2` — but not in any Go package. Every Go package passed;
the failure was in `test-agent-adapters`:

```
FATAL: mpm-opencode has no local TypeScript compiler at node_modules/.bin/tsc.
  Run 'npm ci' inside agent_installation/mpm-opencode to populate devDependencies.
```

This was a missing local `npm install` in the freshly created worktree — the
primary checkout and every other worktree had `node_modules` present and this
one did not. The repository's own `.gitignore` documents the intended
bootstrap (`npm ci` on a fresh checkout, from the committed lockfile). The
lockfile here is byte-identical to the primary checkout's. Running `npm ci`
inside `agent_installation/mpm-opencode` of the worktree — the repository's
documented bootstrap, touching nothing tracked and nothing outside the
worktree — resolved it, and the re-run of the full gate returned exit 0. No
Go code, no test, and no assertion was changed to get there.

### 2.4 The one race-gate exception, and the proof it is not ours

**The final gate is fully green, `make test-race` included.** The race run
that this commit is validated against reported:

```
--- SKIP: TestDrillE2E_ClaudeCode (35.06s)
4569 PASS    26 SKIP    0 FAIL    0 DATA RACE
```

`make test-race` exited 0. No test failed, and the race detector reported
nothing.

**The history of that one test, because "it passed" is not the whole
truth.** An earlier run of the same gate on this source saw it FAIL:

```
--- FAIL: TestDrillE2E_ClaudeCode (90.19s)
    drill_e2e_claude_code_test.go:165: Finish: claude timed out: context deadline exceeded
[claude stderr] [claude-code:unrecognized_model] {"model":"MiniMax-M3.1-Flash","query_source":"sdk"}
```

The same test, on the same source, in the same environment, alternates
between **skipping** and **failing** depending on how long the `claude`
subprocess takes to exit. That is a timing-dependent flake in a test that
shells out to a real CLI, and it is documented here rather than papered
over — a reader who sees it fail tomorrow should find the explanation
already written down.

**Every test this change adds passes under the race detector:**

| New tests under `-race` | Count | Failures | `DATA RACE` reports |
|---|---:|---:|---:|
| `internal/core` (bounded discovery, transactional writer, pair-count arithmetic, shared validation) | 49 | 0 | 0 |
| `cmd/mpm` (CLI flags, JSON envelope, `runBoundedProposal`, failure classification) | 42 | 0 | 0 |
| **Total new tests** | **91** | **0** | **0** |
| **Total `DATA RACE` reports across the whole race run** | | | **0** |

The 91 figure is the count of test functions in the four new or rewritten
test files — 17 + 32 in `internal/core`, 14 + 28 in `cmd/mpm` — each matched
by name against the race log.

The concurrency tests are the ones that matter for a race gate —
`_ConcurrentWritersProduceOneRow`, `_LockContentionIsReportedAsBusy`,
`_DifferentTimestampDuplicateIsStillOneRow` — and they all pass under the
race detector, with no race reports.

**Why the drill test is flaky, and why it is not this change.** The test
spawns the *real* `claude` CLI as a subprocess. In this environment the
ambient model is `MiniMax-M3.1-Flash`, which the Claude Code CLI does not
recognise; it prints `claude-code:unrecognized_model` on stderr. The test
has a gate for exactly this case:

```go
if os.Getenv("MPM_E2E_CLAUDE") != "1" {
    if strings.Contains(h.DebugOutput(), "claude-code:unrecognized_model") {
        t.Skipf("Claude Code rejected the harness model ...")
    }
}
```

But that gate sits **after** `h.Finish(ctx, dm.SQLDB())`, and `Finish` has a
90-second deadline (`TimeoutSecs: 90`). When the CLI exits inside the
deadline, the gate runs and the test **skips** — which is what happened in
the final race run (35.06 s) and in the non-race `make test` run. When the
CLI does *not* exit inside the deadline, `Finish` times out and
`t.Fatalf` fires **before** the skip gate can be reached. Under the race
detector everything is slower and the deadline is hit first — which is how
the earlier run failed at 90.19 s.

This is confirmed empirically rather than argued. A scratch worktree was
created at the **unmodified baseline** `9dba5e9` and the identical command
was run:

```
cd <scratch-worktree>/internal/core
go test -race -tags fts5 -v ./
```

| | Issue #21 worktree | Unmodified baseline `9dba5e9` |
|---|---|---|
| drill test | `--- FAIL: TestDrillE2E_ClaudeCode (90.19s)` | `--- FAIL: TestDrillE2E_ClaudeCode (90.18s)` |
| `mpm-core` package | `FAIL … 279.363s` | `FAIL … 276.312s` |
| `--- FAIL` count in the package | 1 | 1 |

Identical failure, identical cause, identical duration. The baseline package
finishes 3 seconds sooner, so the added core tests contribute about 1 %
to the package's runtime — they are not what tipped the drill test over its
deadline. (Go runs tests in a package in source-file order, and
`drill_e2e_claude_code_test.go` sorts *before* both new
`similarity_candidates_*_test.go` files, so the new tests had not even run
yet when the drill test failed.) That comparison was taken when the patch
carried 39 core tests; the patch now carries 49, which adds a little more
runtime to a package that takes minutes, and does not change the
conclusion — the drill test runs first and fails before any of them execute.

The scratch worktree was removed after the comparison; the worktree list is
back to its original five entries.

**Bottom line for the gate:** the committed tree passes the full gate, with
`make test-race` exiting 0 and reporting **0 failures and 0 `DATA RACE`
reports**. The one test that has ever failed here,
`TestDrillE2E_ClaudeCode`, fails identically at the unmodified baseline
for an ambient-environment reason, and in the final run it skipped instead.
It is not a code defect in this work, and it is not something this work
introduced — it would fail on `main` today.

### 2.5 Measured timings (verbatim from the test log)

```
=== RUN   TestFindSimilarityCandidatesBounded_Dense75RowCluster
    similarity_candidates_bounded_test.go:287: MEASURED dense 75-row cluster: 2775 pairs compared in 1.21513ms (2775 candidates materialised)
--- PASS: TestFindSimilarityCandidatesBounded_Dense75RowCluster (0.00s)
=== RUN   TestFindSimilarityCandidatesBounded_DenseClusterHitsCompareBudgetFirst
    similarity_candidates_bounded_test.go:316: MEASURED dense 75-row cluster refused by the comparison budget in 501ns (0 pairs evaluated)
--- PASS: TestFindSimilarityCandidatesBounded_DenseClusterHitsCompareBudgetFirst (0.00s)
=== RUN   TestFindSimilarityCandidatesBounded_NormalWorkloadAtDefaultWindow
    similarity_candidates_bounded_test.go:367: MATHEMATICAL: C(200,2) = 19900 pairs implied by the default window
    similarity_candidates_bounded_test.go:368: MEASURED normal workload: 19900 pairs compared in 7.188069ms (0 candidates, 19900 below threshold)
--- PASS: TestFindSimilarityCandidatesBounded_NormalWorkloadAtDefaultWindow (0.01s)
--- PASS: TestFindSimilarityCandidatesBounded_DefaultBudgetHasHeadroomForTheDefaultWindow (0.00s)
ok  	github.com/flowbyte-com/mpm-core	0.015s
```

The `MATHEMATICAL:` line is the exact pair count the default window implies,
derived from the binomial coefficient. The `MEASURED:` lines are wall-clock
observations from a single run on the developer's machine. The two are kept
apart deliberately: the first is a fact about the corpus shape, the second is
an observation about one machine at one moment.

---

## 3. Decisions worth recording

**The comparison budget is checked up front, against `C(n,2)`.** This means a
corpus in which every pair qualifies is refused exactly like a corpus in which
none do. It also means the budget is effective regardless of candidate
density, which is the property that makes it a budget rather than a hint.

**The candidate cap bounds allocations, not time.** The pair loop deliberately
runs past the cap so the statistics describe the whole window and
`CandidatesFound` is a real count rather than an artefact of where the loop
stopped. The cost is that the cap does not make the scan faster. The
comparison budget does that, and it is checked first.

**A refused write reports `refused`, not `rolled_back`.** A refusal rolls back
an empty transaction. Reporting that as "rolled back" would tell the operator
an attempt was made and undone, which is not what happened. The distinction is
carried by an explicit flag rather than by inference.

**The one-second UNIQUE artefact is documented, not worked around.** A pair
re-proposed within the same wall-clock second as its original detection is
suppressed by the table's own constraint rather than by the pending check.
Changing the timestamp format or the schema to avoid it would have been a
larger change than the defect warrants, and would have broken parity with the
original writer. The suppression is counted (`SkippedConstrained`) and
reported, so "eligible 1, proposed 0" has an explanation instead of looking
like a lost write.

**The CLI failure path prints the proposal summary too.** The human-readable
path now says what was scanned and that the proposal did not complete, so it
describes the same thing the JSON path does. Previously an operator reading
only stderr would not know how far the scan got.

**The `needs_adjudication` semantics were preserved, not "fixed".** The
verification gate refuses to *apply* an unadjudicated row; in dry-run it
returns the decision with `Adjudicated=false` so the row stays visible. That
is the pre-existing behaviour and the directive asked for the existing review
fields to be preserved, so it was left alone. The new test
`_ProposalRowIsAReviewItemAndStaysQueued` pins it, and
`_ApplyRefusesAnUnadjudicatedRow` pins the apply-side gate.

---

## 4. Not done, and why

| Item | Why |
|---|---|
| Commit | Requires further HAL approval. The working tree holds the change; nothing is committed. |
| Push, PR, merge, issue closure | Not authorised. |
| Making the flaky drill test deterministic | `TestDrillE2E_ClaudeCode` alternates between skipping and failing on the same source, depending on how long the real `claude` subprocess takes to exit; it fails identically at the unmodified baseline under the identical command (§2.4). The final gate run in which this change was validated had it **skip**, so `make test-race` exited 0. Making it deterministic would mean changing an unrelated E2E drill test's timeout or its model gate — a change the directive did not authorise, and one that would mask rather than address the cause. It is left alone, and documented instead. |
| Live `mpm ops resolve-contradictions --propose` | Not authorised, and never executed. |
| Live database reads or writes | Not authorised. All verification used disposable databases. |
| The 41 challenged memories | Not touched. |
| Schema migration | Not needed and not authorised. No schema change was made. |
| Embedding model / production retrieval | Not changed. |
| Installed binary / deployment / service restart | Not authorised. `make build` writes to `.build/bin/` inside this worktree only. |
| AgentShell | Not touched. |
| `cmd/dbg_main.go.tmp` and the forensic snapshot | Preserved untouched (SHA-256 `d84e49cdf28998602e7eafdcf7f7cd36098ab200b885772ee3f0c1a9c2ae4575`). |
| `docs/SPEC.md` product-truth check | Updated: the supersession note, the command signature and the CLI limits table. |

### 4.1 Residual risks carried by this patch

These are known and accepted, not discovered-and-forgotten. Each is stated
here so the next reader knows what the change does *not* guarantee.

| Risk | Why it is acceptable (for now) |
|---|---|
| `isSQLiteBusyError` is consulted only for `BEGIN IMMEDIATE`. A busy error during the INSERT loop or the COMMIT is reported as `db_insert` / `db_commit_rolled_back`, not `db_busy`, so a script that retries on `db_busy` would not retry those. | Largely theoretical: `BEGIN IMMEDIATE` takes the write lock, so a later write *inside the same transaction* cannot be blocked by another writer. The one case that could reach the COMMIT is a reader holding a shared lock in rollback-journal mode; MPM runs WAL. It was left alone rather than adding a branch that cannot be exercised deterministically — an untestable branch is worse than a documented limit. |
| `queue_total_unresolved` is read **before** the review, so under `--apply` it is the pre-review total; the remainder afterwards is `queue_total_unresolved - applied`. | Deliberate. The count is a pre-flight reading: a queue that cannot be counted is discovered before anything is mutated. Moving it after the review would mean learning that the queue was unreachable only after the review had already changed it. |
| The `shared` schema is attached per connection, and the pool can open a connection with no attachment. The count and the queue load now attach on the connection they use, but other `shared.*` reads elsewhere in the codebase still run as pooled queries and remain exposed to the same fragility. | Out of this patch's scope. The fix here is where the patch's own contract required it. A general connect-hook would be the structural fix, and it would change connection management for the whole codebase. |
| The table's `UNIQUE(memory_id_a, memory_id_b, detected_at)` still does not enforce unresolved-pair uniqueness — `detected_at` has one-second resolution. The protection is the unresolved-pair check repeated **inside** the write transaction, which holds the write lock for the duration. | Unchanged by this patch and inherent to the schema. Two proposals of the same pair against the same database file are serialised by that lock. |
| `EnqueueSimilarityCandidates` — the original, non-transactional writer — still exists and is exercised by the tests, but has no production caller. | Left in place deliberately: it is part of the `DatabaseManager` surface, and the shared validation helper is what keeps the two paths from drifting. The production path is `EnqueueSimilarityCandidatesTransactional` only. |

---

## 5. Files changed

| File | Change |
|---|---|
| `internal/core/similarity_candidates.go` | + bounded discovery, budgets, statistics, typed errors, overflow-safe arithmetic |
| `internal/core/similarity_candidates_store.go` | + transactional writer, `TransactionResult`, `CommitState`, `ensureSharedAttachedOnConn`, shared validation reuse, `ErrSharedUnavailable`, `ErrDatabaseUnavailable`, a reliable queue count |
| `internal/core/contradiction_log.go` | + single shared validation path (`validateAndNormaliseContradictionEvidence`); the queue load now pins a connection and verifies the shared attachment |
| `cmd/mpm/ops_resolve_contradictions.go` | rewritten flag parsing, one JSON envelope on every path, nested `propose`, budget plumbing, rewritten help |
| `internal/core/similarity_candidates_bounded_test.go` | new — bounded discovery suite + measurements |
| `internal/core/similarity_candidates_transactional_test.go` | new — transactional writer suite + read-path suite |
| `cmd/mpm/ops_propose_candidates_test.go` | rewritten onto `runBoundedProposal` |
| `cmd/mpm/ops_resolve_propose_cli_test.go` | new — CLI/JSON/flag contract suite |
| `docs/RECALL-MUTATION.md` | + "Bounded proposals (issue #21)", updated idempotency section, + coverage table |
| `docs/SPEC.md` | + supersession note, command signature, five CLI limits rows |
| `docs/ISSUE-21-PHASE-B-REPORT.md` | new — this document |

---

## 6. Post-commit corrections

The implementation commit is `013e8627c6bde4dab9e348531fcba92501caa2bc`. It is
**not amended**. A separate, additional local commit carries the corrections
below, so the implementation and its corrections are separately auditable.

### 6.1 The JSON error contract

`--json` now produces **exactly one** valid JSON envelope on every path,
including the ones where the command refuses to do anything at all:

| Refusal | `completion` | `commit_state` |
|---|---|---|
| Invalid budget value or scan limit | `invalid_flags` (with `--propose`) / `not_run` (without) | `refused` |
| Unsupported `--propose-limit=all` | `invalid_flags` / `not_run` | `refused` |
| Rejected `--propose` flag combination | `invalid_flags` / `not_run` | `refused` |
| Unknown flag | `invalid_flags` / `not_run` | `refused` |
| Database initialisation failure | `db_unavailable` / `not_run` | `refused` |
| Proposal failure | the phase that refused | as the transaction ended |
| Queue-load failure | the phase that refused | `refused` |

The refusal envelope is constructed **without consulting the database**. A
refusal that opened a database to describe itself would be a refusal with
side effects, and the whole point of refusing early is that the operator
gets the same answer whether or not a database happens to exist. The tests
prove this with a poisoned database manager: its sentinel message must
**not** appear on stderr.

Two implementation details make the contract hold:

- The invocation mode is determined by a **pre-pass** over the raw argument
  list, because a single-pass flag loop cannot know whether a document is
  owed when it returns early on argument #1. `--json` first and `--json` last
  therefore behave identically.
- In `--json` mode the help text goes to **stderr**. stdout carries the
  document and nothing else.

An explicit `--help` request is **not** a failure path: it prints the
help text and exits zero, in both modes, and that behaviour is unchanged
from the implementation commit. The contract above is about the failure
paths, and the help text says so in the same words ("ON ANY FAILURE
--json STILL EMITS EXACTLY ONE DOCUMENT").

### 6.2 Transaction failure classification

The classification is now driven by the **rollback**, because the rollback
is the operation that proves nothing was written:

- A failed **rollback** is identified explicitly, whatever triggered it —
  including a validation, budget or queue-read refusal — and the outcome is
  `rollback_failed` / `commit_state: "uncertain"`.
- A failed **COMMIT** whose **rollback was confirmed** is a **confirmed
  rollback** (`db_commit_rolled_back` / `commit_state: "rolled_back"`).
  SQLite refuses to roll back a transaction that has already committed, so a
  successful rollback after a failed commit proves the commit did not take
  effect. Nothing was written, and the report says so.
- A failed COMMIT whose rollback was **not** confirmed is `rollback_failed`:
  an uncertain commit outcome necessarily had an unconfirmed rollback, so
  the `uncertain_commit` completion name is **removed** rather than kept as
  a second way of saying the same thing.
- Both the primary error and the rollback error are preserved — the primary
  in the envelope's `error` field, the rollback in the nested
  `rollback_error` field.
- `candidates_proposed` is zero unless committed rows were confirmed.
- The human output and the JSON output agree about transaction certainty.

The fault-injection seam used by the core tests
(`SetProposalTxFaultsForTest`) is **exported** so the CLI layer can drive
the same end-to-end fault injection. The tests therefore assert what the
transaction actually did, not a synthetic `TransactionResult`.

### 6.3 Documentation accuracy

The `MaxCompareBudget` constant's comment no longer claims a runtime for the
10,000,000-pair ceiling. The ceiling exists to make the scan **bounded**; it
has not been exercised at that scale, and no runtime figure is claimed for
it. See §2.2.

### 6.4 Files changed by the corrective commit

| File | Change |
|---|---|
| `cmd/mpm/ops_resolve_contradictions.go` | + refusal envelope without the database, invocation pre-pass, rollback-dominant classification, `rollback_error`, help-to-stderr in JSON mode |
| `cmd/mpm/ops_resolve_propose_errors_test.go` | new — the JSON error contract |
| `cmd/mpm/ops_resolve_propose_transaction_test.go` | new — the transaction-certainty contract |
| `cmd/mpm/ops_resolve_propose_cli_test.go` | updated for the new completion vocabulary and the `rollback_error` key |
| `internal/core/similarity_candidates.go` | + the `MaxCompareBudget` comment correction (§6.3) |
| `internal/core/similarity_candidates_store.go` | + `SetProposalTxFaultsForTest` exported seam |
| `internal/core/similarity_candidates_transactional_test.go` | updated for the exported seam; + three failed-rollback tests |
| `docs/RECALL-MUTATION.md` | + the refusal contract, the rollback/confirmed-rollback semantics, the corrected completion vocabulary, `rollback_error` |
| `docs/ISSUE-21-PHASE-B-REPORT.md` | + this section; + §2.2's ceiling clarification; + one vocabulary fix in §4.1 |

### 6.5 Validation gate for the corrective commit

All commands were run inside the worktree
`.claude/worktrees/issue-21-bounded-proposals`, on top of the implementation
commit `013e862`.

| Check | Result |
|---|---|
| `make build` | **PASS** — exit 0; all five binaries built to `.build/bin/` |
| `make test` | **PASS** — exit 0; 4 589 PASS, 26 SKIP, **0 FAIL** |
| `make test-race` | **PASS** — exit 0; 4 589 PASS, 26 SKIP, **0 FAIL**, **0 `DATA RACE`** |
| `make test-core-precommit` | **PASS** — exit 0 |
| `make test-tools-precommit` | **PASS** — exit 0 |
| `make test-testenv-precommit` | **PASS** — exit 0 |
| `gofmt -l` on all modified/created Go files | **clean** — no output |
| `go vet -tags fts5 ./...` from `internal/core/` | **PASS** — exit 0, no output |
| `go vet -tags fts5 ./...` from the repository root | **PASS** — exit 0, no output |

`TestDrillE2E_ClaudeCode` **skipped** in both runs. A skip is a skip — it
is not counted as a pass, and the reason is recorded verbatim:

```
[claude stderr] [claude-code:unrecognized_model] {"model":"MiniMax-M3.1-Flash","query_source":"sdk"}
    drill_e2e_claude_code_test.go:179: Claude Code rejected the harness model
    (unrecognized_model on stderr); set MPM_E2E_CLAUDE=1 to bypass this gate
--- SKIP: TestDrillE2E_ClaudeCode (29.21s)
```

The test shells out to the real `claude` CLI, and the ambient session model
(`MiniMax-M3.1-Flash`) is rejected by Claude Code, so the test's own gate
skips it. It is the same ambient dependency documented in §2.4, it skips on
the unmodified baseline `9dba5e9` as well, and it is not affected by this
patch. It did not fail in either run.

Package timings from the `make test` run:

```
ok  github.com/flowbyte-com/mpm/cmd/mpm                    340.833s
ok  github.com/flowbyte-com/mpm-core                        195.867s
ok  github.com/flowbyte-com/mpm-core/tools                   28.936s
ok  github.com/flowbyte-com/mpm/internal/testenv               2.739s
ok  github.com/flowbyte-com/mpm/cmd/mpm-mcp                   3.494s
[test-agent-adapters] Python: 281 tests (floor 150)
[test-agent-adapters] JS: 165 passing tests (floor 100)
[test-agent-adapters] TS: 17 passing tests (floor 15)
[test-agent-installation] collected 427 tests (floor 300)
```

Focused runs for the new suites:

```
go test -tags fts5 -count=1 -run 'TestResolveContradictions_JSONErrorContract|\
TestResolveContradictions_Failed|TestResolveContradictions_Rollback|\
TestResolveContradictions_HumanAndJSON|TestClassifyProposalFailure|\
TestProposalFailureDetail|TestPrintProposalSummary' ./cmd/mpm/
    ok  github.com/flowbyte-com/mpm/cmd/mpm  1.874s

cd internal/core && go test -tags fts5 -count=1 \
    -run 'TestEnqueueSimilarityCandidatesTransactional' ./...
    ok  github.com/flowbyte-com/mpm-core  9.769s
```

No live database access, no deployment, no push, no PR, no merge and no
issue closure were performed. Every test in the new suites runs against a
disposable database under `t.TempDir()`.

---

## 7. Final-review corrections (HAL directive on PR #42)

A HAL final review of DRAFT PR #42 (`bb0bb9a`) authorised two targeted
corrections and no architectural redesign. This section records them. The
implementation commit (`013e862`) and the corrective commit (`bb0bb9a`) are
both preserved and untouched; this work is a **third**, additional commit.

### 7.1 Federated scan connection safety

**The defect.** `LoadSimilarityScanEntriesFederated` read the shared side
over the **pooled** database. SQLite's `ATTACH` is **per connection**, and
`internal/core/db.go` deliberately does not call `SetMaxOpenConns(1)` — a
single-connection pool self-deadlocks when a transaction holds the sole
connection and a nested helper performs a bare `db` lookup. The `ATTACH`
made at startup therefore lives on **one** pooled connection, and a pooled
query can land on another one that has no `shared` schema at all.

The failure was silent and total. The read failed with `no such table:
shared.memories`; that failure was classified as "no shared database"; and
the scan returned the **local half** as a successful result. The operator
saw a scan of half the corpus and a successful exit code.

The mirror-image defect sat next to it. A shared source that was
**configured but unreachable** — an `ATTACH` that never succeeded, a shared
file that was removed, a shared path replaced by a directory — produced the
same local-only success. A broken installation answered exactly like a
local-only one.

**The fix.**

| Installation | Answer |
|---|---|
| No shared source configured | local-only scan, and that is **success** |
| Shared source configured and reachable | **both** sides are read |
| Shared source configured but unreachable | **explicit error**, no partial result |

- Both sides are read on one **pinned** `*sql.Conn`, whose shared attachment
  is verified by `ensureSharedAttachedOnConn` before either side is read.
- The local and shared statements now come from shared helpers
  (`similarityScanQuery`, `scanSimilarityEntries`), so the two sides cannot
  drift apart.
- `DatabaseManager` gains `sharedConfiguredPath`: the shared path the
  operator **CONFIGURED**, recorded whether or not the `ATTACH` succeeded.
  `sharedPath` is only set on a successful attach, so it cannot tell "not
  configured" from "configured but unattachable" — and collapsing those two
  is exactly how a federated read answered with half the corpus and
  reported success.
- `ensureSharedAttachedOnConn` is **unchanged**, so the deployed count path
  and the transactional write path keep their existing error messages.

**What did not change.** The fingerprint filter, the
`deleted_at IS NULL` / `embedding IS NOT NULL` / `embedding != 'null'` /
`embedding_source != 'hash'` exclusions, the newest-first ordering, the
per-side `LIMIT` and the cross-side ID dedupe all hold on both sides. The
regressions pin each of them.

**Regression proof.** The new suite was run against the pre-fix
implementation (the file temporarily restored from `bb0bb9a`): the
fresh-pooled-connection test and all three "configured but unreachable"
subtests **fail**, and pass again once the fix is restored. The
"unconfigured stays local-only" and "filters/dedupe" tests pass on both
implementations — that is their point, and it is why they are there.

**One guard fired.** The repository's own
`TestDBSafety_NoLiveDBLeakInTests` rejected the new test file for calling
`NewDatabaseManager("")`, which falls through to `config.GetMPMDir()` —
the operator's live directory. The fixture was **fixed**, not
whitelisted: it now passes the temporary workspace root explicitly, so the
database it opens is hermetic by construction rather than by an
environment variable that happens to be set. (No live database was opened
in either form: `GetMPMDir()` honours `MPM_WORKSPACE`, which the first
version of the fixture set. The guard rejects the *call shape*, and it is
right to: the redirect is exactly the fragile thing it exists to catch.)

### 7.2 Complete JSON failure reporting

**The defect.** An adjudication is a **write**. It stamps a queue row as
operator-adjudicated, and that stamp is what makes the row eligible to
penalise a memory later. The command reported a failed adjudication by
writing a line to stderr and exiting 1 — with **no JSON document at all**,
even in `--json` mode. A consumer that asked for JSON got nothing: it could
not tell a failed adjudication from a crash, and it could not tell which
request failed.

The worse half was the partial success. An invocation that named three rows
and failed on the second had **already written the first**, and the report
described only the failure. The durable side effect was invisible.

**The fix.**

- The adjudication loop collects a per-request outcome —
  `adjudicated`, `failed` or `not_attempted` — and a failure now emits
  **exactly one** envelope carrying the top-level error **and** every
  request's outcome. The invocation still stops at the first failure (the
  existing behaviour), and the requests after it are reported as
  `not_attempted` rather than dropped.
- The human path confirms each write as it happens (`✓ Adjudicated queue
  row N.`) and reports the failure on stderr, so the earlier side effect is
  not concealed there either.
- `--json --help` now exits zero with the help text **inside** the
  envelope's new `help` key. Plain help text on stdout would be a second,
  unparseable document, and the contract is one document. The human
  `--help` path is unchanged: help text to stdout, exit zero. The two are
  pinned to be the *same* text.
- The envelope's key set gains `adjudications` and `help`. Every emission
  site sets `adjudications` to an empty **array** rather than leaving it
  null, so both the key set and the value's type are stable across every
  path.

**Regression proof.** Five of the six new CLI tests **fail** against the
pre-fix implementation. The sixth — the human help path — passes on both,
which is exactly what it is there to prove.

#### 7.2.1 The same defect, one layer up: the router

The handler-level fix above is **correct and was not enough**. The handler
honours `--json --help`; but a shell invocation does not reach the handler
first.

`Execute` intercepts a help request **before** dispatching, and the
interception renders the **family's** help page — plain text, to stdout.
The interception is skipped only when the request matches an entry in
`subcommandHelpOwners`, and that lookup is made against the token sequence
between the family name and the trailing `help` token:

```text
["ops", "resolve-contradictions", "--json", "help"]
         path = "ops resolve-contradictions --json"    (not an owner)
```

The `--json` flag pushed the path out of the owners map, the family page was
rendered, and `mpm ops resolve-contradictions --json --help` printed
**plain help text on stdout** in a mode whose contract says stdout carries
exactly one JSON document. A handler-level test cannot see this: the
handler is right and the dispatch in front of it is wrong.

**The fix is scoped, deliberately.** A first attempt — dropping flags from
the lookup path for every command in JSON mode — was **measured and
rejected**. Across all 27 owner paths, it changed more than one behaviour:
`mpm config capability --json --help` and `mpm config detect-embedding
--json --help` and `mpm ops changelog --json --help` flipped from exit 0 to
exit 1, and `mpm topic list --json --help` **executed the listing** instead
of printing help. The general rule was wrong.

The shipped rule is an explicit opt-in map, `jsonSubcommandHelpOwners`,
containing exactly one entry — `ops resolve-contradictions` — so that
exactly one command's JSON-mode help is answered by the handler that owns
the page. Measured again after the change: across all 27 owner paths with
`--json --help`, **exactly one line changed** (`ops resolve-contradictions`
went from exit 0 / plain text to exit 0 / JSON). Across 81 human
invocations (27 paths × `--help` / `-h` / `help`), the output was
**byte-identical** — 0 differences — so the human path kept everything it
had.

**The order of the two flags is irrelevant.** `--json --help` and
`--help --json` are the same request, and the JSON contract must hold for
both — otherwise `mpm ops resolve-contradictions --help --json` prints the
plain family page on stdout in JSON mode, which is the very thing the
contract forbids. The human path keeps its **trailing**-token rule
untouched (`mpm ops resolve-contradictions --propose --help` still resolves
to the family page); only the JSON branch accepts the help token anywhere
in the argument list, and only for a path in the opt-in map. All four
orderings (`--json --help`, `--json -h`, `--help --json`, `-h --json`) are
pinned.

**The human path is pinned too**, so the JSON fix cannot quietly widen
into it:

- `mpm ops resolve-contradictions --help` still prints the **command's own**
  page, on stdout, exit 0.
- `mpm ops resolve-contradictions --propose --help` still renders the
  **family** page — a human invocation that carries a flag in front of the
  help token resolves exactly as it did before.
- The JSON-mode help request is **inert**: it opens no database and writes
  no queue row. The routing change moved the request from the interception
  to the handler, so the inertness the interception had is now the
  handler's to prove; the test digests the database and the workspace
  before and after.

**Regression proof.** Against the pre-fix router, **all four** JSON-mode
cases of the one-document test fail and the inertness test fails; the two
human-page tests pass on both implementations, which is exactly what they
are there to prove. All six pass again once the fix is restored.

### 7.3 Files changed by the final-review commit

| File | Change |
|---|---|
| `internal/core/db.go` | + `sharedConfiguredPath` (configured ≠ attached) |
| `internal/core/similarity_candidates_store.go` | federated scan on a pinned, verified connection; shared statement/scan helpers; explicit error for a configured-but-unreachable shared source |
| `internal/core/similarity_scan_federated_test.go` | new — the federated scan contract (5 test functions, 3 of them subtest groups) |
| `cmd/mpm/ops_resolve_contradictions.go` | + per-request adjudication outcomes; one envelope on the adjudication failure path; `--json --help` as one document; + `adjudications` and `help` envelope keys |
| `cmd/mpm/router.go` | + `jsonSubcommandHelpOwners` (explicit opt-in) and the JSON-mode branch of `isSubcommandHelp` (+ `isJSONModeHelpRequest`); the human lookup path is **unchanged** |
| `cmd/mpm/ops_resolve_adjudicate_json_test.go` | new — the adjudication reporting contract (6 test functions) |
| `cmd/mpm/ops_resolve_json_help_router_test.go` | new — the JSON help contract **at the router** (4 test functions, 6 cases), plus the human-path and inertness pins |
| `cmd/mpm/ops_resolve_propose_cli_test.go` | updated for the two new envelope keys |
| `docs/RECALL-MUTATION.md` | + the scan-reads-both-sides contract, + the adjudication reporting contract, + the two new envelope keys, + the corrected `--help` contract, + the two new suites in the coverage table |
| `docs/SPEC.md` | + the 2026-10-20 update note on the scan and the adjudication reporting |
| `docs/ISSUE-21-PHASE-B-REPORT.md` | + this section |

### 7.4 Validation gate for the final-review commit

All commands were run inside the worktree
`.claude/worktrees/issue-21-bounded-proposals`, on top of the corrective
commit `bb0bb9a`, against the staged tree
`97f83eaae616e1a3f53e89019aa3b8c73f79f1e2`.

The committed tree differs from the gated tree by **this section and
nothing else** — one changed file, `docs/ISSUE-21-PHASE-B-REPORT.md`. A
section that records the gate's own results cannot be inside the tree the
gate ran against, by construction. Every Go file and every other document
is byte-identical between the two trees, which is checkable directly:

```
git diff --name-only 97f83eaae616e1a3f53e89019aa3b8c73f79f1e2 \
                   $(git rev-parse HEAD^{tree}) | grep -v '\.md$' | wc -l
0
```

| Check | Result |
|---|---|
| `make build` | **PASS** — exit 0; all five binaries built to `.build/bin/` (run as the dependency of `make test`) |
| `make test` | **PASS** — exit 0; 4 604 PASS, 26 SKIP, **0 FAIL** |
| `make test-race` | **PASS** — exit 0; 4 604 PASS, 26 SKIP, **0 FAIL**, **0 `DATA RACE`** |
| `make test-core-precommit` | **PASS** — exit 0 |
| `make test-tools-precommit` | **PASS** — exit 0 |
| `make test-testenv-precommit` | **PASS** — exit 0 |
| `gofmt -l` on every modified/created Go file | **clean** — no output |
| `go vet -tags fts5 ./cmd/mpm/...` | **PASS** — exit 0, no output |
| `go vet -tags fts5 ./...` from the repository root | **PASS** — exit 0, no output |
| `go vet -tags fts5 .` from `internal/core/` | **PASS** — exit 0, no output |

The two suite runs agree exactly: 4 604 PASS / 26 SKIP / 0 FAIL in both, and
the race run reports no `DATA RACE`. The baseline for this branch
(`bb0bb9a`) reported 4 589 PASS / 26 SKIP / 0 FAIL, so the 15 additional
passing tests are exactly the new ones — 5 in
`internal/core/similarity_scan_federated_test.go`, 6 in
`cmd/mpm/ops_resolve_adjudicate_json_test.go` and 4 in
`cmd/mpm/ops_resolve_json_help_router_test.go`. (Sub-cases inside a
single test function are counted once, by the function.)

`TestDrillE2E_ClaudeCode` **skipped** in both runs. A skip is a skip — it
is not counted as a pass, and the reason is recorded verbatim:

```
[claude stderr] [claude-code:unrecognized_model] {"model":"MiniMax-M3.1-Flash","query_source":"sdk"}
    drill_e2e_claude_code_test.go:179: Claude Code rejected the harness model
    (unrecognized_model on stderr); set MPM_E2E_CLAUDE=1 to bypass this gate
--- SKIP: TestDrillE2E_ClaudeCode (18.46s)
```

The test shells out to the real `claude` CLI, and the ambient session model
(`MiniMax-M3.1-Flash`) is rejected by Claude Code, so the test's own gate
skips it. It is the same ambient dependency documented in §2.4, it skips on
the unmodified baseline `9dba5e9` as well, and it is not affected by this
patch. It did not fail in either run.

Package timings from the `make test` run:

```
ok  github.com/flowbyte-com/mpm/cmd/mpm                    358.836s
ok  github.com/flowbyte-com/mpm-core                        185.802s
ok  github.com/flowbyte-com/mpm-core/tools                   28.345s
ok  github.com/flowbyte-com/mpm/internal/testenv               2.767s
ok  github.com/flowbyte-com/mpm/cmd/mpm-mcp                   3.828s
[test-agent-adapters] Python: 281 tests (floor 150)
[test-agent-adapters] JS: 165 passing tests (floor 100)
[test-agent-adapters] TS: 17 passing tests (floor 15)
[test-agent-installation] collected 427 tests (floor 300)
```

Focused runs for the new suites:

```
go test -tags fts5 -count=1 -v -run 'TestLoadSimilarityScanEntriesFederated' .
    --- PASS: TestLoadSimilarityScanEntriesFederated_ReadsBothSidesOnAFreshPooledConnection (0.10s)
    --- PASS: TestLoadSimilarityScanEntriesFederated_ConfiguredButUnreachableSharedSourceIsAnError (0.24s)
        --- PASS: .../the_attach_never_succeeded (0.06s)
        --- PASS: .../the_shared_file_was_removed (0.09s)
        --- PASS: .../the_shared_path_is_no_longer_a_file (0.09s)
    --- PASS: TestLoadSimilarityScanEntriesFederated_UnconfiguredSharedSourceStaysLocalOnly (0.06s)
    --- PASS: TestLoadSimilarityScanEntriesFederated_KeepsTheFiltersOnBothSides (0.09s)
    --- PASS: TestLoadSimilarityScanEntriesFederated_DeduplicatesAcrossSources (0.09s)
    ok  github.com/flowbyte-com/mpm-core  0.582s

go test -tags fts5 -count=1 -run 'TestResolveContradictions_JSON|\
TestResolveContradictions_NonJSONHelp|TestResolveContradictions_Help|\
TestResolveContradictions_Proposal|TestOpsResolveContradictions_JSONHelp|\
TestOpsResolveContradictions_HumanHelp|TestOpsResolveContradictions_Help' ./cmd/mpm/
    ok  github.com/flowbyte-com/mpm/cmd/mpm  9.482s
```

**The measurements behind the router change** (all against a real binary,
with an isolated `MPM_WORKSPACE` and `HOME`). The "pre-fix" binary in
these comparisons is built from `bb0bb9a`'s `cmd/mpm/router.go` with the
rest of the tree unchanged, so the comparison isolates the routing change
itself:

| Measurement | Result |
|---|---|
| 27 owner paths × `--json --help`, pre-fix vs post-fix | **exactly one line differs** — `ops resolve-contradictions` goes from `exit=0 kind=TEXT` to `exit=0 kind=JSON`; every other path's exit code and output kind is unchanged |
| 81 human invocations (27 paths × `--help` / `-h` / `help`), pre-fix vs post-fix | **0 differences** — the human path is byte-identical |
| `mpm ops resolve-contradictions --json --help` end to end | exit 0; stdout parses as **one** JSON document with 12 keys; `help` populated; `error` null |
| the same invocation, pre-fix | stdout carried the **plain-text** `mpm ops` family page |
| stderr on that invocation | unchanged from the pre-fix binary: the pre-existing `[mpm] scheduler: not running` warning, which never touches stdout |

**The regression proofs** — each new suite was run against the pre-fix
implementation, with the pre-fix file temporarily restored:

| Suite | Pre-fix result |
|---|---|
| `internal/core/similarity_scan_federated_test.go` | the fresh-pooled-connection test and **all three** "configured but unreachable" subtests **FAIL**; the "unconfigured stays local-only" and the filters/dedupe tests PASS (that is their point) |
| `cmd/mpm/ops_resolve_adjudicate_json_test.go` | **5 of 6** functions **FAIL**; the human-help one PASSes on both (that is its point) |
| `cmd/mpm/ops_resolve_json_help_router_test.go` | all **four** JSON-mode orderings and the inertness test **FAIL**; both human-path tests PASS on both |

No live database access, no deployment, no push, no PR, no merge, no issue
closure, no installed-binary replacement, no schema migration and no memory
mutation were performed. Every test in the new suites runs against a
disposable database under `t.TempDir()` or an isolated `MPM_WORKSPACE`. The
forensic snapshot `cmd/dbg_main.go.tmp` is preserved unchanged
(SHA-256 `d84e49cd…ae4575`).
