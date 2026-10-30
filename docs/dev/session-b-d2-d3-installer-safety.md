# Session B — D2 and D3: the installer's layout contract

**Branch:** `fix/installer-prefix-rollback-safety` (from `origin/main` = `4f81690f`)
**Status:** D2 and D3 **CLOSED** by a narrow correction — an unsupported
prefix/data-root combination is refused before the installer touches the
filesystem, and the layout rollback now fails closed.
**Scope:** `scripts/lib/runtime_bundle.sh`, `install.sh`, and the two test files
that cover them. No schema change, no migration, no service change, no
unrelated installer redesign. Session A's D8 (the lesson trust boundary) is
untouched and was not overlapped.

---

## 1. Summary

Two defects in the versioned runtime-bundle layout, both found in the
30 October upgrade rehearsal, both in the same pair of functions:

| | Defect | Shape |
|---|---|---|
| **D2** | `rb_convert_layout` creates a dangling `current/$name` symlink when `--prefix` and `--data-root` differ, and **reports success** | fail-open, silent |
| **D3** | `rb_revert_layout` reads the rollback record as if it could not be false, and can report a successful recovery when **nothing was restored** | fail-open, silent |

Both were reproduced before anything was changed. The reproductions and the
post-fix behaviour are in §3 and §5; the non-vacuity proof is in §6.

**D2's correction is a refusal, and the refusal is the contract.** The
versioned layout has **one root**: the stable paths are written with the
*relative* target `current/<name>`, so they resolve inside the install prefix,
while the active pointer lives at `<data root>/current`. The two agree only
when the prefix and the data root are the same directory. An unsupported
combination is refused **before any filesystem mutation**, so it costs the
operator nothing.

**D3's correction is to read the record as a statement that can be false.** A
truncated record is read whole, a malformed entry is not acted on at all, a
missing record cannot hide conversion material still sitting in the prefix, and
a rollback that does not complete leaves the record in place naming what is
still unrestored. The function returns 0 only when it can *prove* the layout
is back to its pre-conversion shape.

---

## 2. D2 — the intended path semantics

### 2.1 What the code does

`rb_convert_layout <data_root> <prefix> <current_target>` writes, for each of
`bin` and `agent_installation`:

```sh
ln -s "current/$name" "$prefix/$name"
```

The target is **relative**, so the symlink resolves against the directory that
holds it — the install prefix:

```
$prefix/bin  ->  current/bin      resolves to  $prefix/current/bin
```

The active pointer is at `<data root>/current` (`rb_current_link`). So:

* `prefix == data_root` → `$prefix/current` **is** the pointer → the symlink
  resolves into the active bundle. ✔
* `prefix != data_root` → `$prefix/current` **does not exist** → the symlink
  dangles. It names no binary and no host asset.

### 2.2 The reproduction

```
$ rb_convert_layout "$DATA" "$PREFIX" "releases/abc1234"; echo "rc=$?"
  $PREFIX/bin -> current/bin (the previous directory is preserved at .bin.legacy.712225)
  $PREFIX/agent_installation -> current/agent_installation
rc=0
--- $PREFIX/bin is:
    lrwxrwxrwx 1 v v 11 ... /tmp/.../prefix/bin -> current/bin
--- does it resolve?
    DANGLING (no such file)
--- $PREFIX/current exists?
    NO
```

And the ordering makes it worse than a dangling symlink: the pointer has
**already been swapped** by the time the conversion runs, so the installation
is left pointing at a revision whose stable paths resolve to nothing — and the
installer says the promotion succeeded.

### 2.3 The intended semantics — one root, and why

The layout is documented, rehearsed and designed as **one root**:

