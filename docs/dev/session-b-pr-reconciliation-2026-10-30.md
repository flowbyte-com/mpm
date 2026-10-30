# Session B — PR reconciliation and main-branch readiness

**Date:** 30 October 2026
**Authority:** none. Session A remains the sole merge and deployment
authority (`docs/dev/coordination.md` §1). **Nothing was merged.**
**Scope:** #187–#190 plus two PRs this session opened. No live
installation change, no scheduler restart, no deployment, no production
database write.

---

## 1. A's final candidate revision — determined

The assignment named `ba13a2d5` as "previously verified" and warned
that A may have advanced it. **A did.** The final candidate is:

```
3d63244e1e64955db2db243e70e279d370ebf1d6
docs(agent): the managed-instruction contract moves to 5.0.0
```

Determined from the live installation, read-only, four independent ways:

| Evidence | Value |
|---|---|
| `~/.mpm/current` → | `releases/3d63244e1e64955db2db243e70e279d370ebf1d6` |
| `~/.mpm/current/RELEASE.json` → `revision` | `3d63244e1e64955db2db243e70e279d370ebf1d6` |
| the five installed binaries' build identity | `MPM 0.1.0-dev+g3d63244e.t1793377957` |
| the installed `~/.config/opencode/AGENTS.md` provenance stamp | `commit=3d63244e1e64955db2db243e70e279d370ebf1d6 protocol=5.0.0` |

**A has completed the live cutover.** The installation is now the
versioned layout:

```
~/.mpm/current              -> releases/3d63244e…      (the active pointer)
~/.mpm/bin                  -> current/bin
~/.mpm/agent_installation   -> current/agent_installation
~/.mpm/releases/3d63244e…/{bin,schema,config,agent_installation,RELEASE.json}
~/.mpm/.legacy-layout-paths  -> the moved-aside rollback material
```

The five binaries are inside the release bundle, and the bundle's
`RELEASE.json` names the revision.

---

## 2. The commit-by-commit mapping

Every commit on `integration/release-candidate-2026-10-30`, and which
individual PR head contains it:

| Integration commit | In PR | Subject |
|---|---|---|
| `e7116496` | #187 | the LLM request timeout is an advanced setting |
| `39be194a` | #187 | a credential reference is a reference |
| `5dcd60e6` | #188 | the bundle layout has one root, and its rollback fails closed |
| `6463b58a` | #188 | docs(install): the one-root rule |
| `98a5c6b4`, `4a362b68`, `9ff6d5b1`, `4fe322c2` | #188 | the D2/D3 report |
| `c8679dea` | #189 | an agent cannot write a TRUSTED lesson; it proposes one |
| `e8d4e8a3` | #189 | the compaction path must not launder agent text |
| `bed4254a` | #189 | the managed snippet states that compact produces proposals |
| `abfcccde` | #189 | the review queue renders a compaction proposal readably |
| `22f9681d` … `5ef9ad47` | #190 | the classified gate (first cut) |
| `c58a3aa8` | — | `merge(#188)` — a merge commit, no unique content |
| `769659fb` | — | `merge(#187)` — a merge commit, no unique content |
| `b96d6c1d` | — | `merge(#189)` — a merge commit, no unique content |
| `ba13a2d5` | — | `merge(#190)` — a merge commit, no unique content |
| **`70b00a1d`** | **NOT IN ANY PR** | **the plugin's compact description states the proposal contract** |
| **`3d63244e`** | **NOT IN ANY PR** | **the managed-instruction contract moves to 5.0.0** |

**Two corrections exist only on the integration branch.** Merging the
four PRs individually would have lost both.

### 2.1 The two integration-only corrections → PR #193

Both are already **in the deployed runtime**, and both affect it:

| Correction | Effect | Verified in the live installation |
|---|---|---|
| `70b00a1d` | the **plugin payload**'s `mpm_system` description said *"drain eligible raw memories into lessons"* and advertised `lessons_created`/`lesson_ids`, while the combined candidate's drain records a **pending proposal** — the payload contradicted the substrate it talks to | `grep -c "the drain records its synthesis as a PENDING proposal" ~/.mpm/current/agent_installation/mpm-opencode/dist/index.v2.js` → `1` |
| `3d63244e` | the managed-instruction **protocol marker** stayed at `4.0.0` through #189's action rename, which the repository's own bump policy names a MAJOR bump | `~/.config/opencode/AGENTS.md` carries `protocol=5.0.0`; a tree without the commit declares `4.0.0` and would stamp `4.0.0` on the next install |

PR #193 carries them as `git cherry-pick 70b00a1d 3d63244e` — A's
commits, A's authorship, A's messages, **not re-authored and not
duplicated**. The four resulting files are byte-identical to A's
integration branch.

---

## 3. A defect in the COMBINATION, in no PR and on no branch

**The rehearsal was red on the candidate that was deployed.**

