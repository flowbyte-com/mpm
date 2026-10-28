# Session C handoff — the compact managed block, for the engineer taking over

> **Date:** 2026-10-29. **Author:** a successor session, continuing Session C's work.
> **Worktree:** `/tmp/opencode/managed-block` (a git worktree of `/home/v/src/mpm`).
> **Subject:** the *compact managed instruction block* for the OpenCode adapter
> ("Session C"). Companion report: [`session-c-compact-managed-block.md`](session-c-compact-managed-block.md).
>
> **Read this first:** §1. The work is **not on a branch**. It exists only as
> uncommitted changes in a **detached-HEAD** worktree. That is the single
> largest risk in this handover, and it is not mentioned in the companion report.

---

> ## ⚠ STATUS — the handover has been actioned (2026-10-28)
>
> This document is **retained as the record of what was handed over** and of
> **what was wrong with the handover's own premises**. It is not a live
> to-do list. A successor session executed the items below; the body of the
> document is left **unchanged and dated** so the history of the handover is
> not rewritten to look tidier than it was.
>
> | Hand-over item | State now |
> |---|---|
> | **§1** — the work was on a detached HEAD, uncommitted, on no branch | **DONE.** Branch `feat/compact-managed-block` created from `f818bfe`; the work is committed as `a50685f`. `git checkout -b` from the detached HEAD preserved the tree exactly — nothing was lost. |
> | **§3.1** — §10.3 of the companion report is empty | **DONE.** §10.3 is written, with the per-cell table, the timeouts as timeouts, the contaminated cells as contaminated, and the negative control as the strongest uncontaminated result. |
> | **§3.2 / §3.2.1** — the comparison cannot support the claim it was built for; the detector has a blind spot | **DONE.** The detector now scans the agent's **full transcript** (its tool calls and their outputs) as well as the host's output, with the arm's own legitimate reading stripped first. And the arm is now a whole **world** — block + protocol + canonical source — served per arm from a disposable `~/.mpm`, so the old arm can no longer read the 4.0.0 commitments it was being compared against. |
> | **§3.3** — the resume path overwrites the JSON | **DONE.** The writer now **merges**, keyed on `(variant, scenario)`. A resumed run replaces the cells it re-runs and keeps the rest. |
> | **§3.4** — D1 and D2 unfixed | **DONE.** Fixed in `src/index.ts` **and** `src/index.v2.ts` (see below — the handover named only the first file, and that was itself a defect). |
> | **§3.6** — D7, the wrong `mpm_lessons` type enum | **DONE, and the handover under-called it.** See the correction below. |
> | **§3.7** — the Go suite was terminated, not passed | **DONE.** The Go suite is run to completion on this branch; the result is recorded in the companion report's §6.3 ("Suite results"). |
>
> ### Three corrections to the handover itself
>
> These matter more than the items above, because each one is a place where
> following the handover would have produced a wrong result.
>
> 1. **D1 and D2 live in TWO files, not one.** §3.4 says both live in
>    `agent_installation/mpm-opencode/src/index.ts`. They do — but the plugin's
>    tool descriptions are **duplicated verbatim** in `src/index.v2.ts`, and
>    `package.json`'s `main` is `./dist/index.v2.js`, so **the `index.ts` copy
>    is the one OpenCode does not load**. A fix applied only to the file the
>    handover named would compile cleanly, pass every test, and **not reach the
>    agent**. This is recorded as **D8** in §11.1 of the companion report.
> 2. **"The compact block itself is not affected" (D7) is wrong.** The
>    handover said the matrix row "names `mpm_lessons` action `save` without
>    pinning a `type`". It did pin one: the row read *"Correction, recurring
>    pitfall, technique"*, and `correction` and `technique` are exactly the
>    values the substrate **refuses** (`invalid lesson type "correction": must
>    be one of warning, practice, or insight`). The row now reads *"Insight,
>    warning, or practice worth repeating"* — the three real values verbatim.
> 3. **The handover's §1 was right about the risk and wrong about the
>    mechanism.** The work was on no branch, but `git checkout -b` from the
>    detached HEAD is non-destructive, so the fix was safe and cheap. The
>    handover's instinct — *branch the work before running anything that
>    could touch the tree* — was correct and was followed.

---

## 1. The one thing to do before anything else

