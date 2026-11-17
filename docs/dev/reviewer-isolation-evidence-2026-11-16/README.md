# Reviewer isolation — the review evidence (2026-11-16)

> **Why this directory exists.** The twenty-three independent reviews of the
> reviewer-isolation work were carried out by isolated reviewer subagents, not
> by GitHub's review machinery, so **GitHub holds no review objects for them**.
> The reports and the measurement corpora they produced therefore existed only
> in a session scratch directory and were **not in any branch**. This directory
> publishes them into the repository so the work's history survives with the
> code it describes.
>
> **This is a record, not an implementation.** Nothing here changes the
> substrate's behaviour. The code these reviews examined lives on the branch
> that carries this directory.

## The artefacts

| file | what it is |
|---|---|
| `review-round-22.md` | the twenty-second independent review — VERDICT: REQUEST CHANGES. It wrote 109 **novel** structural forms and measured each on the host three times in isolation. |
| `review-round-23.md` | the twenty-third independent review — VERDICT: REQUEST CHANGES, 7 MAJOR / 2 MINOR / 1 NIT, **exposure NONE**. It wrote 103 novel forms and measured 36 divergent or reason-mismatched ones, 103 runs, no flaky verdict. Its own report states that *"A1, A2 and A3 all hold up under the checks I ran"* — the findings were confined to the structural model. |
| `review-round-24-brief-unused.md` | the brief for a twenty-fourth review. It was **written and deliberately not used**, on the operator's direction to stop the matcher-fidelity campaign. It is published here so the decision is on the record rather than inferred from its absence. |
| `host-candidates-round-22.json` | the 109 novel forms the twenty-second review measured. |
| `host-candidates-round-23.json` | the 103 novel forms the twenty-third review measured. |
| `host-verdicts.json` | the host's verdicts for the measured forms — the reference the guard's corpus is checked against. |
| `decision-report.md` | the analysis behind the decision to split A1/A2 from A3, the enforcement table (what the host enforces versus what the OS enforces), **the measured sandbox feasibility experiment** (§4 — bubblewrap 0.11.1, the sentinel probe's five results), and the finite convergence gate. |
| `final-report.md` | the session's closing report: the two PRs, the compatibility decisions, the adversarial results, **the remaining security limitations** (ten numbered residual risks, plus the open browser and capability-loader routes) and the live-environment isolation record. |
| `charlie-external-review.md` | the **independent external review** — P7 APPROVED, A1/A2 APPROVED, **A3 HOLD**; the five corrections to the bubblewrap architecture; and the capability-removal architecture decision, with the reason the session-correlation alternative was rejected. |
| `host-verdicts-a3.json` | the **host's own verdicts** for the implemented reviewer definition, measured on the operator's installed OpenCode (2.3.0) — the resolved permission set the host holds for the agent, and the verdict for every action, with the built-in agents as controls. This is the record of the boundary the A3 implementation actually gets. |
| `a3-completion-report.md` | the **completion-gate report** for the A3 implementation: the enforced boundaries, the capabilities that remain host-side, the escape probes that succeeded and the ones that failed, the integration-test results, the CI run on the exact head, and the residual risks. |
| `charlie-a3-implementation-review.md` | the **independent external review of the implementation**, on a different model from the one that wrote it — **VERDICT: APPROVE**, with three findings (a metadata disclosure of the operator's home path, the full-history copy, and a wrong comment) and the disposition of each. |
| `delta-review-a19b1a78.md` | the **narrow delta review** of the one post-review commit that changed the namespace — **VERDICT: REQUEST CHANGES** (one CRITICAL: a PATH entry could make the operator's HOME a bind root), the **live reproduction** of it, the fix, and the reviewer's **verification of the fix — VERDICT: APPROVE**. |
| `host-verdicts-final.json` | the **host's own verdicts measured at the final head** (`fc1af83f`), with the built-in agents as controls. The reviewer agent's verdict is **deny for every action except `sandbox_exec`**; the built-in agents are unchanged. |
| `gate4-integration-results.txt` | the **full transcript of the merge gate's integration and adversarial suite** — 46 checks, 46 pass — run against the actual plugin execution path with disposable repositories, sentinel files, a temporary runtime root and a temporary database. |
| `a3-merge-gate-report.md` | the **final merge gate for PR #247**: the exact revisions, the delta and the independent review chain, the capability inventory, the integration and adversarial results, the CI state, the remaining documented risks, and the **APPROVE** recommendation. |

## Where each required record lives

| required record | where it is |
|---|---|
| Round-23 findings | `review-round-23.md` |
| Host-matcher measurements | `host-candidates-round-22.json`, `host-candidates-round-23.json`, `host-verdicts.json` |
| The sandbox feasibility experiment | `decision-report.md` §4 — the sentinel probe and its five results |
| The independent external Charlie findings | `charlie-external-review.md` |
| The known production-access incident | this README's disclosure below, and `final-report.md` §"Disclosures" |
| Remaining security limitations | `final-report.md` §"Remaining risks" — ten numbered items |

## The security screening this record passed

This repository is **public**. Everything in this directory was therefore
screened before publication, and the screen is recorded here so a later
reader can tell what was checked and what was changed.

| checked | result |
|---|---|
| Credentials, keys, tokens, private-key blocks | **none present.** The repository's own scanner test fixtures were excluded from the check — they are deliberate poison payloads in `internal/core/*_test.go`, not evidence. |
| Unfiltered private logs | **none published.** No host shell-log, no session log, no database dump. |
| Large disposable test directories | **none published.** Every artefact here is a text or JSON document; the largest is under 40 KB. |
| The operator's absolute home paths | **scrubbed.** Host-specific paths were replaced with canonical forms — the operator's checkout, the review worktree, the review scratch directories, the installed agent-definition directory, the shell-log directory. |
| The live substrate's path and size metadata | **scrubbed.** The live database path was replaced with `<workspace>/src/db/mpm.db`, and its byte counts with a statement that its size and content were observed and never changed. The **substance** of the isolation record — what was and was not opened — is preserved exactly; only the host-specific identifiers were removed. |

The scrub preserved meaning, not just formatting: every sentence that
described a boundary or an observation still says what it said. A reader can
still verify the claim *"the live substrate was never opened"* from the
record; they just cannot learn the operator's directory layout or the
production database's size from it.

**Nothing was deleted to make the screen pass.** The incidents, the
disclosures and the failed probes are all still here — they are the evidence.
What was removed is only the host-specific identifier that pointed at
somebody's machine.

## What the evidence says, in one paragraph

Across twenty-three rounds on the A3 branch and five on the P7 branch, **no
demonstrated production exposure** was found. Every divergent or
reason-mismatched form the reviews measured was **refused by the host** — the
matcher fails close on every one — and no binary was reached and no live
database was opened. The findings were **fidelity and evidence** defects, not
enforcement holes: the guard's model of the host's command matcher disagreed
with the host on forms the corpus did not pin.

The **external** review that followed reached a different conclusion about
the same evidence, and its conclusion is the one in force: the matcher is
**not** the boundary, so the A3 implementation is **HOLD** rather than
merged as a completed security boundary. The matcher research is kept as
historical evidence and optional defence in depth. See
`charlie-external-review.md`.

## What happened to the matcher campaign, and what replaced it

The matcher research above is **history, not the boundary**. The external
review's §3 correction was adopted: a permission rule is a pattern match on a
command string, so a route the rules do not name is still a route, and twenty-three
rounds of matcher fidelity could not change that. The campaign was **closed**, not
resumed.

What replaced it is **capability removal plus a real OS boundary**:

1. the reviewer's agent definition is **default-deny** (`action: "*"`,
   `resource: "*"`, `effect: deny`) and re-opens exactly **one** action —
   `sandbox_exec`, a host-controlled tool;
2. `sandbox_exec` **always** runs inside a bubblewrap mount namespace built by
   `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py`, where every path
   that is not bound **does not exist**;
3. there is **no unsandboxed fallback** and **no privileged mode**: the tool does
   not identify its caller, because it does not need to — it is always confined.

The guard test that modelled the old rules —
`agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py`,
4361 lines — was **deleted**, because the rules it modelled are gone and the
shell it policed is now denied outright. Its record stays in this directory.

The **measured** result is in `host-verdicts-a3.json`: on the operator's installed
host, the reviewer agent's verdict is **deny for every action except
`sandbox_exec`**, while the built-in agents measured in the same pass keep their
ordinary permissions. The report is `a3-completion-report.md`.

## The disclosure this record also carries

During an early round, a reviewer's probe ran the CLI with `HOME` and
`MPM_WORKSPACE` unset, and the CLI opened the operator's live database. The
reviewer disclosed it and it was written to the substrate. It is the reason
every later brief carries the rule *"set `HOME` and `MPM_WORKSPACE` first, in
the same command"*, and it is the incident the whole isolation design exists
to prevent. The remediation's own failing probe for it is
`agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py`.

## Status of the work these reviews examined

The code these reviews examined was **not** a completed security boundary, and
the documents say so. The bubblewrap namespace sandbox was proved and committed
but **inert** — it was not wired into the reviewer's actual execution path — so
the shell protection was **best-effort**, and the tool-catalogue protection was
the only **enforced** layer.

**That is no longer the state of the branch.** The A3 implementation now closes
the gap the reviews identified: the sandbox is on the reviewer's only execution
path, the tool catalogue is default-deny, the host-side file tools and the
browser and capability-loader actions are closed by name, and the install fails
closed. What the boundary does **not** cover — the operator's own session, the
built-in `explore` and `general` subagents, the Go module cache, and the
sandbox base's readability by the operator — is stated in
`a3-completion-report.md` rather than glossed, and the branch is **not** merged
or deployed.

## The merge gate, and the one escape it found

The final merge gate for PR #247 is `a3-merge-gate-report.md`. Its
recommendation is **APPROVE** — the branch is ready for an
operator-authorised merge.

The gate did find a real escape, and it is worth recording how: the
post-review commit `a19b1a78` had added a **toolchain discovery** so a
reviewer on a CI runner would still have a compiler. The narrow delta
review of that one commit found that a PATH entry whose `bin` directory
is a direct child of a home — `~/bin` — made the discovery derive **the
home itself** as a bind root, so the operator's credentials became
readable from inside the namespace. The gate reproduced it live, fixed
it (`fc1af83f`), and the same reviewer re-verified the fix:
**APPROVE**. The full transcript — including the reproduction and the
reviewer's own answers to the verification brief — is in
`delta-review-a19b1a78.md`.

The lesson the record carries: a convenience feature that reads the
host's `PATH` inherits the host's `PATH` as a threat, and the check that
guards it must be at least as strict as the check that guards the base.