| Evidence | What it says |
|---|---|
| `docs/INSTALL.md` §1g | one tree: `~/.mpm/` holds `current`, `releases/`, `bin/`, `agent_installation/` **and** the state |
| `scripts/rehearsal/mpm-upgrade` (the model for the bundle phase) | `<workspace>/` is the data root; `bin -> current/bin` is a sibling of `current` inside it |
| `docs/dev/runtime-migration-plan.md` §4.3 | *"the data root is never touched by the conversion"* — the conversion is a question about paths, all inside one root |
| `install.sh:resolve_paths` | *"PREFIX and DATA_ROOT both default to `$HOME/.mpm` … Single canonical install location."* |

The conversion's own relative target is written for exactly that. A **split**
prefix/data-root is legitimate in the **default** (non-bundle) layout — the
binaries go to the prefix, the state to the data root — and that path is
**unchanged** by this correction. It is only the *bundle* layout that has one
root, because only the bundle layout has a `current` pointer that the stable
paths are written relative to.

### 2.4 Why refusal, and not support for distinct paths

Supporting two roots is a **layout redesign**, not a bug fix:

* The stable path would need a target that resolves across roots. The options
  are an absolute target (`<data root>/current/<name>`, which breaks when the
  data root moves) or a second pointer (`<prefix>/current` →
  `<data root>/current`), and **two pointers are two sources of truth for
  which revision is active**. A pointer that can disagree with the pointer is
  worse than the dangling symlink this correction removes.
* The `rb_convert_layout` idempotency branch hard-codes the relative target,
  so a second layout variant would have to be parameterised through the
  conversion, the revert, and the rollback record.
* The boundary for this work is *"no unrelated installer redesign"*, and a
  second supported layout variant is exactly that.

So the correction is a **refusal, before any filesystem mutation**, with the
reason stated in the diagnostic. The alternative — silently writing a dangling
symlink and reporting success — is what the defect is.

### 2.5 Where the refusal happens

| Site | Role |
|---|---|
| `rb_layout_paths_compatible` (library) | the decision, and the diagnostic. Compares the **canonical** forms of the two paths, so a trailing slash or a symlinked home does not read as a difference, and a path that does not exist yet is not an error |
| `rb_convert_layout` (library) | calls it **first**, before the loop that touches anything. The library is safe on its own |
| `require_bundle_layout_paths` (installer) | one helper, used by `preflight()` and by the top of `phase_bundle()`. `preflight()` runs before the build, so nothing is built, staged or promoted; `phase_bundle()` refuses before it creates the lock file, the staging area or the release |

The refusal exits **1** (*preflight failed*), and the message says what to do
instead: name the same directory, or drop `--bundle`.

---

## 3. D3 — the rollback record read as if it could not be false

### 3.1 The record

`rb_convert_layout` appends the path of each directory it moves aside to
`<data root>/.legacy-layout-paths`. `rb_revert_layout` reads that file to
restore them. The record is the **only** statement of what was moved aside.

### 3.2 The four ways the old rollback lied

**(a) A truncated record was read as a shorter list.** `read` returns non-zero
at EOF even when it has read a final line that has no trailing newline, and
`while IFS= read -r legacy` drops that line:

```
$ printf 'a\nb' > rec.txt
$ while IFS= read -r l; do echo "got:[$l]"; done < rec.txt
got:[a]                      ← "b" is gone
```

The dropped entry is the operator's directory. The function then returned 0.

**(b) A malformed entry was acted on — and acting on it MOVED an arbitrary
path into the install prefix.** The old code derived the name with
`basename | sed` and moved whatever the record named:

```
$ printf '%s\n' "$T/victim" > "$DR/.legacy-layout-paths"
$ rb_revert_layout "$DR" "$PR"; echo "rc=$?"
  victim restored to its pre-upgrade directory
rc=0
--- did the unrelated path get MOVED into the prefix?
    drwxr-xr-x 2 v v 60 ... victim     ← yes
```

A corrupted record was a destructive one.

**(c) A missing record hid conversion material.** The conversion can be
interrupted between moving a directory aside and recording it. The old
rollback returned 0 with the material still sitting in the prefix:

```
$ rb_revert_layout "$DR" "$PR"; echo "rc=$?"     # no record; .bin.legacy.999 present
rc=0                                                ← nothing said, nothing restored
```

**(d) A failed rollback emptied the record the installer points the operator
at.** The old code truncated the record unconditionally, so after a failure —
the one case where the operator needs it — the file was zero bytes:

```
$ rb_revert_layout "$DR" "$PR"; echo "rc=$?"
ERROR: the preserved directory ... is missing — it cannot be restored
rc=1
$ wc -c < "$DR/.legacy-layout-paths"
0                                     ← the evidence was gone
```

`install.sh:981` tells the operator *"the layout conversion could not be fully
undone — see `$DATA_ROOT/.legacy-layout-paths`"*. That file was empty.

### 3.3 What the rollback does now

`rb_revert_layout` returns 0 **only when it can prove the layout is back to
its pre-conversion shape**:

| The record | Before | Now |
|---|---|---|
| valid | restored; record consumed | **unchanged** |
| truncated (no trailing newline) | last entry **silently dropped**; rc=0 | the entry is read and restored; rc=0 |
| malformed entry | the named path was **moved into the prefix**; rc=0 | refused, **not acted on**, the entry named; rc=1 |
| an entry outside the prefix | the path was moved | refused; rc=1 |
| missing, with material still in the prefix | rc=0, nothing said | rc=1, the stranded directory named **with the command to restore it** |
| missing, with nothing to restore | rc=0 | **unchanged** — the fresh-installation no-op |
| a restore that failed | rc=1, and the record was **emptied** | rc=1, and the record **names what is still pending** |

Three implementation details carry the weight:

* **The read handles a final line without a newline** — `while IFS= read -r
  legacy || [ -n "$legacy" ]`. The `||` clause is load-bearing: without it a
  truncated record is a shorter list.
* **Every entry is validated before it is acted on.** An entry must name
  `<prefix>/.<name>.legacy.<pid>`, with `<name>` one of the convertible names
  and the suffix digits, and its parent must be the prefix (compared
  canonically). Anything else is malformed: reported, not acted on.
* **The prefix is checked directly.** A preserved directory still sitting in
  the prefix means the conversion is not fully undone, whatever the record
  says. This is what makes a *missing* record fail closed.

The restore itself now uses `mv -T` (with the library's existing atomic-rename
fallback). Without `-T`, `mv src dst` where `dst` is an existing directory
moves `src` **into** it — the operator's directory would end up one level
deeper, with the rollback reported as a success.

### 3.4 What the rollback deliberately does NOT do

It does not **restore** an unrecorded preserved directory automatically. The
record is the authority on what must be restored; without it, the function
cannot know whether the directory it found is the whole of what was moved
aside. It reports the directory and the exact command instead:

```
ERROR: the preserved directory /prefix/.bin.legacy.999 is still in the install prefix — the layout conversion
ERROR: is NOT fully undone. It is not named by the rollback record /data/.legacy-layout-paths, or its restoration failed.
ERROR:   restore it by hand:  rm -f /prefix/bin && mv -T /prefix/.bin.legacy.999 /prefix/bin
```

That is the honest report: the rollback did not restore it, and here is where
it is.

---

## 4. What changed

| File | Change |
|---|---|
| `scripts/lib/runtime_bundle.sh` | new `rb_canon_path`; new `rb_layout_paths_compatible`; new `rb_legacy_path_name`; `rb_convert_layout` refuses an incompatible path pair before it touches anything; `rb_revert_layout` rewritten to fail closed |
| `install.sh` | new `require_bundle_layout_paths`, called from `preflight()` and from the top of `phase_bundle()` |
| `scripts/tests/test_runtime_bundle.py` | 22 new tests: `TestLayoutPathSemantics` (9) and `TestRollbackValidation` (13). The fixture now redirects `HOME` into the disposable root |
| `scripts/tests/test_install_bundle_paths.py` | **new** — 5 installer-level tests: the refusal, and the guarantee that nothing is created |
| `docs/INSTALL.md` | the one-root rule, and what the rollback does with a record it cannot trust |