The Session C work is **uncommitted and unbranched**.

```
$ git -C /tmp/opencode/managed-block rev-parse --abbrev-ref HEAD
HEAD                      # detached
$ git -C /tmp/opencode/managed-block branch --list feat/compact-managed-block
                          # no such branch
$ git -C /tmp/opencode/managed-block log --all --oneline --grep=compact -i
                          # nothing: the work is on no branch and in no commit
```

The companion report (§"Status", line 10) states the branch is
`feat/compact-managed-block` "from `origin/main` @ `f818bfe`". **That branch was
never created.** The work is real and complete-looking, but it lives only in
the working tree of a detached HEAD.

**Do this first**, before any experiment that could reset the worktree:

```bash
cd /tmp/opencode/managed-block
git checkout -b feat/compact-managed-block        # keeps the working tree as-is
git add -A                                         # incl. the untracked new files
git commit -m "feat(agent-installation): the compact managed block for OpenCode"
```

`git checkout -b` from a detached HEAD preserves the working tree exactly, so
this is a safe, non-destructive first move. `git stash` is **not** a substitute
— it would separate the work from its report.

Untracked files that must not be lost (they are listed as `??` by `git status`):

| Path | What it is |
|---|---|
| `agent_installation/measure/` | three verification harnesses + `fixtures/` (the 3.0.0 baseline) |
| `agent_installation/tests/test_managed_block_behaviour_matrix.py` | 11 tests pinning the matrix |
| `agent_installation/tests/test_cross_agent_discoverability.py` | 5 tests, Scenarios A–D |
| `docs/dev/session-c-compact-managed-block.md` | the companion report (44 KB) |

---

## 2. What state the work is in

**Implemented, verified, and green — but not finished, not merged, not deployed.**

| | |
|---|---|
| **Scope** | Replace the always-on OpenCode managed block (11,225 B / 2,009 tok) with a 2,526 B / 590-tok behaviour matrix; bump the behavioural contract 3.0.0 → 4.0.0; add a `THE BEHAVIOURAL COMMITMENTS` section (C1–C12) to the canonical protocol. |
| **Where it lives** | uncommitted changes to 13 tracked files (+4 untracked paths), on detached HEAD @ `f818bfe` (= `origin/main`). |
| **Branch base** | `origin/main` @ `f818bfe`. The accepted RC baseline `4111c46` is untouched and additive. |
| **Deployed?** | **No.** The live `~/.mpm` is still at `3df7231`; the live `~/.config/opencode/AGENTS.md` is byte-unchanged (`md5 cb00135ea4a0c42729232f10aee36805`). |
| **Merged?** | **No.** Awaiting Session A's review and operator approval. |
| **Blocking?** | Nothing external. §5 lists what is incomplete. |

### 2.1 What is finished and verified

Verified by me (a successor session) on 2026-10-29, in this worktree:

| Check | Command | Result |
|---|---|---|
| Python suite | `cd agent_installation && python3 -m unittest discover tests` | **469 tests, OK** (skipped=2; the report says skipped=1 — the extra skip is environment-dependent, not a regression) |
| Drift gate | `python3 agent_installation/scripts/render_managed_blocks.py --check` | **parity**: 1 adapter, 1 copy/paste example, README example all byte-for-byte |
| Non-vacuity | `python3 agent_installation/measure/mutation_probe.py` | **9/9 mutations detected** — the tests can actually fail |
| Installer regression | `agent_installation/measure/installer_regression.py` | **26/26 checks pass** (fresh install, upgrade, reinstall, existing-block replacement, retired-host safety) |

### 2.2 The change, in one paragraph

The canonical block in
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` becomes a ten-row table
mapping *situation → destination* (`mpm_memory` / `mpm_decisions` /
`mpm_lessons` / `mpm_evidence` / `mpm_theories` / `mpm_work` / `mpm_skills` /
`mpm_references` / `mpm_handoff` / and "nothing" for routine information).
The contract did not shrink — it **moved**. The twelve behavioural
commitments C1–C12 moved into the canonical protocol, one path away from the
always-on block. `agent_installation/mpm-opencode/templates/AGENTS.md.snippet`
and the README's generated example are regenerated artefacts of that source.

---

## 3. What is unfinished

### 3.1 §10.3 of the companion report is empty — the measurement was run but never written up

This is the biggest gap. `docs/dev/session-c-compact-managed-block.md` line 524
reads literally:

```
### 10.3 Results

