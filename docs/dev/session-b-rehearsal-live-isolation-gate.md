# Session B — the rehearsal's live-installation isolation gate

**Branch:** `fix/rehearsal-live-isolation-gate` (from `origin/main` = `4f81690f`)
**Status:** the false positive is **CLOSED**. The invalid file-snapshot
assertion is replaced by a classified gate that separates the live
system's ordinary writes from unauthorised changes, and the gate's own
behaviour — including the negative control — is a gated test.
**Scope:** the rehearsal harness and its test. No live installation
change, no scheduler stop, no deployment, no database modification, no
unrelated work.

**Reported by:** Session A (integration and release authority), from the
30 October upgrade rehearsal.

---

## 1. Summary

The rehearsal asserted that the live installation was untouched by
hashing `(path, size, mtime)` over a list of live paths, **including the
live database file**:

```sh
live_snapshot_paths() {
  printf '%s\n' \
    "$LIVE_MPM/bin" \
    "$LIVE_MPM/src/db/mpm.db" \        # ← the live database
    "$LIVE_MPM/agent_installation" \
    "$LIVE_SYSTEMD_USER/mpm-scheduler.service" \
    "$LIVE_OC_CONFIG/opencode.jsonc" \
    "$LIVE_OC_CONFIG/AGENTS.md"
}
```

That assertion was **invalid**. The live scheduler records health checks
and SQLite checkpoints the WAL, so the live database's size and mtime
move on their own, and the suite reported *"the live tree's snapshot is
unchanged"* **FAILED** for a reason that had nothing to do with the
rehearsal.

The correction replaces it with a **classified** gate: the protected
artifacts are compared **by content**; the live database is compared **by
class**, because the live system legitimately writes to it. The
scheduler's own writes are **reported as evidence**; a change to the
operator's knowledge is a **failure**.

---

## 2. The measurement — the defect is real, and it is a false positive

### 2.1 The live database moves on its own

With the production scheduler **active** and nothing else running:

```
$ for i in 1 2 3; do printf "  t=%s size=%s mtime=%s\n" "$i" \
      "$(stat -c%s ~/.mpm/src/db/mpm.db)" "$(stat -c%Y ~/.mpm/src/db/mpm.db)"; sleep 20; done
  t=1 size=22454272 mtime=1793370994
  t=2 size=22454272 mtime=1793370994
  t=3 size=22474752 mtime=1793371085        ← grew by 20 KB
```

Over four minutes, with the operator's knowledge unchanged:

```
t=0    content-fingerprint=5882dd04c19bfbe9c83354d752141a3838332cfb51caec3f18a5cfe948e02848  size=22536192
t=60   content-fingerprint=5882dd04c19bfbe9c83354d752141a3838332cfb51caec3f18a5cfe948e02848  size=22556672
t=240  content-fingerprint=5882dd04c19bfbe9c83354d752141a3838332cfb51caec3f18a5cfe948e02848  size=22622208
```

The file grew by **86 KB**. The domain content fingerprint **never
changed by one bit**.

### 2.2 The old assertion fails on that

The old rule, taken twice ninety seconds apart with the scheduler live:

```
  snapshot A: 604bfed791f3f50839e040381d402365415b4a273f0e11416b3d8ff08f4d3790
  snapshot B: 3bb4af474ac9da6bcba1466f08bed3538486e1f4bef306cd9f2b9b4c4e07664d
  → CHANGED  (FAIL)
```

The rehearsal had touched nothing. **That is the false positive.**

### 2.3 What the live scheduler actually moves

Per-table content hashes over a 120-second window with the scheduler
live — **only two tables moved**:

| Table | Before → after | What it is |
|---|---|---|
| `tool_invocations` | 22707 → 22762 rows (+55) | the audit trail: the live plugin's polling and the scheduler's health checks |
| `system_config` | 1 row, content changed | the scheduler's liveness counter (`scheduler_active_uptime`) |

Every other table — `memories` (143), `lessons_base` (36), `evidence`
(101), `memory_revisions` (154), `retrieval_metadata` (86),
`memory_settling_baselines` (142), `schema_migrations` (18) — was
byte-identical.

**The classification is measured, not guessed.**

---

## 3. The correction

### 3.1 The gate

`scripts/rehearsal/live_gate.py` takes a **classified** snapshot before
and after, and compares the two.