**Preserved on purpose:** the default (non-bundle) install path still accepts a
split prefix and data root. The valid-record rollback still restores everything
and still consumes the record. The missing-record-with-nothing-to-restore case
still succeeds. A second revert of an already-reverted layout is still a no-op.

---

## 5. Regression coverage

### 5.1 `TestLayoutPathSemantics` (9) — D2

| Test | Pins |
|---|---|
| `test_the_default_layout_converts_and_the_symlinks_resolve` | the positive case — and the assertion that matters is that the symlink **resolves** (`is_symlink()` is satisfied by a dangling link; `(bin/"payload.txt").is_file()` is not) |
| `test_a_distinct_prefix_is_refused` | the refusal, and that the diagnostic names both paths |
| `test_a_refused_conversion_touches_nothing` | **before any filesystem mutation** — the operator's directory is still there, nothing was moved aside, no record was written, no symlink was created |
| `test_an_empty_prefix_is_refused` | a missing path |
| `test_an_empty_data_root_is_refused` | a missing path |
| `test_two_paths_that_name_the_same_directory_are_compatible` | the rule is about the **directory**: a trailing slash and a symlinked home both name one |
| `test_two_paths_that_name_different_directories_are_incompatible` | the rule |
| `test_a_path_that_does_not_exist_yet_is_accepted_when_both_name_it` | a fresh root that does not exist yet is not an error |
| `test_a_missing_path_that_names_a_different_directory_is_refused` | a missing path that names a different directory is still refused |

### 5.2 `TestRollbackValidation` (13) — D3

| Test | Pins |
|---|---|
| `test_a_valid_record_restores_every_directory` | the behaviour that already worked |
| `test_a_successful_revert_consumes_the_record` | the record is consumed; a second revert is a no-op |
| `test_an_empty_record_is_a_no_op` | an empty record |
| `test_a_truncated_record_still_restores_its_last_entry` | **the truncation regression** — the last entry is restored, not dropped |
| `test_a_malformed_record_is_refused_and_nothing_is_moved` | **the destructive regression** — the unrelated path is not moved into the prefix |
| `test_a_malformed_record_keeps_its_evidence` | a failed revert leaves the operator their evidence |
| `test_a_record_naming_a_path_outside_the_prefix_is_refused` | the shape is right but the location is wrong |
| `test_a_missing_record_with_nothing_to_restore_is_a_no_op` | the fresh-installation case still succeeds |
| `test_a_partially_completed_conversion_is_not_a_successful_no_op` | **the partially-completed conversion** — the stranded directory is named, with the command to restore it |
| `test_an_unrecorded_directory_is_named_in_the_record` | the record the installer points at names what is pending |
| `test_a_failed_revert_preserves_the_record` | **the evidence regression** — the record is not emptied |
| `test_a_failed_revert_does_not_nest_the_directory` | **the `mv -T` regression** — the operator's directory is not overwritten and the preserved one is not nested inside it |
| `test_a_conversion_and_its_revert_round_trip` | the whole cycle |

### 5.3 `test_install_bundle_paths.py` (5) — the installer surface

| Test | Pins |
|---|---|
| `test_the_canonical_layout_is_accepted` | the guard accepts the canonical layout |
| `test_a_split_prefix_is_refused` | the guard refuses, and the installer **exits** rather than carrying on |
| `test_the_default_install_path_still_accepts_a_split_prefix` | the **default** layout's split is untouched |
| `test_the_bundle_phase_refuses_before_it_creates_anything` | **the guarantee** — no `releases/`, no `current`, no `staging/`, no lock file, no record; the operator's directory untouched |
| `test_a_refused_bundle_phase_leaves_the_split_prefix_untouched` | nothing is written into the prefix either |

All of it runs in a disposable `mktemp` root with `HOME` redirected into it.

---

## 6. Non-vacuity — the tests fail against the pre-fix code