*(filled in below)*
```

The harness **was** run and **did** produce results, but they were never
transcribed into the report. The run has now completed all 10 cells, so the
table in §3.2 is the full result set — §10.3 can be written from it. The
artefacts:

| Artefact | State |
|---|---|
| `/tmp/opencode/behaviour-comparison.json` | **6 of 10 cells** — see §3.2 |
| `/tmp/opencode/behaviour-run.log` | ends mid-cell at `=== old / transfer ===` |

### 3.2 The behavioural comparison is complete — but it cannot support the claim it was built for

The run finished all 10 cells (5 scenarios × 2 arms). The full table, from
`/tmp/opencode/behaviour-comparison.json` at 09:51:

| Cell | `rc` | mem | dec | theo | les | hand | work | Contaminated | Usable? |
|---|---|---|---|---|---|---|---|---|---|
| `old / reusable-discovery` | 0 | 1 | 0 | 0 | 1 | 0 | 0 | no | **yes** |
| `new / reusable-discovery` | 0 | 1 | 0 | 0 | 1 | 0 | 0 | no | **yes** |
| `old / uncertain-hypothesis` | **-1** | 1 | 0 | 0 | 0 | 0 | 0 | no | **no** — timed out |
| `new / uncertain-hypothesis` | 0 | 1 | 0 | **1** | 1 | 0 | 0 | **yes** | **no** — spoiled |
| `old / routine-observation` | 0 | 0 | 0 | 0 | 0 | 0 | 0 | no | **yes** |
| `new / routine-observation` | 0 | 0 | 0 | 0 | 0 | 0 | 0 | no | **yes** |
| `old / transfer` | **-1** | 0 | 0 | 0 | 0 | 0 | 0 | no | **no** — timed out |
| `new / transfer` | 0 | **2** | **1** | **2** | 1 | **1** | **1** | **yes** | **partially** |
| `old / acknowledgement` | 0 | 0 | 0 | 0 | 0 | 0 | 0 | **yes** | **yes** (zero records) |
| `new / acknowledgement` | 0 | 0 | 0 | 0 | 0 | 0 | 0 | no | **yes** (zero records) |

**Read the pattern before you read the cells. It is the opposite of the one
the report is set up to tell.**

- **Where both arms completed cleanly, there is no difference.**
  `reusable-discovery`: identical (1 memory, 1 lesson each).
  `routine-observation`: both wrote nothing.
  `acknowledgement`: both wrote nothing — and the old arm's cell is
  *contaminated* while writing zero records, so the flag is noisy in both
  directions.
- **Where the new arm distinguished itself, the old arm's cell timed out.**
  The new arm created a **theory** on `uncertain-hypothesis` and wrote
  **eight records across six collections** on `transfer` — including a
  decision, a work item and a handoff. The old arm was killed at the
  600-second budget in **both** of those scenarios, having written only one
  memory in one of them and nothing in the other.

**So the comparison cannot support the conclusion it was built to reach.** The
old arm's cells are missing precisely in the two scenarios where persistence
was being measured, and it was killed *for being slow*, not for failing to
behave. The report must not state or imply that the compact block makes
agents persist more, because the cells that would show it either timed out
(old) or are flagged contaminated (new).

**What it *does* support**, and what §10.3 should say:

1. The compact block's **uncertainty rule works in practice**: the new arm
   created a pending theory where a memory would otherwise have gone.
2. The **negative control passes on both arms**: neither block writes for a
   routine observation or an acknowledgement. That is a real, controlled,
   uncontaminated result and it is the strongest thing in the table.
3. The **harness is not yet fit to measure the difference it exists to
   measure.** Two fixes are needed before any claim can be made: a larger
   per-cell budget for the demanding scenarios (§3.3), and a contamination
   detector that scans the agent's full transcript (§3.2.1).

**Both arms were contaminated by the same route** on `transfer` — the prompt
sends the agent to inspect the whole project, and both agents found the
harness's own artefacts and the real repository. The `new` arm was flagged
(it narrated the exact filenames); the `old` arm was not (it read the same
files through tool calls, which never reach the host's stdout — §3.2.1). The
contamination is arguably *controlled*, but the flags do not show that, and
the detector is not what established it. Say so explicitly in §10.3 rather
than leaving a reader to infer it from the flags.

**Three cells are unusable, for three different reasons. Do not read them as
results.**

1. **`new / uncertain-hypothesis` is spoiled.** Marked `CONTAMINATED` (hit
   `tokvenv`): the agent read the harness's own venv and reported
   harness-adjacent artefacts. Per the harness's own rule, a contaminated cell
   is *reported, not counted* — so it cannot be used as evidence either way.
2. **`old / uncertain-hypothesis` and `old / transfer` hit the 600-second
   per-cell budget** (`returncode -1`). The harness's own comment is explicit:
   *"A cell that times out is a MISS, not an abort."* But the **cause** matters:
   the `transfer` scenario asks the agent to inspect an entire project, which
   does not fit in a 600-second cell. That is a **harness-budget finding, not
   a behavioural result about the 3.0.0 block.**
3. **`old / acknowledgement` is flagged contaminated while writing zero
   records.** The flag fires on the agent's narration, not on what it
   persisted, so a cell can be flagged for mentioning a filename it never
   acted on. Read the flags as a screen, not a verdict.

### 3.2.1 The contamination detector has a blind spot

The detector scans `proc.stdout + proc.stderr` — the **host process's**
output — not the agent's tool-call transcripts (`block_behaviour_comparison.py`
lines 94–104). An agent that reads a harness artefact **through a shell
command** sees it in its own tool output, which never reaches the host's
stdout, so it is not flagged.

Concretely: the `old / transfer` cell read the harness source, the 3.0.0
fixtures and the real repository, and was **not** marked contaminated — while
the adjacent `new / uncertain-hypothesis` cell was marked contaminated for
reading the harness's venv, because that cell *mentioned* it in its answer.

Two consequences for the next engineer:

- A `contaminated: false` flag is **not** evidence that a cell was clean.
  Treat cells that did substantive tool work with suspicion, whatever the flag
  says.
- If the detector is meant to be trustworthy, it has to scan the agent's
  **full transcript** (the host's session/artifact store, or a wrapper that
  records tool input and output), not the host's stdout. Until then, treat
  the contamination flags as a **lower bound**.

### 3.3 The harness's resume path has a data-loss trap

`block_behaviour_comparison.py` rewrites the whole `--json` file after **every**
cell, from the in-memory `results` list of **that invocation only**. It does
**not** merge with an existing file. So resuming with:

```bash
python3 agent_installation/measure/block_behaviour_comparison.py \
  --json /tmp/opencode/behaviour-comparison.json --scenarios transfer acknowledgement