`scripts/rehearsal/00_simulate.sh` seeded the synthetic corpus with:

```sh
"$SIM_WS/bin/mpm" call mpm_lessons --payload '{"action":"save",...}'
```

**#189 made `mpm_lessons action=save` operator-only**, and the refusal
was invisible because every seeding call redirects its output to
`/dev/null`. The rehearsal noticed one suite later, as
`FAIL the synthetic lesson survived (0 rows)` — a symptom, not a cause.

**So the migration's preservation of the lesson surface was not being
exercised at all** on the deployed candidate, and the rehearsal's one
failing assertion did not say why.

Reproduced on A's integration branch:

```
$ git show origin/integration/release-candidate-2026-10-30:scripts/rehearsal/00_simulate.sh | grep mpm_lessons
141:"$SIM_WS/bin/mpm" call mpm_lessons --payload '{"action":"save",…}' >/dev/null 2>&1
```

and confirmed against the deployed binary, in a disposable workspace:

```
$ mpm call mpm_lessons --payload '{"action":"save",…}'
{"error":"mpm_lessons action=save is operator-only: … Nothing was written. …
 An operator can write a trusted lesson directly from a terminal with:
 mpm lesson add <content>","success":false}

$ mpm lesson add "…" --type warning --tags rehearsal
Lesson added with ID: 442c49d183680802 (reinforcement: 1)
```

**This is a gap in the combination** — exactly what per-PR review cannot
see. It is not a conflict and not a rewrite: a whole-tree search found
**one** executable caller of the refused action in the entire tree, and
it is this line.

**PR #192 fixes it**: the operator path (`mpm lesson add`, which creates
a *trusted* lesson — the thing the migration's preservation claim is
about), and a seeding step that is **loud**: a refused seed now stops
the rehearsal instead of surfacing one suite later as an unrelated
symptom. Four regression tests pin it; the non-vacuity is demonstrated.

**After #192, the rehearsal passes against the live installation that
runs the new binary: 135 assertions, 0 failures.**

---

## 4. Source-tree equivalence — the answer to "does merging drift?"

**Not "do the commit messages match" — do the TREES match.**

Merging all six heads onto `origin/main` in A's order
(#188 → #187 → #189 → #193 → #190 → #192) and diffing the result against
A's deployed candidate `3d63244e` gives **eight differing files**, and
**every one of them is the rehearsal harness, its tests, or
documentation**:

```
docs/dev/coordination.md                             (my ledger row)
docs/dev/session-b-rehearsal-live-isolation-gate.md  (my record)
scripts/rehearsal/00_simulate.sh                     (the rehearsal)
scripts/rehearsal/README.md                          (the rehearsal)
scripts/rehearsal/lib.sh                             (the rehearsal)
scripts/rehearsal/live_gate.py                       (the rehearsal)
scripts/tests/test_rehearsal_live_gate.py            (a test)
scripts/tests/test_rehearsal_seeding.py              (a test)
```

By area — and this is the question that matters:

| Area | What it is | Files that differ |
|---|---|---|
| `cmd/` | the binaries | **0** |
| `internal/` | the binaries and the schema | **0** |
| `internal/core/schema.go` | the schema | **0** |
| `agent_installation/` | the plugin payload and the managed instructions | **0** |
| `install.sh`, `uninstall.sh` | the installer | **0** |
| `scripts/lib/`, `scripts/build-provenance.sh` | the installer's library and build provenance | **0** |
| `Makefile` | the build | **0** |
| `docs/SPEC.md` | the behavioural contract | **0** |
| `scripts/rehearsal/`, `scripts/tests/` | the rehearsal harness | 6 (additions and the gate) |

### 4.1 The strongest check — the compiled payload

The plugin payload is **built**, not committed (`dist/` is gitignored),
so the source difference is what ships. The reconciled tree's compiled
payload is **byte-identical** to the deployed one:

```
✔ BYTE-IDENTICAL  dist/index.v2.js
```

### 4.2 The managed instructions

The **behavioural text** is byte-identical between the reconciled tree
and the deployed canonical source (the block rendered from the
reconciled tree diffs clean against lines 87–135 of the deployed
`MPM_AGENT_INTEGRATION_SNIPPETS.md`), and
`render_managed_blocks.py --check` reports byte-for-byte parity.

The **protocol marker** differs only because the reconciled tree lacks
`3d63244e` — which is exactly what PR #193 restores. With #193 merged,
both the text and the marker match.

---

## 5. The merge sequence

**PR #193 is based on #189's branch**, because both corrections describe
#189's behaviour: the compaction drain produces proposals, and the
protocol marker versions that contract. Applying them before #189 would
be wrong, not merely out of order.

| # | PR | Base | Why here |
|---|---|---|---|
| 1 | **#188** installer rollback safety | `main` | A's first; the installer |
| 2 | **#187** onboarding timeout | `main` | A's second; independent of the rest |
| 3 | **#189** lesson trust boundary | `main` | A's third; the breaking action rename |
| 4 | **#193** A's two integration-only corrections | `fix/lesson-trust-boundary` | **must follow #189** — it describes #189's behaviour |
| 5 | **#190** the rehearsal's classified isolation gate | `main` | the rehearsal harness; touches no file the others touch |
| 6 | **#192** the rehearsal's lesson seeding | `main` | the rehearsal harness; touches no file the others touch |

**#190, #192 and #193 touch disjoint files** from #187–#189 and from each
other, so they merge in any order among themselves. The **only**
ordering constraint in the whole set is **#189 before #193**.

### 5.1 The validation plan

Run against the reconciled tree (the merge of all six heads), with the
production scheduler **active**:

| Gate | What it proves |
|---|---|
| `make build` | the binaries compile |
| `make test-scripts` | the installer contract and the script suite |
| `make test-race` | the canonical Go gate, with the race detector |
| `make docs-check` | the documentation is fresh |
| `python3 -m unittest discover tests` in `agent_installation/` | renderer parity (managed-block byte-for-byte) |
| `mpm-lint --gate` | the repository's own linter |
| `make rehearsal` | **the executable proof of the migration procedure**, against the live installation with the production scheduler active |
| the tree diff vs `3d63244e` | §4 — that the merge introduces no deployment drift |

### 5.2 The validation RESULTS — run against the reconciled tree

The reconciled tree is the merge of all six heads onto `origin/main`
(`85dd9542`), with the production scheduler **active** and the live
installation untouched.

| Gate | Result |
|---|---|
| `make build` | **OK** — all five binaries |
| `make test-scripts` | **OK** — **758 tests** (1 skipped) |
| `make test-race` | **OK** — the canonical Go gate, with the race detector |
| `make docs-check` | **OK** — 13 checks, 0 review items, 0 errors |
| renderer parity (`agent_installation/`) | **OK** — **473 tests** (1 skipped) |
| `mpm-lint --gate` | **PASS** |
| the rehearsal, production scheduler **active** | **PASSED** — **135 assertions, 0 failures, 0 suites failed** |
| the tree diff vs `3d63244e` | **9 files differ**, all of them the rehearsal harness, its tests and documentation; 0 in every runtime area (§4) |
| the compiled plugin payload vs the deployed one | **✔ BYTE-IDENTICAL** (`dist/index.v2.js`) |

**CI on the three PRs opened or changed this session:** #190 **green**
(Build + core tests, Full test suite, Release gate), #192 **green** (all
three), #193 **green** (all three).