The new tests were run against the **original** `scripts/lib/runtime_bundle.sh`
and the **original** `install.sh`, restored from `origin/main` in the same
worktree and then put back:

```
$ git checkout origin/main -- scripts/lib/runtime_bundle.sh
$ python3 -m pytest scripts/tests/test_runtime_bundle.py -q \
      -k "PathSemantics or RollbackValidation"
16 failed, 6 passed, 24 deselected
```

The **16 failures** are the regressions. The **6 passes** are the
preserve-existing-behaviour cases — the default conversion, the valid record,
the empty record, the missing-record no-op, the second revert, and the round
trip — which is exactly the set that was supposed to pass before and after.

```
$ git checkout origin/main -- install.sh
$ python3 -m pytest scripts/tests/test_install_bundle_paths.py -q
4 failed, 1 passed
```

---

## 7. Gates

| Gate | Result |
|---|---|
| `make test-scripts` | **PASS** — see §7.1 |
| `mpm-lint --gate` | **PASS** (exit 0) — mutex, SQL, fd and import thresholds all within their bounds |
| `make docs-check` | **PASS** — 13 checks, 0 review items, 0 errors |
| `shellcheck -S warning` on the two changed shell files | no new findings (the 6 reported are pre-existing, and none are in the changed regions) |
| `bash -n` on both changed shell files | clean |
| `make test-race` / `make test` | **not run** — no Go file was touched |

### 7.1 `make test-scripts`

709 tests collected (floor 50), `OK (skipped=32)`. The floor is deliberately
low so ordinary test removal does not trip it.

`scripts/tests/test_runtime_bundle.py` grew from 24 tests to 46, and the new
`scripts/tests/test_install_bundle_paths.py` adds 5.

---

## 8. Does this change Session A's upgrade runbook?

**No. No assumption in the runbook changes, and no step of it needs to change.**

The runbook is the operator's checklist in
`docs/dev/runtime-migration-plan.md` §9, and the release record is
`docs/dev/release-candidate-2026-10-30.md`. Assessed point by point:

| The runbook says | Assessment |
|---|---|
| The install is at `~/.mpm`, one root | **Unchanged.** The runbook never uses a split prefix/data-root. D2's refusal is a no-op for it |
| *"The conversion moves the previous `bin/` and `agent_installation/` **aside** rather than deleting them; their locations are listed in `~/.mpm/.legacy-layout-paths`, and they are the rollback material for the conversion itself."* (§9.2) | **Strengthened, not changed.** That sentence relied on the record being readable and truthful. It now is: the record is read whole, a malformed entry is refused rather than acted on, and a failed rollback leaves the record in place instead of emptying it. The runbook's reliance on the record is now sound where it was previously load-bearing on a file that could be silently truncated |
| The runtime rollback restores *"which files are where"* and not the database (§9.4) | **Unchanged.** D3 is entirely about which files are where |
| The rollback is performed by re-pointing `current` (`ln -sfn` + `mv -T`) | **Unchanged.** That is the pointer rollback, and it is unaffected. D3 is the *layout* rollback, which runs inside the installer's own failed-promotion path |
| The checklist has a *"boundaries were respected"* item | **Unchanged.** No live installation, service, database or Git metadata was touched by this work |

**One thing the runbook now gets for free.** A failed promotion inside
`install.sh --bundle` used to be able to print *"the layout conversion could
not be fully undone — see `~/.mpm/.legacy-layout-paths`"* while that file was
empty. The operator was pointed at evidence that had just been destroyed. The
record now names what is still unrestored, so the pointer is finally worth
following.

**One thing a future operator should know, which is not in the runbook because
the runbook does not do it:** `--bundle` with `--prefix` and `--data-root`
naming different directories is now refused before anything is created. The
canonical install (`~/.mpm` for both) is unaffected.

---

## 9. Does this invalidate the rehearsal evidence?

**No. The rehearsal exercised a different implementation, and neither defect
was reachable from it.**