```

**overwrites** `/tmp/opencode/behaviour-comparison.json` with only the 4 new
cells, destroying the 6 completed ones. **Copy the existing file first**
(`cp /tmp/opencode/behaviour-comparison.json /tmp/opencode/behaviour-comparison.cells1-6.json`),
or write to a different `--json` path and merge by hand.

The interrupted run was launched as:

```bash
cd /tmp/opencode/managed-block && rm -f /tmp/opencode/behaviour-comparison.json && \
  nohup timeout 3300 python3 agent_installation/measure/block_behaviour_comparison.py \
    --json /tmp/opencode/behaviour-comparison.json --timeout 600 \
    > /tmp/opencode/behaviour-run.log 2>&1 &
```

Note `--timeout 600` (10 min/cell) and `timeout 3300` (55 min overall). The
overall budget is the binding constraint on a full re-run: 10 cells × up to
10 min does not fit in 55 min. Budget ~2 hours for a clean 10-cell run, or
run the remaining cells in two invocations and merge.

**Raise `--timeout` for the `transfer` scenario.** Two cells have already
died at 600 s (§3.2), and both are cells where the agent was doing
legitimately lengthy work rather than hanging. 600 s is a reasonable budget
for `reusable-discovery` and `routine-observation`; it is not for `transfer`.
Either give that scenario its own larger budget, or drop it (§5, step 5) —
but do not let it keep recording timeouts and call the result a miss.

### 3.4 D1 and D2 are still unfixed — and they are a stated condition of the recommendation

The companion report §13 condition 2 says D1 and D2 **must** be fixed before
or with this branch. They are **not** fixed. Both live in
`agent_installation/mpm-opencode/src/index.ts`:

| # | Line | Current (wrong) | Should be |
|---|---|---|---|
| **D1** | 602 (`mpm_handoff` description) | `Required params.session_id, params.summary. Optional: … params.note.` | Only `summary` is required. `state`/`commitments`/`open_questions` are optional. **`note` must be removed** — it is silently dropped. |
| **D2** | 512 (`mpm_theories resolve` description) | `Required params.theory_id, params.conclusion, params.new_status.` | The schema uses **`theoryId`** (not `theory_id`); only `theoryId` + `conclusion` are required; `newStatus` (`proven`\|`disproven`) is optional. |

These are plugin *descriptions* that teach an agent to write a field the
substrate discards. Leaving them unfixed means the new block and the plugin's
own description disagree **in the same session** — which is the exact failure
mode the compact block was designed to remove.

### 3.5 The other four documentation defects are reported, not applied

§11 of the companion report lists D3–D6. They are **reported only** — the
branch was scoped away from them — and all four are still open:

| # | File | Defect |
|---|---|---|
| **D3** | `agent_installation/mpm-agent-protocol.md` §4.1 | Names five hosts on the `mpm-mcp` surface; four were retired 2026-10-27, and OpenCode uses its own 17-tool plugin surface. |
| **D4** | `MPM_AGENT_INTEGRATION_SNIPPETS.md` tool-reference table | The table is the canonical record of each tool's action enum but is **not checked against the live registry**. It and the plugin have already drifted once. Needs a test that parses `internal/core/tools/registry_list.go` and asserts the table matches. (The *matrix* has this test; the *table* does not.) |
| **D5** | `internal/core/tools/registry_list.go` (`mpm_skills`) | Says `content` + `name` are required. In fact `version` is also required and `content` needs YAML frontmatter. Minimal working form: `{"name":…, "version":…, "body":…}`. |
| **D6** | `internal/core/tools/registry_list.go` (`mpm_references`) | `action=add` is an **ingestion** operation (fetches a URL / parses a local file), not a pointer-recording one. Recording a URL for later belongs to `mpm_evidence action=add`. |

### 3.6 A seventh defect, found on 2026-10-29 and not yet in §11

**D7 — the companion report §3 states the wrong `mpm_lessons` type enum.**

Report §3 (the "why this row exists" table) says:

> The registry's `mpm_lessons save` takes `fact` + `type` (`warning`,
> `correction`, `technique`).

The real registry enum is **`["insight", "warning", "practice"]`**
(`internal/core/tools/registry_list.go:149`). Verified two ways: by reading
the registry, and by a live call that was rejected with
`invalid lesson type "correction": must be one of warning, practice, or
insight`. `correction` and `technique` do not exist.

This one is cheap to get wrong and expensive to leave: an agent that reads
the report and follows it writes a lesson with an invalid `type`, and the
write **fails**. It also matters for D4 — the report's own argument is that
the tool-reference table must be checked against the live registry, and here
is a second table in the same document that already disagrees with it.

The compact block itself is **not** affected: the matrix row for lessons
names `mpm_lessons` action `save` without pinning a `type`, so nothing the
block teaches is wrong. The defect is in the report's prose, and in the
protocol if the enum was copied there.

### 3.7 The Go test suite was terminated, not passed

`/tmp/opencode/make-test3.log` (2026-10-28 09:17) ends with:

```
make: *** [Makefile:605: test] Terminated
EXIT=2
```

That is a **timeout/kill**, not an assertion failure. The Go suite was never
observed green at this commit by this session. The Python suite (the surface
this branch actually touches) **is** green — 469 OK.

---

## 4. What is blocked

| # | Blocker | Owner | Notes |
|---|---|---|---|
| **B1** | **Session A's review.** The branch is "not merged, not deployed; awaiting Session A's review and operator approval." | Session A (adjudicator) | The review material is `docs/dev/session-a-adjudication.md` (49 KB) plus the companion report. |
| **B2** | **The lifecycle transport default** is an open *product* decision (options 1/2/3, RC report §7.2), owned by "the adapter's owner (Session C), adjudicated by A". | Session C + Session A | Option 1 or 2 removes release-note condition 1; option 3 keeps it as a release-note line. **This is the one decision still assigned to the role this handoff replaces.** |
| **B3** | **Two prior-session PRs remain open**, awaiting Session A: **#148** (`fe767f2`, session-lifecycle observations) and **#149** (`89afaed`, the session-end investigation that finding F2 rests on). | Session A | Both commits exist locally and on origin. Neither is affected by this branch. |

Nothing is blocked on infrastructure, credentials, a missing binary, or an
unavailable dependency. The `mpm` binaries build (`make build`), `opencode`
v2.1.4 is at `/home/v/.opencode/bin/opencode`, and the Python suites pass.

**Not blocked, but owned elsewhere:** the lesson surface's pending state
(D2-core, a schema change) and lifting the P2b gate (needs F2 and D2-core
first). The RC is a **CONDITIONAL GO** with two one-line release-note
conditions (RC report §9).

---

## 5. What to do next, in order

1. **Branch and commit the work.** §1. Nothing else matters until the work
   survives a worktree reset. Do this before running anything that could
   touch the tree.
2. **Fix D1 and D2** in `agent_installation/mpm-opencode/src/index.ts` (§3.4).
   Small, mechanical, and a stated condition of the recommendation. Re-run the
   Python suite afterwards.
3. **Fix the harness, then re-run the two timed-out cells.** The run is
   complete but **the comparison cannot currently support the claim it was
   built for** (§3.2) — the old arm timed out in exactly the two scenarios
   where persistence was being measured. Two changes are needed: a larger
   `--timeout` for the demanding scenarios (600 s is too small for
   `transfer`), and a contamination detector that scans the agent's full
   transcript rather than the host's stdout (§3.2.1). Copy
   `/tmp/opencode/behaviour-comparison.json` aside before any re-run (§3.3) —
   the harness overwrites it from the current invocation only.
4. **Write §10.3 of the companion report** — the results table, the misses,
   the contaminated cell, and the sample-size caveat. §10.1 already states the
   design; §12 L4/L9 already state the limitations. **§10.3 is the only
   section that needs authoring.** Write it to say what §3.2 says: the
   negative control passes on both arms, the uncertainty rule works, and
   **the old arm's cells are missing where it matters** — so the report must
   not claim the compact block increases persistence. Report timed-out cells
   as timeouts, not as misses; report the contaminated cells as contaminated.
5. **Decide whether `transfer` belongs in the comparison at all.** Its prompt
   sends the agent to inspect the entire project — which includes the
   harness's own artefacts — so it is inherently contamination-prone, and it
   does not fit in the current cell budget. Either exclude it and rely on the
   three scenarios that complete cleanly, or give it its own budget and its
   own contamination rule. This is a design call to make **before** re-running,
   not after.
6. **Fix D3–D7** or file them as tracked follow-ups (§3.5, §3.6). D4 and D7
   are the two with test-shaped fixes, and D7 is a one-line prose fix.
7. **Re-run the Go suite to completion** (§3.7) so the branch has an observed
   green Go run, not a terminated one.
8. **Hand to Session A for review**, and settle the lifecycle-transport
   default (B2).

---

## 6. Reference map

| Need | Path |
|---|---|
| The companion report (44 KB; §10.3 empty) | `docs/dev/session-c-compact-managed-block.md` |
| The canonical source of the block (edit here) | `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` |
| The canonical protocol (now carries C1–C12) | `agent_installation/mpm-agent-protocol.md` |
| The renderer + drift gate | `agent_installation/scripts/render_managed_blocks.py` |
| The harnesses + what they are for | `agent_installation/measure/README.md` |
| The 3.0.0 baseline (comparison arm) | `agent_installation/measure/fixtures/block-3.0.0.md` |
| The new matrix test (11 tests) | `agent_installation/tests/test_managed_block_behaviour_matrix.py` |
| Discoverability test (Scenarios A–D) | `agent_installation/tests/test_cross_agent_discoverability.py` |
| The RC readiness report (CONDITIONAL GO) | `docs/dev/release-candidate-2026-10-30.md` |
| Session A's review material | `docs/dev/session-a-adjudication.md` |
| Harness run artefacts | `/tmp/opencode/behaviour-comparison.json`, `/tmp/opencode/behaviour-run.log` |
| Installer regression artefact | `/tmp/opencode/installer-regression.json` |

### 6.1 Non-obvious facts worth knowing before you touch this

- **The canonical source carries the managed block TWICE** — once as the
  OpenCode copy/paste example (first occurrence), once as the host-neutral
  canonical block (last occurrence). The renderer takes the **last**. A naive
  search-and-replace hits the wrong one. The mutation probe now replaces **all**
  occurrences, because a mutation that hit only the first left every test green
  *for the wrong reason*.
- **The `AGENTS.md.snippet` and the README's block example are generated.**
  Never hand-edit them; edit the canonical source and re-run the renderer.
- **One line of the block is intentionally not hard-wrapped** — the
  reference-discovery sentence. `test_installed_block_drift.py` strips that
  line to build a stale-block fixture and can only do so if the phrase is
  unbroken. The drift test now asserts this, so the constraint is documented
  rather than accidental. Do not re-wrap it.
- **Size ceilings moved** from 14,000 B / 2,000 words to **3,100 B / 460
  words**. Any old ceiling assumption in a test will now fail.
- **`mpm_work` is not a native OpenCode tool.** It is reachable only through
  the `mpm call <tool>` CLI fallback. The block names it because it is the
  correct destination; the CLI clause is what makes the name actionable here.
- **`mpm_handoff write` silently drops `context`, `next_steps`, `artifacts` and
  `tags`.** Only `summary`, `session_id`, `state`, `commitments` and
  `open_questions` persist. This was probed, not read off the schema, and it
  is why the block names the fields explicitly.
- **The substrate's shared collections really are shared.** FTS search does not
  filter by session: a fact written in session A is retrievable by session B
  with no handoff involved. Verified against the real binary (Scenario A).
- **The live installation was never touched** by any of this work. If you are
  unsure whether something was deployed, it was not.

---

## 7. Provenance of this handoff

Written by a successor session on 2026-10-29, from: the companion report, the
git history and working-tree state, the live MPM database, the harness
artefacts in `/tmp/opencode/`, and direct re-execution of the four verification
commands in §2.1. Claims not re-verified by that session are marked as such
in §3.7 and §4.

### 7.1 Where the successor's records live

The same handoff was written into the **live MPM store** so that the next
session's wake context carries it, not just the repository:

| Record | id |
|---|---|
| handoff | `860103507e3aa477` |
| memory — the detached-HEAD / branch-never-created risk | written 2026-10-29 |
| memory — `mpm_handoff write` silently drops fields | `32830fc1bafd38b8` |
| lesson (warning) — the harness overwrites its own output file on resume | `5e22db01ce4b6d3c` |
| lesson (insight) — contamination-prone scenarios | `781b440792114450` |

Query them on the live store (`/home/v/.mpm/src/db/mpm.db`) with
`mpm_memory query` and `mpm_handoff list`.

### 7.2 An unusual thing the successor should know

The session that wrote this document was **itself running inside the
harness** — specifically, as the `old / transfer` cell of the behavioural
comparison (the `transfer` scenario's prompt is verbatim the request that
produced this handoff). The live `AGENTS.md` it was given was byte-identical
to the 3.0.0 fixture, and its working directory was the harness's disposable
`old` cell.

Two things follow from that, and they are worth stating plainly rather than
leaving to be discovered:

- The `old / transfer` cell **timed out at 600 s** and recorded **zero** MPM
  records. That is **not** evidence that the 3.0.0 block makes agents persist
  nothing — the agent was killed mid-investigation, before it reached the
  point of writing. The cell is a timeout, and §3.2 says so.
- The successor's own transcript read every harness artefact and the real
  repository, so it **would** have tripped the contamination detector had it
  emitted a final message naming them. The cell was not flagged because the
  detector scans the host's stdout only (§3.2.1) and the cell was cut off
  before producing one. **Read §3.2.1 before trusting any `contaminated:
  false` flag.**
- The successor **is** the `old / transfer` cell. Its zero-record result is
  its own timeout, and the `new / transfer` cell — a different agent, with
  the 4.0.0 block — wrote 8 records across 6 collections and finished
  cleanly. If §10.3 is written from these cells, the old arm's zero must be
  attributed to the **budget**, not to the block. The successor did not
  write to the disposable store at all: its MPM writes failed (a
  client-side error), and it wrote the records to the **live** store
  instead (§7.1). So the `old` arm's zero is also partly an artefact of a
  failed write path, and is even weaker evidence than the timeout alone
  suggests. **Do not use it.**