### 5.3 One thing the validation caught, and fixed

The reconciled tree's script suite failed with **exactly one** failure:
the repository's own guard, `scripts/tests/test_no_obsolete_paths.py`,
flagged the reconciliation record for naming `scripts/install.sh` and
`scripts/uninstall.sh` — paths that do not exist (the canonical installer
lives at the repository root), and that the guard forbids naming at all.
The record's area table now names the paths that exist. Re-verified:
**758 tests OK**.

A guard catching its own repository's documentation is the guard working.

---

## 6. Merge-readiness verdicts

| PR | Verdict | Notes |
|---|---|---|
| **#187** onboarding timeout | **ready** | CI green; independent of the rest; no shared-interface change beyond the ones A arbitrated |
| **#188** installer rollback safety | **ready** | CI green; A's first merge; the installer is the riskiest surface and it was merged first by A, which is the order to keep |
| **#189** lesson trust boundary | **ready, with one consequence** | CI green; it is the breaking change, and it **breaks the rehearsal's lesson seeding** — which #192 fixes. Merging #189 without #192 leaves the rehearsal red |
| **#190** the rehearsal's isolation gate | **ready after this session's two targeted fixes** | CI green before; the two review points (the narrowed operational exceptions, the release-pointer integrity) are now in, with 45 tests and demonstrated non-vacuity |
| **#192** the rehearsal's lesson seeding | **ready — and it must not be skipped** | 4 tests, non-vacuity demonstrated; without it the rehearsal is red on the merged tree |
| **#193** A's two integration-only corrections | **ready** | A's own commits, cherry-picked verbatim; the four files are byte-identical to A's integration branch; it is the difference between a merge that reproduces the deployed runtime and one that does not |

---

## 7. What was NOT done

* **Nothing was merged.** A is the merge authority; the cutover is in
  progress and no transfer of authority has been given.
* No change to A's source worktree, the installed runtime, the database,
  the configuration, the scheduler or the OpenCode files. Every read of
  the live installation was read-only (`file:…?mode=ro`, `readlink`,
  `cmp`).
* No deployment, no release, no tag.
* No modification of live credentials.
* Issue **#178** stays open. Issue **#60** stays open — it requires A's
  live MiniMax verification evidence, which this session did not gather
  and must not gather.
* No unrelated work.