The rehearsal's driver is `scripts/rehearsal/mpm-upgrade` — a **prototype**,
explicitly described as *"the rehearsal artefact, not production code"* (its
own header). It does not call `rb_convert_layout` or `rb_revert_layout` at
all:

| Rehearsal mechanism | Why D2/D3 could not occur there |
|---|---|
| one root: `$WORKSPACE` holds the pointer, the releases **and** the stable paths (`scripts/rehearsal/mpm-upgrade:18-19`, `:494-495`) | there is no second root to diverge from, so the dangling-symlink case is structurally impossible |
| the rollback list is an **in-memory array** (`LEGACY_DIRS`, `:501`, `:539`), not a file | there is no on-disk record to truncate, corrupt or omit — the D3 failure modes have no path into it |
| the stable paths are checked to **resolve into the bundle** (`:570-577`) | the rehearsal's own assertion is stronger than the shipped installer's was, and it is unchanged |

The evidence the rehearsal supports therefore stands:

* `docs/dev/runtime-migration-evidence.md` §9's proof 2 — *"a failed initial
  conversion is recoverable at each intermediate step"* — is a property of
  `scripts/tests/test_migration_gate.py`, which drives the **shipped**
  `phase_bundle`. That suite still passes, in full, after this correction
  (§7).
* The rehearsal's claims about the **shape** of the layout — the pointer, the
  releases, the stable paths resolving into the active bundle — are properties
  of a single-root layout, and a single root is what the layout now *requires*
  rather than merely assumes.

**What the evidence did not cover, and now does.** The rehearsal and the
migration gate both exercised the **well-formed** recovery path. The
malformed, truncated and missing record cases were untested, which is how D3
survived to the rehearsal. They are covered now (§5.2), and the guarantee is
stated in `docs/INSTALL.md` §1g.

**One thing the rehearsal prototype shares with the shipped installer**, and
which this correction does **not** change, because the prototype is not
production code: `scripts/rehearsal/mpm-upgrade:543` restores with
`mv "$legacy" "$WORKSPACE/$name"` — no `-T` — and carries the same nesting
hazard the shipped `mv -T` fix removes. The prototype's `LEGACY_DIRS` array
holds only paths it created itself, so the hazard is not reachable there
either. It is recorded here rather than silently repaired, because the
rehearsal artefact is the *model* for `install.sh`'s bundle phase and a
divergence between them should be a decision, not an accident.

---

## 10. What was NOT done

* **No live installation change.** No `install.sh` run against `~/.mpm`, no
  service restart, no `~/.mpm` modification. Every test used a disposable
  `mktemp` root with `HOME` redirected into it.
* **No database schema change.** No Go file was touched at all.
* **No unrelated installer redesign.** The default install path, the
  non-bundle phases, the uninstall path, the service handling and the build
  are all untouched. The correction is one guard function and one rewritten
  rollback.
* **No change to the migration gate's seven proofs**, and no change to the
  rehearsal harness. The installer-level proof lives in a new file beside the
  gate precisely so that a historical evidence record's count is not rewritten.
* **No change to the rehearsal driver.** The `mv -T` divergence is reported
  (§9), not repaired.
* **Session A's D8 work was not touched or overlapped.**
* **No merge.** The PR is opened for review.

## 11. Residual limitations — reported, not repaired

* **A stale record from an earlier successful conversion is still acted on.**
  A promotion that succeeds leaves its preserved directories in the prefix and
  its record in place. A *later* promotion's rollback reads that record and
  restores those directories — undoing the earlier conversion. This is
  pre-existing behaviour, it is reachable only when a promotion fails after the
  pointer is swapped, and it is a different defect from D3 (the record is
  valid; it is stale). It is reported here rather than folded into a narrow
  fix.
* **The rollback does not restore an unrecorded preserved directory
  automatically** (§3.4). It reports it and prints the command.
* **The rehearsal prototype's `mv` without `-T`** (§9).