| Class | What it is | The rule |
|---|---|---|
| **PROTECTED** | the installed binaries (`bin/**`), the host assets (`agent_installation/**`), the systemd unit, `opencode.jsonc`, `AGENTS.md` | **byte identity** — the path set, the mode, and the sha256. A changed path, a changed mode or a changed hash is a **failure**, named |
| **DOMAIN** | the operator's knowledge inside the live database: `memories`, `lessons`, `evidence`, with the volatile bookkeeping columns excluded | **must not change**. This is the substrate's own established definition (`state_fingerprint.py --content`), so the live database is measured by exactly the same instrument as the simulated one. A change is a **failure** |
| **OPERATIONAL** | `tool_invocations`, `system_config` — the two tables the live scheduler actually moves | **may change**, and the movement is **reported with the delta** |

Everything else the live database does is **reported as evidence, not
asserted**: the rehearsal's own process tree is separately proven not to
have opened the live database at all (the syscall detector), and the live
binary may legitimately migrate the live database while the rehearsal
runs.

**Why the database FILE is not byte-compared.** Its bytes move for
reasons that have nothing to do with the rehearsal. Its *domain state*
does not. The file's size and mtime are **reported** — with the reason —
and never asserted.

**Why the protected artifacts' mtime is not compared.** An mtime records
*when* a file was written, not *what it contains*. Comparing content is
both stricter where it matters and immune to the noise. It is also
strictly stronger than the old rule: a content change that preserved the
size and put the mtime back was **invisible** to the old assertion and is
caught by this one (§6.2).

### 3.2 The gate fails closed

If the live database cannot be read, the gate says it **could not
measure** and exits 2. A gate that cannot measure must not report
success. The suites fail on a snapshot that cannot be taken, rather than
proceeding to a vacuous assertion.

### 3.3 The evidence, as it appears in the rehearsal

```
THE LIVE INSTALLATION WAS NEVER TOUCHED
  LIVE-GATE: the live installation's isolation
    protected artifacts checked : 436
    protected artifacts changed : 0
    domain tables checked       : 3
    domain tables changed       : 0
    EXPECTED operational movement (the live system's own bookkeeping):
      - tool_invocations: rows 22984 -> 22989 (+5)  [the audit trail — the live plugin's polling and the scheduler's health checks]
  PASS the live installation was not modified by the migration (gate rc=0)
  PASS no syscall in the migration touched the live tree (0 hits)
```

That report is the **explicit evidence** the assignment asks for: the
movement in the live database is named, attributed, and separated from
the domain state, which did not change.

---

## 4. Where the change lands

| File | Change |
|---|---|
| `scripts/rehearsal/live_gate.py` | **new** — the classified gate: `snapshot` and `compare` |
| `scripts/rehearsal/lib.sh` | `live_snapshot` (the invalid size/mtime rule) **removed**; `live_gate_snapshot` / `live_gate_compare` added |
| `scripts/rehearsal/10_obligations.sh` | the classified gate replaces *"the live tree's snapshot is unchanged"* |
| `scripts/rehearsal/20_negative.sh` | the same — and the BEFORE snapshot moved to the **start** of the suite, so the comparison spans the whole suite instead of two adjacent instants |
| `scripts/tests/test_rehearsal_live_gate.py` | **new** — 28 tests: the negative control, the non-vacuity proofs, the fail-closed cases, the "same instrument" proofs |
| `scripts/rehearsal/README.md` | the detectors section rewritten to describe the classification |

---

## 5. Preserved on purpose

* The rehearsal's **other two detectors** are untouched: the `strace` of
  the whole process tree, and the systemd path shim. The gate replaces
  the *file-state* detector; it does not weaken the causal one.
* **Every other assertion** in the obligations, negative and schema
  suites is unchanged. The rehearsal still runs **135 assertions**.
* The rehearsal's structure, its work-directory rules, its
  destructive-command guard and its harness-integrity guard are
  unchanged.
* The live database is still opened **read-only**, through
  `file:…?mode=ro`.

---

## 6. The negative control and the non-vacuity

`scripts/tests/test_rehearsal_live_gate.py` drives the gate against a
**disposable stand-in** for the live tree, built fresh for every test.
Nothing in it resolves into the operator's real `~/.mpm`, and the
negative control introduces its unauthorised change **in the stand-in**,
so production is never touched.

### 6.1 The negative control — the gate must DETECT

