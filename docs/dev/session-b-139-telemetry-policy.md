# Session B — Issue #139 policy completion

> **Scope.** This document records the **policy-completion** work for
> issue #139: the approved product policy for `mpm-telemetry observe`,
> the specification amendment that makes it official, and the
> regression coverage that holds it in place.
>
> The technical basis is [#150](https://github.com/flowbyte-com/mpm/pull/150)
> (the observe correction) on top of
> [#151](https://github.com/flowbyte-com/mpm/pull/151) (the two
> independently reviewable path corrections). This PR is the third and
> last in that stack. **Nothing here is merged or deployed.**
>
> **No live telemetry, credential, deployment, scheduler configuration
> or automatic critic scheduling was touched.**

---

## 1. The approved policy, and where each point landed

| # | Policy point | Where it lives | How it is pinned |
|---|---|---|---|
| 1 | `observe` is a read-only diagnostic **by default** | `cmd/mpm-telemetry/observe.go` | `TestPolicy_DryRunIsACompatibilityAlias`, `TestObserve_DefaultReportsAndWritesNothing`, `TestE2E_ObserveReportsAndWritesNothing` |
| 2 | It **never** automatically creates trusted lessons or unsupported pending theories | the same file — there is no write path at all | `TestObserve_DefaultReportsAndWritesNothing` (row counts), `TestPolicy_TheSubstrateHandleIsReadOnly` (**structural**), `TestE2E_ObserveAndTheRealRegistryAgreeThatNothingWasStored` (the real registry) |
| 3 | `--dry-run` is kept as a **compatibility option** | `observe.go` | `TestPolicy_DryRunIsACompatibilityAlias` (byte-identical output), the smoke test's alias assertion |
| 4 | `--mpm` is **deprecated with an explicit warning**, not silently ignored | `observe.go` (`mpmFlagDeprecation`), `help.go` | `TestPolicy_MpmFlagWarnsExplicitly`, `TestPolicy_NoMpmFlagMeansNoWarning`, the smoke test's warning assertion |
| 5 | `src/db/telemetry.db` is the canonical store, **provided the remaining production consumers confirm it** | `internal/telemetry/paths.go` (unchanged) | `TestCanonicalStorePath_MatchesTheSystemdUnit`, `TestCanonicalStorePath_MatchesTheSpec`, `TestCanonicalSocketPath_MatchesTheSpec`, `TestCanonicalStorePath_MatchesTheSystemdSocket` |
| 6 | SPEC §6.6 is amended, preserving the distinction between observations and trusted knowledge | `docs/SPEC.md` §6.6 | the spec assertions above read the spec itself |
| 7 | The output stays **explicitly labelled unverified** | `internal/telemetry/observe.go` (`Observation.Status`, `Observation.Wrote`) | `TestPolicy_OutputIsLabelledUnverified`, the smoke test's shape assertions |

---

## 2. The policy, in the operator's terms

### 2.1 `observe` reports; it does not write

The default mode used to *promise* a write — a `type: "observation"`
lesson in the substrate. That promise was the trust-boundary exception
this issue is about, and it is gone.

```
$ mpm-telemetry observe --since 1700000000 --threshold 300
{
  "observation": "high-token-no-artifact",
  "status": "unverified-observation",
  "session_id": "sess_smoke",
  "invocations": 2,
  "input_tokens": 200,
  "output_tokens": 100,
  "total_tokens": 300,
  "artifacts": 0,
  "detail": "high-token-no-artifact: session sess_smoke burned 300 tokens across 2 invocations with zero artifacts",
  "wrote": "nothing"
}
```

Exit 0. Nothing written. A quiet store produces no output and exit 0 —
there is nothing to report, which is not a failure.

**Why the observation is not stored, in one sentence:** the token counts
live in `telemetry.db`, which no citation type can address, and the
observation is about an *absence* — a session that recorded nothing — so
there is nothing in `mpm.db` to cite. A trusted lesson would therefore
be an **unsupported** claim, and a pending theory an **unresolvable**
one.

**Arbitration is a human act.** An operator reads the report and decides
what, if anything, to record, and records it through the ordinary
surfaces, where the normal evidence rules apply.

### 2.2 The guarantee is structural, not conventional

The "writes nothing" property is not only a row-count assertion. Both
handles the observe path holds are opened with SQLite's `mode=ro`:

- the telemetry ledger — `telemetry.OpenReadOnly`;
- the substrate — `openSubstrateReadOnly`.

So the path **cannot** write. A write attempt fails with *"attempt to
write a readonly database"*. A future change that "helpfully" added a
write would have to reopen the substrate first, and
`TestPolicy_TheSubstrateHandleIsReadOnly` would fail at that point —
before any trusted row existed.

### 2.3 `--dry-run` is a compatibility alias

It used to be the "do not write" mode. The default is now read-only too,
so the flag means exactly what the default already means, and the two
produce **byte-identical** output (asserted).

### 2.4 `--mpm` warns, loudly

```
$ mpm-telemetry observe --mpm /opt/mpm/bin/mpm
warning: --mpm is DEPRECATED and has no effect.

mpm-telemetry observe no longer shells out to mpm. The artifact count is
computed IN-PROCESS, read-only, from the substrate's own attribution
rule — so this flag has nothing to select, and the binary it names is
never executed.

--mpm will be removed in the next release.
```

The warning goes to **stderr** (stdout is the report), is **not
suppressible**, and is emitted **only when the flag is passed** — a
deprecation warning that appears on every run is noise, and a flag that
warns when it was never used teaches the operator to ignore warnings.

"Silently accepting a flag that does nothing is how an operator keeps
believing the command writes."

### 2.5 The canonical location, confirmed by the production consumers

Policy point 5 is conditional — *"provided the remaining production
consumers confirm it"*. They do, and the confirmation is now **asserted
rather than assumed**, from three independent directions:

| Consumer | What it says | Asserted by |
|---|---|---|
| the collector | writes to `<workspace>/src/db/telemetry.db` | `TestE2E_ObserveReadsTheCanonicalStoreLocation` — the REAL collector, started with **no** `MPM_TELEMETRY_DB` override, exactly as the systemd unit configures it, receives a REAL frame over its REAL socket, and the REAL observer finds it |
| the systemd unit | `Environment=MPM_TELEMETRY_DB=%h/.mpm/src/db/telemetry.db` | `TestCanonicalStorePath_MatchesTheSystemdUnit` — the unit's value is expanded and compared against `StorePath(its own MPM_WORKSPACE)` |
| the specification | `$MPM_WORKSPACE/src/db/telemetry.db` | `TestCanonicalStorePath_MatchesTheSpec` — and the old path is asserted **absent** |

The socket path gets the same treatment: the code unified on `runtime/`
in commit `664081e` while the spec said `run/`, and
`TestCanonicalSocketPath_MatchesTheSpec` now pins both the presence of
the canonical path and the absence of the stale one.

**This is the assertion that would have caught the original divergence.**
The spec and the code disagreed for two months; the only consumer that
read the spec was the one that broke.

---

## 3. The specification amendment

`docs/SPEC.md` §6.6 is amended. The changes, in full:

1. **The store path** → `$MPM_WORKSPACE/src/db/telemetry.db`, overridable
   with `MPM_TELEMETRY_DB`, plus a new paragraph stating that
   `internal/telemetry.ResolveStorePath` is the **single** resolver used
   by the collector, every reader and the critic — and that the result
   is never cwd-relative.
2. **The socket path** → `$MPM_WORKSPACE/runtime/mpm-telemetry.sock`.
3. **The `observe` paragraph** → rewritten: a read-only diagnostic; the
   count computed in-process and read-only; **no** cross-DB subprocess,
   with the reason (every `mpm call` writes a `tool_invocations` row,
   even a read); both stores opened read-only; a missing ledger is a
   clear error and a missing substrate is reported as such.
4. **A new "An observation is not trusted knowledge" subsection** — the
   load-bearing distinction, stated as two numbered reasons (no
   resolvable evidence source; the observation is about an absence), with
   the explicit consequence: **never** a trusted lesson, **never** a
   pending theory, and the exact JSON shape the command emits.
5. **`--dry-run`** documented as a compatibility option; **`--mpm`**
   documented as deprecated, accepted, ignored, and warning.
6. **The verification paragraph** → the hermetic E2E now exercises the
   real path.

The distinction between observations and trusted knowledge is preserved
*and made explicit*, because the previous wording — *"surface as `type:
"observation"` lessons — descriptive, not prescriptive"* — described a
**lesson**, and a lesson is trusted on arrival.

---

## 4. The regression coverage: the real executable and the real registry

> **Do not repeat the fake-MPM and unreachable-threshold testing
> failures.**

The old coverage had two failure modes, and both make a suite look green
while proving nothing:

- **A fake `mpm`.** The old observe tests injected a shell script that
  answered `mpm_provenance` with `{"count":0}` — a tool that has never
  existed in the registry. The suite was green because the fake agreed
  with the code; the command was dead in production.
- **An unreachable threshold.** The hermetic E2E ran observe with
  `--threshold 999999999` *"to avoid cross-DB shell-out"*, so the path
  under test was never entered.

Both are gone. The new coverage builds the **real** `mpm` and
`mpm-telemetry` binaries from this tree (it does not *discover* them in
`.build/bin`, because a test that skips when a binary is missing is a
test that does not exercise anything), and drives them as **separate
processes** against a substrate the **real** `mpm` initialised.

| Test | What it exercises |
|---|---|
| `TestE2E_ObserveReportsAndWritesNothing` | the real binary, a qualifying session, a **reachable** threshold: the observation is reported and **no** trusted surface changes |
| `TestE2E_ObserveReadsTheCanonicalStoreLocation` | the real collector, with **no** `MPM_TELEMETRY_DB` override, receiving a real frame over its real socket — and the real observer finding it. The collector's own ledger is at the canonical path, and nothing is at the spec's old path |
| `TestE2E_TheRegistryStillHasNoProvenanceTool` | the **real registry**, asked directly: `mpm call mpm_provenance` must still answer *"unknown tool"*. This is the assertion that keeps the original defect from being reintroduced by a well-meaning *"just add the tool"* patch |
| `TestE2E_ObserveAndTheRealRegistryAgreeThatNothingWasStored` | after a real observe run, the real registry reports no lesson |
| `TestE2E_ChildrenRunInAHermeticWorkspace` | the guard: every child's environment pins `MPM_WORKSPACE` and `HOME` at temporary directories and is not the operator's |

### 4.1 An accident worth reporting

The first draft of the E2E file initialised the substrate with a child
that **inherited the ambient environment**. The child therefore resolved
the operator's `$HOME/.mpm` — and wrote **four rows** into the live
installation's `tool_invocations` table (rowids 12715–12718,
`mpm_context query_global_rules`, 2026-10-28 09:00:49, framework
`opencode`).

The test still **passed**, because the live substrate has the same
schema. That is precisely the failure `internal/testenv`'s own doc
comment names: *"the single most common way a test reaches
production"*.

The fix is that every child in the file now goes through
`testenv.Env(t)`, and `TestE2E_ChildrenRunInAHermeticWorkspace` asserts
the environment is hermetic. The live installation's row count is now
**unchanged** across a full E2E run (verified: 12745 before, 12745
after).

The four rows were left in place rather than deleted: they are truthful
records of calls that did happen, and `tool_invocations` is an audit
table whose permanent-deletion path is operator-only. They are reported
here so the operator can decide.

---

## 5. Non-vacuity

Every new assertion was checked against the fault reinstated:

| Reinstated fault | Test that failed | Observed |
|---|---|---|
| the spec names the workspace root again | `TestCanonicalStorePath_MatchesTheSpec` | `SPEC.md still names $MPM_WORKSPACE/telemetry.db — a path nothing writes to` |
| the default mode refuses instead of reporting (the previous policy) | `TestObserve_DefaultReportsAndWritesNothing` | `runObserve = …` — the default mode is a read-only diagnostic and must succeed |
| the substrate handle is opened write-capably | `TestPolicy_TheSubstrateHandleIsReadOnly` | the write succeeds, so the guarantee is conventional only |
| the deprecation warning is removed | `TestPolicy_MpmFlagWarnsExplicitly` | the warning does not mention *DEPRECATED* |
| the observation is shaped like a lesson write | `TestPolicy_OutputIsLabelledUnverified` | the output contains `"type"` / `"action"` / `"fact"`, which claims trusted knowledge |

---

## 6. Compatibility

| Surface | Before this PR | After | Classification |
|---|---|---|---|
| `observe` (default), qualifying session | exit **1**, a refusal on stderr, nothing written | exit **0**, the observation on stdout, nothing written | **the approved policy change** |
| `observe` (default), quiet store | exit 0, silent | exit 0, silent | compatible |
| `observe --dry-run` | exit 1 (broken) → exit 0, observation printed | unchanged | compatible |
| `observe --mpm <path>` | selected the binary for the cross-DB lookup | accepted, ignored, **warns on stderr** | **deprecated**, removal next release |
| `observe` output shape | the lesson payload | the diagnostic observation | **the approved policy change** |
| `mpm-telemetry query` / `cost` | exit 0 with an empty result, having created the ledger | exit 1 with a legible error; the ledger is never created | **#151** |
| the substrate | — | the sidecar opens it read-only, in-process | new coupling; **read-only** |

**The one genuinely breaking consequence** is the first row: the default
mode goes from *refusing* to *reporting*, and its exit code goes from 1
to 0. Automation that treated a non-zero exit from `observe` as "a
session needs attention" must now read stdout for the observations.
That is the approved change, and it is the only one.

---

## 7. What was NOT done

- **Nothing merged, nothing deployed.** The live installation was not
  modified (beyond the four audit rows in §4.1, which the test wrote
  before the hermetic-environment fix and which were left in place).
- **No automatic critic scheduling was enabled.** The critic's telemetry
  hunt remains a flag-gated, read-only diagnostic.
- **No registry tool was added.** `mpm_provenance` is still absent, and
  `TestE2E_TheRegistryStillHasNoProvenanceTool` holds it absent.
- **The missed-opportunity hunt's inline copy of the attribution
  expression** (`internal/critic/missed_opportunity.go:303-314`) is
  still there. Unifying it means restructuring a `GROUP BY … HAVING`
  aggregate — a different change with a different risk profile, and
  outside this issue.
- **No `mpm-telemetry query` behaviour change beyond #151's** — it is
  not this PR's subject.

---

## 8. A note on the CI trigger for a stacked pull request

The repository's workflow filtered its `pull_request` trigger to
`branches: [main]`. A pull request whose base is a **feature** branch
therefore produces **no checks at all** — the event never matches, so
the workflow never runs.

That is invisible until you stack, and this delivery stacks: PR #152's
base is `feat/session-b-139-telemetry-observe`, and it had **zero**
checks. A pull request with no checks cannot be reviewed on its
evidence, and splitting one change into independently reviewable pieces
is pointless if the pieces arrive without the evidence.

The fix is a **single commit, `e0183cb`**, on the branch in the
**middle** of the stack (#150). The placement is not arbitrary:

- The trigger is evaluated against the pull request's **merge ref**,
  not against `main` — confirmed empirically: #150's base is
  `fix/telemetry-canonical-store-path`, and after the commit landed
  there the workflow ran for it, which a `branches: [main]` filter
  alone would never have permitted.
- So the commit has to be present in **both** #150's merge ref and
  #152's merge ref. #150's branch is an ancestor of #152's, so one
  copy there reaches both.
- Putting it in the **bottom** pull request (#151) would *not* have
  worked: #150's branch diverged from #151's at `6219345`, so #151's
  head is not an ancestor of #150's, and #152's merge ref never
  carried it. That was the first attempt, and it produced no run —
  which is how the mechanism was identified.

Two consequences worth stating plainly:

- Once #150 merges, `main` carries the widened trigger and every
  future stacked pull request is covered without a further change.
- It is the one commit in the stack that is **independent of the
  telemetry work**. A reviewer who objects to it can drop it without
  touching anything else.


---

## 9. Correction — the trigger landed by a different, broader commit

*Appended by Session A during the release-freeze integration, 2026-10-28.
The section above is left unchanged as the dated record of what was known
when it was written.*

§8 says the fix is "a single commit, `e0183cb`, on the branch in the middle
of the stack". That was true of this stack at the time. It is not what
`main` carries today.

While the release candidate was being assembled, the **documentation stack**
(#160 → #161 → #162) independently widened the same trigger, and it widened
it **without a `branches:` filter at all** — every pull request runs the
checks, whatever its base. #160 landed first, so `main` carries the
unfiltered form.

`e0183cb`'s narrower form (`branches: [main, 'fix/**', 'feat/**']`) was
**not** used, for a reason this section's own diagnosis points at: it
excludes the `docs/**` bases this repository actually stacks in. The
documentation stack bases #161 on `docs/correctness` and #162 on
`docs/library`, so a `fix/**`+`feat/**` filter would have left both of them
reporting **no checks** — re-opening the very gap §8 identifies.

The commit `e0183cb` was therefore dropped in the integration, and its
rationale is preserved in the comment above `pull_request:` in
`.github/workflows/build-test.yml`. The runner-minutes concern it raised is
real, and is already bounded by the workflow's `concurrency` group, which
cancels a superseded run per ref.

The mechanism §8 describes is unchanged and still true: the trigger is
evaluated against the pull request's **merge ref**, so a commit must be
present in the merge ref of every pull request in the stack for all of them
to be covered. With the unfiltered form, that constraint disappears — which
is why the documentation stack's own pull requests now report checks where
they previously reported none.
