# Continuity 001, PR 1 — the review evidence

The durable assignment storage: two tables, seven indexes and thirty
triggers, created by a migration inside the fatal migration transaction and
carrying twenty-four numbered rules.

This is the evidence for **PR 1 of six** of Continuity 001 (issue #258, pull
request #259). It states the exact revision that was reviewed, what was
tested, what the adversarial passes found, the GitHub evidence, and what
blocks the merge.

---

## 1. The exact code revision

| | |
|---|---|
| **Reviewed revision** | `3e83130fa5ecdb996c81373d28cfa57dfdcb0ca5` |
| Branch | `feat/continuity-001-assignment-schema` |
| Base | `main` |
| Pull request | [#259](https://github.com/flowbyte-com/mpm/pull/259) |
| Tracking issue | [#258](https://github.com/flowbyte-com/mpm/issues/258) |
| The DDL | `internal/core/migration_agent_assignments.go` |
| **DDL pin** | SHA-256 `41f55ebbf41d18060684eb5f31ced3f5ea2d660fd465cc675fd20494dd7728bd`, asserted by `TestAgentAssignmentDDLIsPinned` |

The DDL is a **single literal**, and a test asserts its SHA-256. The hash
appears in three places — the test's pin, the design note's header and this
manifest — and the eighth independent review checked all three against the
literal itself, after the sixth review found that the note's copy had gone
stale. A reformat, a dropped constraint or an added column fails the build.

**This evidence file is published in a later, documentation-only commit.** It
does not name its own commit, and it changes no Go file, no DDL and no test:
the hash pin in the reviewed revision and the hash pin at the branch head are
the same value, which is what makes the reviewed artefact and the shipped
artefact the same artefact.

**A placeholder was published earlier in the campaign.** It was committed at
`6c462633` with `PENDING` in place of the revision and a stale DDL hash, and
it carried no findings, no CI and no verdict — only the shape of the file.
This commit replaces it with the finished evidence. The placeholder changed
nothing in the code: the reviewed revision's own pins are the ones above,
and they were correct before and after.

---

## 2. The tests

| gate | what it covers | result |
|---|---|---|
| `make build` | the five binaries and the updater | pass |
| `make test` | every package, with the FTS5 build flags | pass — 146 packages |
| `go run ./cmd/mpm-lint --gate` | all nine classification gates | pass |
| `python3 scripts/docs_check --base 712064c3334ee33035bac747f841857f3221b910` | 13 documentation checks | pass — 0 errors, 0 review items |

The acceptance suite for this change is **62 tests** in `internal/core` plus
**2** command-level restore tests in `cmd/mpm`. Each invariant is observed as
a **refused write**, not as a comment — and each refusal is asserted against
the **rule that refused it**, so a refusal for the wrong reason is a
failure. That distinction is not cosmetic: three separate rounds found a
test passing for the wrong reason, and the assertion is what turned those
into failures.

**The restore is proved through the command.** `cmd/mpm/handlers_restore_assignment_test.go`
builds a dump from the **live** schema — the real DDL of every table, index
and trigger, plus real rows — and requires `mpm restore-db` to accept it and
to restore the objects and the rows. It does so in **two** shapes: the order
that proves the rows satisfy the schema (indexes and triggers first), and the
order `sqlite3 .dump` actually writes (the data before the indexes, with the
foreign keys turned off for the duration) — because `mpm backup` **is**
`sqlite3 .dump`.

Both tests are **non-vacuous**. The seventh review found that every
assertion they made was also true of the database as it was *before* the
restore, so a handler that validated the dump, returned 0 and wrote nothing
would have satisfied them. Each now writes a **marker** row after the dump is
built — so the dump does not carry it — and requires it to be **absent** from
the restored database; the first also requires the file to have been
**replaced**, not written in place. All three inventories (tables, indexes,
triggers) are compared against the live schema, so a stale inventory cannot
silently restore a weaker schema.

---

## 3. The adversarial results

### 3.1 Eight independent reviews

The change was reviewed **eight** times, in the installed sandboxed reviewer
(`mpm-reviewer`, default-deny, `sandbox_exec` only). Between them the rounds
returned **22 blocking findings**; every one was a real defect, and every one
is closed with a regression test that fails without the fix.

| round | blocking | minor / nit | what the blocking findings were |
|---|---|---|---|
| 1 | 11 | 13 | the implementer could skip its review; the role was mutable; the ledger permitted gaps and rewrites; a child could store its own allow-list; the tree could be malformed or self-parented; `TEXT PRIMARY KEY` permits NULL; the claim discipline checked updates only; INTEGER affinity does not make a number an INTEGER; a task could be deleted and re-inserted, resetting its epoch; the budgets accepted nonnumeric text; a BLOB and a TEXT key coexisted in the unique index |
| 2 | 2 | 14 | the delegation tree could be made into a **cycle**; a recorded approval could be **rewritten**, not merely cleared |
| 3 | 5 | 13 | the depth condition did **not** exclude a cycle; a child could be **promoted to a root**; `INSERT OR REPLACE` defeated every update- and delete-triggered rule; the gate's record was rewritable by the statement that **leaves** the gate; `approval_evidence` and `approved_at` were rewritable |
| 4 | 1 | 10 | `UPDATE OR REPLACE` deleted a **claimed** assignment, freed its id and reset its epoch |
| 5 | 2 | 8 | the assignment's **id was mutable**, so a rename freed it and a stale token became current on a different row; the restore test's trigger inventory was one short |
| 6 | **0** | 8 | — (two contract-vs-DDL gaps and three false claims in the documents, all closed) |
| 7 | 1 | 5 | the sixth round's fix was on the **UPDATE path only**, so an assignment could be **created** carrying a forged approval; the lease could be carried forward across a re-claim; both restore tests could pass **vacuously** |
| 8 | **0** | 5 | — (one harness under-measurement, one over-claiming column comment and three nits, all closed) |

**The verdicts, in order:** REQUEST CHANGES, REQUEST CHANGES, REQUEST
CHANGES, REQUEST CHANGES, REQUEST CHANGES, APPROVE WITH MINOR/NIT FINDINGS,
REQUEST CHANGES, APPROVE WITH MINOR/NIT FINDINGS.

The sixth and eighth rounds returned **no** blocking finding, and neither was
a sign-off. The sixth found two real gaps between the published contract and
the DDL; the seventh then found that the sixth's **fix** was on the UPDATE
path only, so a row could be created already carrying the gate's record or a
complete forged approval. A review round is not the same thing as an
approval, and the campaign is published in full rather than summarised to
its last word.

**A disclosure.** The reviewer's configured default model returned a quota
error partway through the campaign, so rounds 2–8 were run with
`--model minimax-coding-plan/MiniMax-M3.1-Flash`. The **sandbox and the
permission set were unchanged** — the same `mpm-reviewer` agent, the same
default-deny policy, the same `sandbox_exec`-only tool surface — and the
findings were captured host-controlled with a digest. Only the model
differed, and it differed in the direction of a *weaker* reviewer, which is
why the findings are reported rather than discounted.

### 3.2 The negative controls

**Thirty-four** controls, each removing one invariant and re-running the
acceptance suite. **Thirty-three** of them have at least one
invariant-specific failure. The counts are **measured**, not asserted:
`scripts/continuity-001-controls.py` runs every control and prints them, and
it refuses to measure anything at all unless the **baseline is green** — a
control run against a broken tree reports nothing rather than a number.

That guard earned its keep during this campaign: a stale harness from an
earlier run had left a mutation in the tree, and the baseline caught it and
refused to measure. The counts would otherwise have been a table of
plausible numbers describing nothing.

| control | tests that fail | invariant-specific |
|---|---|---|
| the state-machine trigger is removed | 15 | 8 |
| the initial-state trigger is removed | 7 | 1 |
| the unclaimed-at-creation trigger is removed | 7 | 1 |
| the id-not-reused trigger is removed | 8 | 1 |
| the born-empty trigger is removed | 7 | 1 |
| the id-immutability trigger is removed | 8 | 2 |
| the key-immutability trigger is removed | 8 | 2 |
| the claim-needs-a-live-assignment trigger is removed | 8 | 2 |
| the lease-changes-only-on-a-claim trigger is removed | 8 | 2 |
| the tree-shape trigger is removed | 7 | 1 |
| the tree-shape-on-UPDATE trigger is removed | 7 | 1 |
| the depth-immutability trigger is removed | 8 | 2 |
| the role-immutability trigger is removed | 7 | 1 |
| the root-recipient-is-eligible trigger is removed | 7 | 1 |
| the root-recipient-is-eligible-on-UPDATE trigger is removed | 7 | 1 |
| the permitted-operations INSERT trigger is removed | 9 | 3 |
| the permitted-operations UPDATE trigger is removed | 8 | 2 |
| the eligible-recipients INSERT trigger is removed | 7 | 1 |
| the eligible-recipients UPDATE trigger is removed | 7 | 1 |
| the not-deletable trigger is removed | 8 | 2 |
| the awaiting-action-is-immutable trigger is removed | 10 | 4 |
| the approval-is-immutable trigger is removed | 11 | 5 |
| the ledger's id-not-reused trigger is removed | 7 | 1 |
| the ledger's monotonic-index trigger is removed | 7 | 1 |
| the ledger's no-UPDATE trigger is removed | 7 | 1 |
| the ledger's no-DELETE trigger is removed | 7 | 1 |
| **all four** claim-discipline triggers are removed | 8 | 1 |
| the unique idempotency index is removed | 6 | **0** |
| the idempotency rule's index **and** its trigger are removed | 10 | 3 |
| the composite containment foreign keys are removed | 2 | 1 |
| the root/child budget `CHECK` is removed | 2 | 1 |
| the state vocabulary `CHECK` is removed | 2 | 1 |
| the `typeof()` type guards are removed | 6 | 5 |
| the whole migration is not called | 53 | 47 |

One control reports **zero**, and it is published rather than hidden: the
unique idempotency index is not independently load-bearing, because the
id-not-reused trigger refuses a duplicate key on its own. The index is
still needed — it is what lets the API **find** a duplicate without refusing
the write — so the control that measures the **rule** removes both layers.

The published table was compared against the measurement **row by row**:
34 controls in both, **no value mismatches**, and exactly one control with a
zero invariant-specific count.

### 3.3 What the reviews could not verify

The eighth review's sandbox has no Go module, so the suite could not be
built there and the review could not reproduce the control counts. It
verified the structural side — the control set, the `STRUCTURAL` set, and
that every mutator removes exactly the object it names — and said plainly
that the counts remained asserted rather than measured **in that review**.
They are measured here, by the harness this change carries.

---

## 4. The GitHub evidence

| | |
|---|---|
| Pull request | [#259](https://github.com/flowbyte-com/mpm/pull/259) |
| Head revision | `3e83130fa5ecdb996c81373d28cfa57dfdcb0ca5` |
| Tracking issue | [#258](https://github.com/flowbyte-com/mpm/issues/258) |

Every CI run on the branch is listed below, **per revision**, so a reader can
see that the CI that passed is the CI that ran the reviewed commit — not a
run of an earlier one.

| CI run | revision | conclusion |
|---|---|---|
| [44263362702](https://github.com/flowbyte-com/mpm/actions/runs/44263362702) | `7032ffb5` | success |
| [44269400027](https://github.com/flowbyte-com/mpm/actions/runs/44269400027) | `7c29ccb2` | success |
| [44282611630](https://github.com/flowbyte-com/mpm/actions/runs/44282611630) | `cfd2d41a` | success |
| [44293006872](https://github.com/flowbyte-com/mpm/actions/runs/44293006872) | `1230b062` | success |
| [44301192470](https://github.com/flowbyte-com/mpm/actions/runs/44301192470) | `962eea39` | success |
| [44317831158](https://github.com/flowbyte-com/mpm/actions/runs/44317831158) | `b24d4125` | success |
| [44335313151](https://github.com/flowbyte-com/mpm/actions/runs/44335313151) | `6c462633` | success |
| [44355178537](https://github.com/flowbyte-com/mpm/actions/runs/44355178537) | `a31cd6e3` | success |
| [44374424756](https://github.com/flowbyte-com/mpm/actions/runs/44374424756) | `3e83130f` — **the reviewed revision** | **success** |
| — | the documentation-only evidence commit | PENDING |

---

## 5. The verdict

**APPROVE WITH MINOR/NIT FINDINGS** — the eighth and final review's verdict,
and every one of its five findings is closed in the head revision.

The eight rounds are: five REQUEST CHANGES, one APPROVE WITH MINOR/NIT
FINDINGS, one REQUEST CHANGES, one APPROVE WITH MINOR/NIT FINDINGS. **22
blocking findings** in total, all closed, each with a regression test that
fails without the fix.

The review that matters is not the last one but the **set**: the seventh
round found a blocking defect in the sixth round's own fix, which is why the
campaign ran eight rounds rather than six. The storage's own claims are
enforced by the database — 34 properties, checked by 34 negative controls —
and the documents and the DDL agree about all of them.

---

## 6. Blockers

| | |
|---|---|
| **Blockers to the merge** | **none** |

Nothing outside this PR's scope is blocked by it. The dispatcher stays
disabled behind an explicit activation gate, every privileged operation
(merges, deployment, production data, security exceptions) stays outside the
automation surface, and PRs 2–6 carry their own gates.

**The merge itself is the operator's.** This PR is not merged by any
automation, and PR 2 does not begin until the operator says so.

---

## 7. The scope of this PR

PR 1 is **storage only**. There is no API, no CLI, no tool, no dispatcher and
no OpenCode adapter; the two tables are created empty and stay empty until
PR 2. PR #224's Stage 1 DDL is **referenced, not duplicated or altered**,
and no existing table, row, index or trigger is modified.

The six properties the schema deliberately does **not** enforce are listed,
with the reason for each, in
[`docs/dev/continuity-001-assignment-schema.md`](../../dev/continuity-001-assignment-schema.md)
§5 — and each is deferred to a named later PR, not left open.