| Test | Pins |
|---|---|
| `test_a_modified_configuration_file_is_detected` | the protected configuration |
| `test_a_modified_managed_instruction_is_detected` | the managed instructions |
| `test_a_modified_executable_is_detected` | the executable path |
| `test_a_new_executable_is_detected` | an executable that appears |
| `test_a_removed_executable_is_detected` | an executable that disappears |
| `test_a_modified_host_asset_is_detected` | the host assets |
| `test_a_modified_systemd_unit_is_detected` | the managed unit |
| `test_a_written_operator_memory_is_detected` | **the database case** — the test must not treat all database mutations as harmless |
| `test_a_modified_lesson_is_detected` | a modified lesson |
| `test_a_new_table_in_the_live_database_is_detected` | a migration applied to the live database — the incident the rehearsal's own README documents |

### 6.2 The non-vacuity — the old rule and the new one, measured side by side

**A/B.** The old rule is reproduced verbatim in the test and run against
the same tree the gate is run against. Ordinary scheduler activity — the
audit trail grows, the database file grows with it, its mtime moves — and
**nothing about the installation changes**:

```python
self.assertNotEqual(old_before, old_after,
    "the old rule must have seen a change here — otherwise this "
    "test proves nothing about the difference between the two")
self.assert_clean(before, after)
```

The old rule saw a change; the gate passed.

**The blind spot.** A content change that preserves the size and puts the
mtime back was **invisible** to the old rule. The gate compares
content, and catches it:

```python
p.write_text(original.replace("config", "EVIL!!"))   # same length
os.utime(p, (st.st_atime, st.st_mtime))              # same mtime
```

`test_the_old_size_and_mtime_assertion_would_have_missed_it` asserts
that size and mtime are identical — so the old rule would have reported
*"unchanged"* — and that the gate still detects it.

### 6.3 Ordinary activity must not fail the gate

`test_the_audit_trail_growing_is_not_a_failure`,
`test_the_scheduler_liveness_counter_being_rewritten_is_not_a_failure`,
`test_a_read_that_bumps_the_volatile_columns_is_not_a_failure` and
`test_the_database_file_growing_is_not_a_failure`.

### 6.4 The gate fails closed

`test_an_unreadable_database_is_a_measurement_failure` (exit 2),
`test_snapshots_from_different_gate_versions_are_not_comparable`
(exit 2), and `test_a_missing_live_installation_is_not_a_measurement_failure`
— nothing installed means nothing to protect, which is a clean
measurement, not a failure to measure.

**And a table the gate cannot READ is a measurement failure.** The gate
hashes every table in the live database; when the `SELECT` failed, it
returned "no fingerprint" and the table was **silently excluded** from
the comparison. A table the gate cannot read is a table the gate cannot
measure, and *"could not measure"* reported as *"nothing changed"* is the
exact failure shape this change exists to remove. `table_fingerprint`
now raises `MeasurementError` in that case, the gate exits 2, and the
mutation — replacing the exception with a silent skip — is pinned by
`test_a_table_that_exists_but_cannot_be_read_is_not_skipped`.

Measured on the live database at the time of writing: **91 tables and
views, 0 unreadable** — so this is a latent hole closed, not a live
failure.

### 6.5 And the SUITES fail closed

A gate that cannot measure is not enough: the **suite** must not swallow
that failure and proceed to a vacuous assertion. Two tests drive the
actual suite scripts against a disposable, deliberately unreadable
stand-in for the live tree:

* `test_the_obligations_suite_fails_when_the_gate_cannot_measure`
* `test_the_negative_suite_fails_when_the_gate_cannot_measure`

Each asserts that the suite **reports the measurement failure in its
own summary** (`failed: 1`) and that it **never reached** the isolation
assertion.

**The non-vacuity of these two was demonstrated, not asserted.** The
guard was removed from both suites — the mutation that turns a
fail-closed suite into a vacuous one — and the two tests **failed**:

```
FAILED …::test_the_negative_suite_fails_when_the_gate_cannot_measure
FAILED …::test_the_obligations_suite_fails_when_the_gate_cannot_measure
2 failed, 22 deselected
```

The first draft of these tests asserted only that the suite exited
non-zero, and it **passed against the mutation** — because the suite
failed later for an unrelated reason (the simulation was not set up).
That is the same failure shape as the defect this change fixes: a
detector that reports success without having looked. The tests were
rewritten to assert on the suite's **own** failure message, which is
what distinguishes *"the suite reacted"* from *"the gate printed
something and the suite carried on regardless"*.

The end-to-end behaviour, measured directly:

```
$ LIVE_MPM=/tmp/opencode/broken-live/.mpm … bash scripts/rehearsal/10_obligations.sh
LIVE-GATE: could not measure: cannot open the live database …: disk I/O error
LIVE-GATE: a gate that cannot measure must not report success.
  FAIL the live-tree snapshot could not be taken — the isolation gate cannot measure
== obligations ==
  passed: 0
  failed: 1
rc=1
```

Both suites behave the same way.

### 6.6 The live database and the simulated one are measured alike

The rehearsal proves that a migration left the **simulated** state
unchanged with `state_fingerprint.py --content`. The gate proves that
the **live** state is unchanged. Those two claims are only comparable
if both are measured by the **same instrument** — so the gate's domain
definition must not drift away from the fingerprint's.

Three tests pin that, and the third **measures** it: the same table,
hashed by the fingerprint and by the gate, must produce the **same
digest**.

| Test | Pins |
|---|---|
| `test_the_volatile_column_sets_are_identical` | the gate excludes exactly the columns the fingerprint excludes |
| `test_the_domain_tables_are_the_fingerprint_s_content_tables` | the gate's domain tables are the fingerprint's content tables |
| `test_the_per_table_hash_is_byte_identical` | the same table, hashed by both, yields the same digest |

Measured on this host, at the moment of writing:

```
state_fingerprint hex: e168d85910cd84cadd053d14005a3e569da16dd0b6673488f9aa5178d0c43085
live_gate        hex: e168d85910cd84cadd053d14005a3e569da16dd0b6673488f9aa5178d0c43085
IDENTICAL: True
```

**The non-vacuity is demonstrated.** A volatile column added to
`state_fingerprint.py` made the first test fail, naming the column:

```
AssertionError: Items in the first set but not the second:
'a_new_volatile_column' : the gate must exclude exactly the columns the
substrate's own fingerprint excludes
```

If someone changes the fingerprint's definition and this test is not
there, the live database and the simulated database would be measured
by two different rulers — and *"unchanged"* would mean two different
things.

---

## 7. The rehearsal, run with the production scheduler live

```
$ systemctl --user is-active mpm-scheduler
active
$ REH_REV=HEAD REH_WORK=… bash scripts/rehearsal/run.sh
…
  assertions passed : 135
  assertions failed : 0
  suites failed     : 0
REHEARSAL PASSED — 135 assertions, 0 failures
```

**And repeated**, with the scheduler still active and nothing else
touching production — see §8 for the run table. Each run's gate report
shows the scheduler's own movement as evidence and passes.

The live installation was not modified: the protected artifacts are
byte-identical, the domain state is identical, the database's own file
moved only by the scheduler's doing, and the syscall detector reports
**0** hits on the live paths.

### 7.1 The other gates

| Gate | Result |
|---|---|
| `make test-scripts` | **OK** — **710 tests** green (682 baseline + 28 new) |
| `make docs-check` | **OK** — 13 checks, 0 review items, 0 errors |
| `mpm-lint --gate` | **PASS** |
| `shellcheck -S warning` on the three changed shell files | **53 findings**, against 56 before the change — three fewer (the removed helpers took their findings with them) and **none new** |

The gate's own suite, on its own: **28 passed**.

---

## 8. The repeated runs

| Run | Result | The gate's evidence |
|---|---|---|
| 1 (full, with the bundle build) | **PASS** — 135 assertions, 0 failures | `tool_invocations: rows 22984 -> 22989 (+5)` |
| 2 | **PASS** — 135 assertions, 0 failures | `+1` (obligations), then `+1` (negative) |
| 3 | **PASS** — 135 assertions, 0 failures | `+2` (obligations), then `+1` (negative) |
| 4 | **PASS** — 135 assertions, 0 failures | `+1` (obligations), then `+1` (negative) |
| 5 | **PASS** — 135 assertions, 0 failures | after the fail-closed and same-instrument additions |
| 6 | **PASS** — 135 assertions, 0 failures | after the unreadable-table fix |
| 7 | **PASS** — 135 assertions, 0 failures | no operational movement in the window |
| 8 | **PASS** — 135 assertions, 0 failures | no operational movement in the window |
| 9 | **PASS** — 135 assertions, 0 failures | no operational movement in the window |

**Nine consecutive green rehearsals**, with the production scheduler
**active** throughout and nothing else touching production. Runs 1–4
were against the first cut of the gate; runs 5–9 against the final one.

