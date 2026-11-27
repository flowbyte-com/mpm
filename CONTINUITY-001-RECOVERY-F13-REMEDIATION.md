# Continuity-001 — PR #269 Final Targeted Remediation

Remediation of the independent remediation review of PR #269
(comment `7202245109`, verdict **REQUEST CHANGES**).

| Field | Value |
|---|---|
| Previous reviewed head | `5fb0316413bf558114b63b695aebf20ec4885a91` |
| Previous reviewed tree | `535e7f473515c47901eea9f63762ff8a704e1dc1` |
| Base | `3c1f7f85292537a8239d4bb1761887d58d105f96` (= `origin/main`) |
| Contract | v6 `7164812125` (SHA-256 `520a627f…be42`) + mandatory C1/C2 `7189686851` |
| New head / tree | published in the PR body |
| PR | [#269](https://github.com/flowbyte-com/mpm/pull/269), open and unmerged |

The reviewed architecture is unchanged. This remediation touches only
the B1 trust boundary (F13/F11), the dispatch page-size knob (F10
residual), and the containment verification harness.

---

## 1. F13 — reproduction BEFORE

Reviewed head `5fb03164`, real CLI, disposable workspace:

```
mpm backup aware.sql                      # rc 0 (supersession-aware schema)
cat >> aware.sql <<'SQL'
CREATE TRIGGER trg_begin_shadow BEFORE INSERT ON agent_tasks
FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'shadow-injected'); END;
SQL
mpm restore-db aware.sql                  # rc 0  ← DEFECT
SELECT COUNT(*) FROM sqlite_master
 WHERE type='trigger' AND name='trg_begin_shadow';          # 1
INSERT INTO agent_tasks …;                # Error: stepping, shadow-injected (19)
```

Cause: `parseTriggerTarget` located the header by a raw
`strings.Index(upper, "BEGIN")`, and the trigger NAME contained
`BEGIN`; the header was truncated before `ON`, the target was unknown,
classification skipped the trigger, and `addSchema` silently replayed
it. The final schema check looked for `name LIKE 'trg\_agent\_task%'`,
so it could not see it either.

## 2. F13 — refusal AFTER

Same dump, final head:

```
mpm restore-db aware.sql                  # rc 1
Restore refused: unexpected_trigger: trg_begin_shadow
```

No shadow trigger exists in the restored database, the source database
is byte-for-byte unchanged, and the source dump is unchanged.

## 3. The three enforcement layers

### A. Token-aware header parsing

`parseTriggerHeader` (new) resolves the name and the `ON` target from
the TOKEN stream produced by the existing tokenizer:

- `CREATE` / `TRIGGER` / optional `IF NOT EXISTS` as KEYWORD tokens;
- the name and the target via `qualifiedIdentifierAt`, which resolves
  `schema.name`, `"quoted"`, `[bracketed]`, and either quoting
  combination, taking the final component;
- the header's `ON` is the first `ON` KEYWORD after the name, and the
  body begins at the first `BEGIN` KEYWORD after it.

A trigger name, a quoted identifier or a string literal containing
`BEGIN` (or `ON`) is a single token and cannot terminate the header.
`sqlTokenInvalid` was added for an unterminated quoted identifier: it is
never accepted as a name or a target (previously the tokenizer filed it
as an identifier, which made an unparseable target look real).

`parseTriggerStatement` now delegates to the same parser, so the
classification map, the canonical inventory and the replay planner share
one name resolution.

### B. Fail-closed classification

- `restoreDumpInventory` gained `triggerTargetKnown`; classification
  records a target only when it was POSITIVELY resolved.
- A non-canonical trigger whose target is unknown is now
  `unexpected_trigger` — absence of proof that it is unmanaged is not
  proof that it is safe.
- `addSchema` no longer appends on a parse failure: an unclassifiable
  trigger statement, an unclassifiable target, or a trigger on a managed
  table each REFUSE the restore. Nothing falls through to
  `userTriggerStmts`.
- Legitimate unrelated triggers (including ones that READ a managed
  table) still replay after the rows; the SQL trust boundary is
  unchanged (bodies still come from the dump only for non-managed
  tables, and managed triggers are installed from canonical text).

### C. Independent final schema verification

`verifyFinalManagedSchema` now:

- enumerates triggers through `sqlite_master`, and decides ownership by
  `tbl_name` — the four managed tables derived from
  `managedRestoreTables` — never by a name prefix;
- requires every canonical trigger to exist, carry its trusted body, and
  be bound to the table its canonical definition names
  (`checkTriggerBinding`);
- requires NO other trigger on a managed table, whatever its name;
- keeps the canonical table/column/index checks and
  `PRAGMA foreign_key_check`, inside the existing restore transaction
  before commit. Any failure rolls back to `.pre-restore` and leaves the
  source dump untouched.

## 4. Independent backstop evidence

`TestFinalSchemaVerificationIsAnIndependentBackstopIsF13` calls
`verifyFinalManagedSchema` DIRECTLY — the classifier is bypassed
entirely — against a live canonical schema with:

| Mutation | Result |
|---|---|
| unexpected trigger `rogue_shadow` on `agent_tasks` | refused, `final_canonical_schema_mismatch` names it |
| unexpected `trg_begin_shadow` on `agent_tasks` | refused |
| canonical trigger dropped | refused, `missing` |
| canonical body weakened | refused, `drifted` |
| canonical name bound to `projects` | refused |
| wrong-binding comparison (`checkTriggerBinding`) | refused, `is defined on` |
| clean canonical schema | passes |

This is the backstop that stays effective if an earlier layer regresses
again: it does not consult classification, names or the dump at all.

## 5. F11 closure

The over-broad direction and the fail-open direction are both closed:

- a trigger on an unrelated table that merely references a managed table
  replays verbatim (`TestRestoreTriggerTargetClassificationIsF11`, and
  the end-to-end `projects_audit_managed` restore: rc 0, installed);
- an unknown trigger on any managed table is refused, by name or not
  (`TestRestoreRefusesUnexpectedTriggerOnEveryManagedTableIsF13`,
  `TestRestoreRefusesBEGINNamedManagedTriggerIsF13`,
  `TestRestoreRefusesQuotedBEGINNamedManagedTriggerIsF13`);
- an unparseable header fails closed
  (`TestRestoreRefusesUnparseableTriggerHeaderIsF13`,
  `TestTriggerHeaderParsingIsTokenAwareIsF13`).

## 6. F10 defensive correction

`DispatchCandidatePageSize` is clamped by `dispatchPageSize()`: a zero
or negative value uses `defaultDispatchCandidatePageSize` (256), so the
keyset cursor can always advance. Ordering, historical exclusion,
bounded retrieval and the live-candidate limit are unchanged.
`TestDispatchPageSizeIsClampedIsF10` drives 0, −1 and −256 with a
timeout guard and returns the eligible candidate each time.

## 7. T115 / N28 / N33

| Case | Result |
|---|---|
| T115 duplicate trigger | `TestRestoreRefusesDuplicateTriggerIsT115` (unchanged) |
| T115 unexpected trigger | `TestRestoreRefusesUnexpectedTriggerIsT115b` plus the four F13 fixtures (BEGIN-named, quoted, schema-qualified, arbitrary name, all four managed tables) |
| N28 final canonical schema | `TestFinalSchemaVerificationIsAnIndependentBackstopIsF13` (independent of classification) |
| N33 canonical re-creation from trusted DDL | unchanged; canonical triggers are still installed from constants, and the final check compares their bodies and bindings |

## 8. T49/T50 — capable-host evidence

The containment assertions are unchanged; only the SKIP predicate was
narrowed.

- `requireUserNamespace` probes the exact production prerequisite,
  `unshare --user --map-root-user true`, BEFORE the run. A skip happens
  only when that probe fails and its output names a user-namespace
  prerequisite (`uid_map`, `newuidmap`, `user namespace`, `userns`).
- A launcher failure is never converted into a skip: once the
  prerequisite is established, any failure is `t.Fatalf`.
- `TestUserNamespaceLimitationClassificationIsNarrow` pins the
  distinction: the CI `uid_map` message classifies as environmental,
  while `bwrap: bind mount … Operation not permitted`, a containment
  assertion failure and a missing runtime binary do not.

Execution record:

- **2026-11-26 ~16:03 UTC, head `5fb03164`** (containment code identical
  to the final head): both tests executed and PASSED on this host —
  `--- PASS: TestContainedWorkerCannotReachHostPathsIsT49 (0.02s)`,
  `--- PASS: TestContainedReviewerCannotReachTheDatabaseOrHomeIsT50 (0.01s)`.
  No inference or provider request occurred (the probe script is bound
  as the runtime binary and never contacts the sidecar).
- **Final head**: the same host now refuses the prerequisite —
  `unshare: write failed /proc/self/uid_map: Operation not permitted`,
  kernel audit `apparmor="DENIED" profile="unprivileged_userns"
  capability=21 capname="sys_admin"`,
  `kernel.apparmor_restrict_unprivileged_userns = 1`, no `newuidmap`, no
  sudo. Both tests therefore report `--- SKIP` with that exact message;
  this is an environment limitation, not a pass.
- Host: kernel `7.0.0-44-generic`, util-linux `2.41.3`, bubblewrap
  `0.11.1`; `unshare --user true` succeeds, `--map-root-user` is denied.
  Per the mission's hard stop, host security settings were NOT changed
  to obtain a result.

T51 (`TestNoBubblewrapMeansNoLaunchIsT51`) passes: the documented
same-UID limitation is unchanged.

## 9. Regression preservation

Re-run after the change (all green): real baseline/candidate backup
restoration and the five real-CLI round trips; multi-generation and
second-generation round trips; SQL type/value preservation; canonical
trigger normalisation (T110/T116); legacy PR1/PR3 compatibility (T117);
malformed restore rollback; C1/C2 temporal validation; unsupported
rework refusal (T66/T80); MCP derived-state parity (F9); dispatch
pagination (F10); PR2 negative controls (14/14).

## 10. Gates and CI

Recorded in the PR body: `make build`, `mpm-lint --gate`,
`make test` (end-to-end), `make test-tools`, `make test-scripts`,
`make test-testenv`, `make docs-check`, race suites, PR2 controls,
real backup/restore integration, and the three CI jobs on the final
head. Environmental skips are named, not counted as passes.

## 11. Remaining limitations

- T49/T50 execute on a capable host; this host cannot now create the
  required user namespace (specific policy above), so they SKIP here.
  The prior successful execution at the identical containment code is
  recorded, and the assertions are unchanged.
- Pre-feature PR1/PR3 classes remain exercised with genuine `.dump` text
  whose schema markers are reduced to the pre-feature set; no historical
  binary exists in CI.

## 12. Frozen pilot and production preservation

- Frozen pilot `/home/v/projects/mpm-continuity-001-pilot`: untouched
  (`src`/`review`/`source-repo` clean at `0598edb7`; `rehearsal-src`
  retains its 9 pre-existing entries), dispatcher `enabled: false`.
- `/home/v/src/mpm`: not modified (20 pre-existing entries).
- No live migration, real supersession/cancellation, inference or
  provider request, dispatcher enablement, installation, deployment,
  scheduler/updater change. All databases disposable.
