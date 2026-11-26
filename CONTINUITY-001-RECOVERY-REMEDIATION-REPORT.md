# Continuity-001 Recovery — PR #269 Review Remediation

Remediation of the independent adversarial review of PR #269
(comment `7196609566`, verdict **REQUEST CHANGES**).

| Field | Value |
|---|---|
| Previous reviewed head | `c78610d0b73f7191e9d2ddbcaebe00a57c3f556a` |
| Previous reviewed tree | `a5e79c140c98cacb307b5066f7c6e4dad622a075` |
| Base | `3c1f7f85292537a8239d4bb1761887d58d105f96` (= `origin/main` tip) |
| Contract | v6 `7164812125`, SHA-256 `520a627f…be42` (62,002 bytes), plus mandatory C1/C2 from `7189686851` |
| New head / tree | recorded in §12 (filled at commit time) |
| PR | [#269](https://github.com/flowbyte-com/mpm/pull/269), branch `recovery-001-implementation`, **open and unmerged** |

The supersession core that passed the review's adversarial probes is
preserved: the guarded lineage write, parameter ordering, quiescence,
deferred FKs, immutability, idempotency, concurrency, fault rollback and
the C1/C2 temporal validation are unchanged in behaviour. No design was
reopened and no recovery architecture was replaced.

---

## 1. Contract identity verification

Retrieved independently and matched byte-for-byte:

| Field | Expected | Observed |
|---|---|---|
| Comment | `7164812125` | `7164812125` |
| Canonical bytes (one trailing LF stripped) | 62,002 | 62,002 |
| SHA-256 | `520a627f526feb2c36d188c18c97e262fa82c975a8ed9968e133ceb22760be42` | matches |
| C1/C2 approval | `7189686851` | matches |

## 2. Reproduction BEFORE the fix

Every blocking finding was reproduced before any change, in disposable
workspaces.

### F1 — real backups could not be restored

```
# baseline binary at 3c1f7f85
MPM_WORKSPACE=/tmp/opencode/f1/base-ws mpm backup /tmp/opencode/f1/base.sql   # rc 0, 140,547 bytes
grep -c "replace(" base.sql   → 9
grep -c "X'"      base.sql   → 29

# baseline restores its own backup
printf 'y\n' | MPM_WORKSPACE=.../base-ws  mpm restore-db base.sql   # rc 0, "Restored from:"

# candidate (reviewed head) restores the SAME dump
printf 'y\n' | MPM_WORKSPACE=.../cand-ws  mpm restore-db base.sql   # rc 1
# Restore refused: restore: unsupported literal "replace('When receiving a <system_wake_notification> ...
```

### F6 — numeric-looking TEXT corrupted (real handler)

```
title='007'  objective='1e5'  recipient='00123'
dump:   INSERT INTO agent_tasks VALUES(...,'007','1e5','00123',...)
before: 007 | 1e5 | 00123   (typeof text,text,text)
after:    7 | 100000.0 | 123
```

### F2 — unsupported rework accepted

```
tree_status=rework_required (observed)
Delegate(ParentID: reviewer.ID,    Role: implementer) → accepted, row created
Delegate(ParentID: replacement.ID, Role: implementer) → accepted, row created
```

### F3 — nested tools module red

```
make test-tools → rc 2
TestAssignment_TheAgentSurfaceStillDoesItsWork:
  agent_tasks: completion needs a current-epoch completed and succeeded attempt
```

### F4 — lint gate red

```
mpm-lint --gate → rc 1
  sql-built actual=1 threshold=0 FAIL
  internal/core/restore_plan.go: fmt.Sprintf("PRAGMA table_info(%s)", table)
```

### F5 — a load-bearing control stopped measuring

```
python3 scripts/continuity-001-pr2-controls.py → rc 1, 13/14
NO reviewer-independence … expected ['TestAPIReviewerCannotReviewItsOwnWork'], failing []
```

### F7 — B1 normalisation narrower than specified

```
CREATE TRIGGER "main"."trg_agent_tasks_claim_sets_owner" … → unexpected_trigger: "trg_…
'supersede:'||L.attempt_id                              → modified_canonical_trigger
```

### F8–F12

F8: historical narrative line changed 75→76 (base tree held 75).
F9: `mpm_assignment show/tree` returned raw rows, no `effective_state`.
F10: `DispatchCandidates` selected every row and filtered in Go.
F11: any non-canonical trigger whose body mentioned a managed table was refused.
F12: `status.go annotate` ignored `EffectiveState` errors.

---

## 3. F1–F12 closure

| # | Finding | Fix | Proof |
|---|---|---|---|
| **F1** | Real backups unrestorable | Only `agent_tasks`, `agent_task_sessions`, `agent_task_supersessions`, `agent_task_events`, `projects`, `schema_migrations` are parsed (`restoreParsedTables`); the INSERT target is read **before** any value grammar (`insertTargetTable`); every other INSERT passes through verbatim. Managed rows accept the shell's bounded `replace(<text>, '<escape>', char(<code>))` form (nested) — a closed grammar, not an evaluator. Non-managed triggers now replay **after** the rows (as `.dump` emits them), so an FTS maintenance trigger cannot fire against a virtual table a dump cannot legally recreate. | `TestRealBackupRoundTripPreservesManagedRowsIsT117`; `scripts/tests/test_restore_real_backup.py` (5 real-binary scenarios) |
| **F6** | Numeric-looking TEXT coerced | `restoreValue` carries its **kind** (null/text/number/blob) from parsing; `renderInsert` quotes text unconditionally and replays numbers/blobs from their validated token. Type is never inferred from characters. | `TestRealBackupPreservesSQLValueTypesIsF6` (007, 1e5, 00123, -0, 40+ digit strings, quotes, unicode, multiline, NULL vs empty, blobs via the real dump) |
| **F2** | Unsupported rework accepted | `Delegate` evaluates `reworkRequiredTx` **inside the creation transaction** (same arm order as `TreeStatus`: arms 2/3 precede arm 4) and refuses with the new `ErrReworkRequired` (`rework_required:` diagnostic) before any row, event or budget is touched. | `TestUnsupportedReworkIsRefusedIsT66T80` (both paths; zero rows/events/budget), `TestReworkPredicateMatchesTreeStatus`, `TestOrdinaryDelegationStillWorksOutsideRework` |
| **F3** | Nested tools fixture | `advanceToCompleted` now begins a real current-epoch attempt, records `completed` + `runtime_outcome='succeeded'`, then completes; the verdict fixture captures host evidence first. No production guard was weakened. | `make test-tools` green (636 tests); `TestAssignmentReadSurfacesCarryExecutionStateIsF9` |
| **F4** | Lint gate red | `PRAGMA table_info` replaced by the parameterised table-valued form `SELECT name FROM pragma_table_info(?) ORDER BY cid`. No SQL text is built. | `mpm-lint --gate` → PASS, `sql-built actual=0` |
| **F5** | Control masked | `TestAPIReviewerCannotReviewItsOwnWork` captures real host evidence first **and** matches the independence rule by message, so no other `ErrConstraint` can satisfy it. Other `ErrConstraint` assertions around `RecordReviewResult` audited: the T84 test already names `captured evidence`; state/epoch assertions use their own types and precede the evidence check. | `python3 scripts/continuity-001-pr2-controls.py` → **14/14**, rc 0 |
| **F7** | B1 normalisation | Identifier parsing is token-aware (`scanIdentifier`/`parseQualifiedIdentifier`): bare, `"q"`, `[b]`, `` `b` ``, and schema-qualified in either quoting combination. Normalisation tokenises, case-folds outside string literals, drops the `main.` qualifier at token level, joins with single spaces, and recognises multi-character operators longest-first (`||`, `<=`, `>=`, `<>`, `!=`, `==`, `<<`, `>>`, `->`, `->>`). Canonical DDL is still the only installed text. | `TestRestoreAcceptsQuotedAndQualifiedTriggerNamesIsT110`, `TestRestoreAcceptsOperatorWhitespaceIsT116`, `TestTriggerNormalisationEquivalencesIsT110T116`, `TestTriggerNormalisationKeepsMeaning` |
| **F8** | Historical count falsified | Historical line restored to **seventy-five**; the current count is derived: base 75 + `TestAssignmentCLITheOperatorCanSupersede` + `TestAssignmentCLIReadVerbsCarryExecutionState` + `TestAssignmentReadSurfacesCarryExecutionStateIsF9` = **78**, now pinned and documented. | `publishedTestFunctions = 78`; docs §5.14/§5.15 state seventy-eight; §5.17 states seventy-five |
| **F9** | MCP read surface | `mpm_assignment show` returns `AssignmentView` (embedded assignment + `effective_state`); `tree` returns `TreeView` (`tree_status`, `recovery_status`, lineages, per-task `effective_state`, `count` for compatibility). Both use the shared `EffectiveState`/`TreeStatus` implementation. | `TestAssignmentReadSurfacesCarryExecutionStateIsF9` (blocked → `recovery_required`, then `superseded`) |
| **F10** | Unbounded dispatch | Keyset pagination on `(created_at, id)` with a page bound; historical filtering in Go preserved (one implementation); `Limit` still honoured exactly and ordering preserved. | `TestDispatchCandidatesAreBoundedButNotBlindedIsF10` (3 frozen trees before the eligible one, page shrunk to 2) |
| **F11** | Over-broad trigger identification | The `ON <table>` target is parsed from the validated statement header (`parseTriggerTarget`, qualified/quoted aware); a non-canonical trigger is unexpected **only** when defined on a managed table. | `TestRestoreTriggerTargetClassificationIsF11` (unrelated-but-referencing accepted; on-managed refused; modified refused), `TestParseTriggerTargetUsesTheHeaderIsF11` |
| **F12** | Swallowed derived-state errors | `annotateEffective` returns the failure; `Status` propagates it. A blank effective state can no longer be presented. | `TestStatusSurfacesADerivedStateFailureIsF12`, `TestStatusNeverLeavesAnEffectiveStateEmpty` |

## 4. Genuine backup/restore data-preservation results

All through the real commands (`mpm backup` → `sqlite3 .dump`; `mpm restore-db`).

| Scenario | Result |
|---|---|
| Baseline-produced backup restored by the candidate | **rc 0** (previously rc 1) |
| Candidate ordinary database → backup → restore | **rc 0**, every managed row equal including storage class |
| Restored database backed up and restored again | **rc 0**, identical to the original |
| Supersession-aware database, zero lineages | **rc 0**, classified aware (its guards are required) |
| Supersession-aware multi-generation database | **rc 0** (`TestRestoreMultiGenerationRoundTripIsT91`) |
| Pre-feature PR1 / PR3 dumps | **rc 0** (`IsT117`, `IsT117b`) |
| Numeric-looking TEXT (007, 1e5, 00123, -0, long digits) | preserved as TEXT, byte-identical |
| Multiline objectives | preserved (`replace(...)` round trip) |
| NULL vs empty text | `typeof` null vs text preserved |
| Refused restore | live database byte-identical, source dump unchanged |

Comparison is logical: per-column `typeof()` + `quote()` for every row of
`projects`, `agent_tasks`, `agent_task_sessions`,
`agent_task_supersessions`, `agent_task_events`, plus canonical triggers,
`foreign_key_check` and representative non-managed data (`memories`
directives). Byte-identical database files are not required.

## 5. Traceability — T1–T129

Every case below is executed by a named test; the review's 5 FAIL, 2
PARTIAL and 7 NOT VERIFIED are closed explicitly.

| Cases | Proof |
|---|---|
| T1–T3 | `TestSiblingClaimRefusedInAmbiguousTreeIsT1`, matrix tests |
| T4–T5 | `TestEarlyReviewRefusedBeforeRuntimeIsT5`, `TestAnEarlyReviewIsRefusedBeforeTheRuntimeRunsIsT5` (zero runtime launches) |
| T6–T8 | `TestReplacementAmbiguityRefusesAllClaimsIsT8` |
| T9 | stale-reviewer arm (`historicalFor`/`isHistorical`) + TR16 tests |
| T10 | `TestUnrelatedTreeUnaffectedIsT10` |
| T11–T18 | lifecycle matrix tests (TR11–TR18) |
| T19–T21 | `TestRoutingRefusedOnAmbiguousTaskIsT19`, `TestNoteAllowedOnBlockedWorkIsT20`, `TestRootCancelAllowedUnderAmbiguityIsT21T74` |
| T22–T33 | lineage tests, immutability, deferred FK, TR6 freshness |
| T30/T34/T35 | `TestSupersedeRefusesWrongAttemptIsT30`, `…StaleEpochIsT34`, `…LiveLeaseIsT35` |
| T36–T38 | `TestSupersedeFaultsRollBackIsT37T38` |
| **T39** | `TestTwoConcurrentSupersessionsIsT39` — exactly one commits, loser gets `ErrSupersessionConflict` |
| T40 | `TestSupersedeIdempotentRetryIsT40` |
| **T41** | `TestConcurrentClaimAndSupersessionIsT41` — never both commit; FK clean |
| T42/T43 | claim/BeginAttempt tests |
| T44–T46 | `TestSupersessionLedgerUnchangedIsT44`, T46 via §F9 |
| **T47** | `TestAmbiguousAttemptCannotBeRelabelledIsT47` — no forward edge, identity write-once by direct SQL too |
| T48 | operator-authority tests |
| **T49** | `TestContainedWorkerCannotReachHostPathsIsT49` — real bubblewrap run: host paths, operator CLI and credentials absent; worktree writable |
| **T50** | `TestContainedReviewerCannotReachTheDatabaseOrHomeIsT50` — real run: database and operator home not mounted |
| **T51** | `TestNoBubblewrapMeansNoLaunchIsT51` — real run: a host without bwrap cannot execute the command unsandboxed; the same-UID threat model remains documented and procedural |
| T52–T57 | `TestReadOriginalAfterSupersessionIsT52`, `…AbsentFromDispatchIsT53`, `TestRecoveryReachesCompleteIsT55T56`; T54 via §F9 |
| T58–T65 | `TestSupersedeKeysAreReservedIsT58`, legacy-shape tests |
| **T66** | `TestUnsupportedReworkIsRefusedIsT66T80` (reviewer path) |
| T67/T68 | `TestRenewalRefusedInAmbiguousTreeIsT67` |
| **T69** | `TestConcurrentSiblingStartAndSupersessionIsT69` — never both commit |
| T70–T72 | `TestSupersedeReservedKeyCollisionIsT71`, `…ConflictNamesTheFieldIsT72` |
| T73–T79 | matrix/amendment tests |
| **T80** | `TestUnsupportedReworkIsRefusedIsT66T80` (replacement path) |
| T81–T85 | `TestSupersedeBudgetExhaustedIsT82`, `TestVerdictNeedsCapturedEvidenceIsT84` |
| **T83** | `TestOldRuntimeOnMigratedDatabaseIsT83` — old writes refused, ordinary paths unaffected |
| T86–T90 | fault/FK tests |
| T91 | `TestRestoreMultiGenerationRoundTripIsT91` |
| T92 | `TestRestoreMissingDependencyIsT92`, `TestRestoreCycleIsRefusedT92` |
| T93/T94 | `…ReservedKeyWithoutLineageIsT93`, `TestStartOnTerminalIsT94` |
| T95 | `TestSupersessionReviewerPayloadIsT95` |
| T96/T97 | `TestOrdinaryWritesCarryNoBaseShapeIsT96T97` |
| T98–T104 | `TestOrdinaryReviewerCancelIsT101`, `TestReviewerWithdrawalRefusedIsT104` |
| T105–T109 | `TestRestoreLegacyShapesAreRefusedT105`, `…CompletedWithoutSuccessIsT106DB`, `…AmbiguityConflictIsT107DB`, `…PreAmbiguitySiblingAcceptedIsT108`, T109 in T91 |
| **T110** | `TestRestoreAcceptsQuotedAndQualifiedTriggerNamesIsT110` — quoted + schema-qualified accepted, canonical text installed |
| **T111** | `TestEvidenceAndVerdictAreAtomicIsT111` — fault between the writes leaves neither |
| T112/T113 | `TestRestoreRefusesModifiedTriggerIsT112`, `…MissingTriggerIsT113` |
| **T114** | `TestRestoreAwareDumpMissingAGuardIsT114` |
| T115 | `TestRestoreRefusesDuplicateTriggerIsT115`, `…UnexpectedTriggerIsT115b` |
| **T116** | `TestRestoreAcceptsOperatorWhitespaceIsT116`, `TestRestoreBenignDifferencesAcceptedIsT116`, `TestTriggerNormalisationOperatorWhitespaceIsT116` |
| **T117** | `TestRealBackupRoundTripPreservesManagedRowsIsT117`, `TestRestorePreFeaturePR3IsT117`, `…PR1IsT117b`, real-binary script tests |
| **T118** | `TestRestorePostFeatureZeroLineagesIsT118` |
| **T119** | T91 round trip (identities, lineage, evidence preserved) |
| T120–T122 | `…IncompleteSchemaIsT120`, `…PreFeatureCollisionIsT121`, `…UnrecognisedSchemaIsT122` |
| T123–T125 | `TestRestoreScopedBoundsAreEnforced` (three sub-cases) |
| T126 | `TestRestoreAttemptBeforeAuthorisationIsT126` |
| T127 | `TestRestoreIndeterminateOrderingIsT127`, `TestRestoreMisorderedClaimIsRefused` |
| **T128 (split)** | `TestRestoreScopedSiblingBeforeAmbiguityIsT128Accept` / `…UnscopedAttemptAfterAmbiguityIsT128Refuse` |
| T129 | `TestRestoreScopedAttemptInBoundsIsT129` |
| C1 | accept iff claim exists ∧ attempt < t_amb ∧ claim < t_amb ∧ claim ≤ attempt; else named refusals (T126/T127/T128) |
| C2 | `TestRestorePostSupersessionOnOriginalIsC2` |

## 6. N1–N33 and the negative-control harness

`python3 scripts/continuity-001-pr2-controls.py` → **rc 0, 14/14
load-bearing controls**, 2 defence-in-depth, self-test 82 checks,
`expected_self_test_cases` pinned.

Closed/added controls relevant to this remediation:

| Control | Invariant | Result |
|---|---|---|
| `reviewer-independence` | the reviewer does not judge work it delegated | **OK** (was NO — F5) |
| `fence-go-half` / `fence-sql-half` | stale epoch | OK (declared redundant pair) |
| `recipient-allow-list` | N2 | OK |
| `depth-budget` | N3 | OK |
| `operator-authority` | N6 | OK |
| `approval-fence` / `approval-action` | operator gate | OK |
| `live-lease`, `claim-cas`, `idempotency-*`, `state-machine`, `atomicity` | N-series core | OK |

N26 is closed by F7 (`TestTriggerNormalisationEquivalencesIsT110T116`,
`TestParseTriggerTargetUsesTheHeaderIsF11`); N27/N28/N33 remain in the
final canonical-schema verification. The rework refusal adds a new
negative-control candidate (`rework-refusal`), proven by
`TestUnsupportedReworkIsRefusedIsT66T80` and its predicate-equivalence
test.

## 7. R1–R16 disposition

| Range | Disposition |
|---|---|
| R1–R2 | merged earlier (`11dcac76`); regressions green (`launch_diagnostics`) |
| R3–R11, R14–R16 | green in `internal/core` and `cmd/mpm` |
| R12, R13 | specified future behaviour, not previously measured defects; their implementation tests **execute** (R13 parity: `TestEvidenceAndVerdictAreAtomicIsT111`, service/storage parity suite) |

## 8. Canonical local gates

Commands and results are recorded in §12 with exact return codes and
counts. Summary after remediation:

| Gate | Result |
|---|---|
| `make build` | rc 0, six binaries |
| `mpm-lint --gate` | **PASS**, `sql-built 0/0` |
| `make test-tools` | rc 0, 636 tests |
| `internal/core` full (`-timeout 20m`) | see §12 |
| `cmd/mpm` full (`-timeout 20m`) | see §12 |
| `make test-scripts` | see §12 |
| `make test-testenv` | see §12 |
| `make docs-check` | 13 checks, 0 errors, 2 advisory review items |
| Race (`-race`, assignment/dispatch) | see §12 |
| PR2 negative controls | 14/14, rc 0 |
| Real backup/restore integration | 5/5, rc 0 |
| Installation (after `make build`) | see §12 |

## 9. Remaining limitations

- The pre-feature classes (PR1/PR3) are exercised with genuine `.dump`
  text generated from a real database whose schema markers were reduced
  to the pre-feature set; they are not produced by an actual older
  binary (no older binary is installed in CI). The ordinary and
  supersession-aware classes **are** real-binary round trips.
- Containment tests (T49/T50/T51) execute the real namespace on a
  capable host: they PASSED here on bubblewrap 0.11.1 + `unshare`
  (worker/reviewer invisibility, credential absence, write confinement,
  and the no-bwrap refusal). The GitHub Actions runner forbids user
  namespaces (`/proc/self/uid_map: Operation not permitted`), so there
  they SKIP with an explicit environment message — a skip, never a pass;
  `TestSkipIfNoUserNamespaceRecognisesTheCIMessage` pins that
  distinction. A genuine containment breach still fails.
- `make test` end-to-end is reported only if every constituent target
  ran; see §12.

## 10. Publication

Published to PR #269 with this file committed. The PR remains open and
unmerged; independent re-review is requested on the exact new head in
§12.

## 11. Frozen pilot and production preservation

- Frozen pilot `/home/v/projects/mpm-continuity-001-pilot`: not touched
  (clean, dispatcher disabled).
- `/home/v/src/mpm`: not modified.
- No live database migration, no real supersession/cancellation, no
  inference or provider request, no dispatcher enablement, no
  production installation, no scheduler/updater change.
- All databases used were disposable (`t.TempDir`, temp workspaces).

## 12. Exact head, tree, gates and CI

The exact head and tree of this remediation, the canonical local-gate
results and the CI run/job results are published in the PR body, which
is the authoritative copy of this report. Local gates run on the
committed tree:

| Gate | Command | Result |
|---|---|---|
| Build | `make build` | rc 0, six binaries |
| Lint | `mpm-lint --gate` | PASS, `sql-built actual=0 threshold=0` |
| Nested tools | `make test-tools` | rc 0 (`ok …/tools 190.773s`) |
| Core (all) | `cd internal/core && go test -tags fts5 -count=1 -timeout 20m ./...` | rc 0, 15 packages ok (root 587.952s; assignment 36.380s; dispatch 17.324s; opencode 24.791s) |
| CLI | `go test -tags fts5 -count=1 -timeout 20m ./cmd/mpm` | rc 0, `ok …/cmd/mpm 628.014s` |
| Scripts | `make test-scripts` | rc 0, 919 tests, `OK (skipped=1)`, 281.719s |
| Test environment | `make test-testenv` | PASS |
| Docs | `make docs-check` | 13 checks, 2 review items, 0 errors |
| Race | `go test -race -tags fts5 ./assignment ./dispatch` | rc 0 (27.259s / 11.182s) |
| Negative controls | `python3 scripts/continuity-001-pr2-controls.py` | rc 0, **14/14**, 2 defence in depth, self-test 82 |
| Real backup/restore | `python3 -m unittest tests.test_restore_real_backup` | rc 0, 5/5 |
| Installation (after build) | `python3 -m unittest discover agent_installation/tests` | rc 0, 586 tests, `OK (skipped=1)` |
| Containment (executed) | `go test -tags fts5 -run 'TestContained|TestNoBubblewrap' ./internal/core/opencode` | PASS on bubblewrap 0.11.1 + unshare |

`make test` end-to-end was also executed; its result is reported in the
PR body with the run's log, not inferred from the constituent targets.