The live database's size across the three repeats of the first batch:

```
  before: 22695936 bytes
  after:  22716416 bytes        ← grew by 20 KB
```

That growth is the exact condition under which the old assertion failed.
Every run passed, and every run's gate report showed the movement as
**evidence** — `tool_invocations: rows 23003 -> 23004 (+1)` — with the
protected artifacts unchanged and the domain state unchanged.

Runs 7–9 show the other half of the behaviour: the scheduler did **not**
write inside those windows, so there was **no operational movement**,
and the gate still passed with the protected artifacts and the domain
state unchanged. The gate does not depend on the scheduler being busy
to be meaningful, and it does not fail when the scheduler is quiet.

**The live installation was not modified by any of the nine runs.** The
binary is still dated 24 October, `mpm-scheduler` is still `active` with
`MainPID=3017` and `NRestarts=0`, and the live database was only ever
opened read-only, through `file:…?mode=ro`.

---

## 9. What this changes for Session A

**A's record `docs/dev/release-candidate-2026-10-30.md` is not edited
here.** It is A's document, and its other findings stand.

**But one of its rows rests on the invalid method.** Row 10 of that
record's check table reads *"The live tree snapshot is unchanged | PASS"*,
and the record's "three independent detectors" section describes the
snapshot as *"a snapshot of the live binaries, the live database, the
live unit file and the live OpenCode config, before and after"*.

That check passed on the day it was run — the scheduler happened not to
write inside the window — but the **method** could not support the
claim: it would have failed on a tree the rehearsal never touched, and
it could not see a content change that preserved a file's size. The
correction replaces the method; it does not contradict the result.

**What A's record's other two detectors prove is unaffected.** The
`strace` (check 11, *0 hits*) and the systemd shim (check 12's shim
log) are independent of the file-state rule and remain exactly as
recorded.

**For the final combined-candidate verification, the corrected gate is
the file-state detector that must be in the candidate.** It is the
detector that fails when the production scheduler is busy, which is
precisely the condition under which the combined verification runs.

---

## 10. Residual limitations — reported, not repaired

* **A table outside the classified set that moves is REPORTED, not
  failed.** The gate prints *"a table OUTSIDE the expected operational
  set moved"* with the delta. The reason: the operational set was
  **measured** over a window, and a table the live system starts writing
  to later would otherwise make the rehearsal fail for a reason that has
  nothing to do with it. The trade is deliberate — a reported change is
  visible, a spurious failure is not.
* **A table that APPEARS in the live database is a FAILURE.** That is
  the rehearsal's own documented incident — a newer binary applying a
  migration to the live database — and it must be caught. The cost is
  that an operator who installs a new MPM version *while* a rehearsal
  runs will see this fail; the diagnostic names the table.
* **The domain fingerprint is the substrate's established definition**
  (`memories`, `lessons`, `evidence`, volatile columns excluded). A
  domain change outside those three tables is reported, not failed.
* **The strace detector is still skipped when `strace` is not
  installed**, and the suite then reports *"0 hits"* — a vacuous pass.
  That is pre-existing and is **not** changed here; it is recorded
  because it is the same failure shape as the one this change fixes: a
  detector that reports success without having looked.

---

## 11. What was NOT done

* No live installation change. The live `~/.mpm` was **read** (read-only,
  through `file:…?mode=ro`) and never written.
* **No scheduler stop.** The production scheduler ran throughout every
  rehearsal run in this record.
* No deployment. No service restart. No unit edited.
* No modification of the **live** database — it was only ever opened
  read-only, and the negative control's unauthorised change is made in a
  **disposable stand-in**.
* No change to the rehearsal's assertions other than the isolation
  check. The other two detectors, the work-directory rules, the
  destructive-command guard and the harness-integrity guard are
  unchanged.
* No change to `docs/dev/release-candidate-2026-10-30.md` or to any
  other session's record.
* No merge.

**The one write outside the repository** was to the shared **dogfooding**
workspace (`~/.mpm-dogfood`), which `docs/dev/coordination.md` §6.2
names as the reasoning channel — a memory, a lesson, a decision and a
handoff, carrying the measurement, the design and the reasoning. That
workspace is the shared one, not the live installation, and the writes
were made through the canonical `MPM_WORKSPACE` / `MPM_DB_PATH` pair so
that they could not land in the live database. Verified afterwards: the
live `tool_invocations` carries **0** rows matching this work.
