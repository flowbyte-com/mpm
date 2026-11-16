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

## The disclosure this record also carries

During an early round, a reviewer's probe ran the CLI with `HOME` and
`MPM_WORKSPACE` unset, and the CLI opened the operator's live database. The
reviewer disclosed it and it was written to the substrate. It is the reason
every later brief carries the rule *"set `HOME` and `MPM_WORKSPACE` first, in
the same command"*, and it is the incident the whole isolation design exists
to prevent. The remediation's own failing probe for it is
`agent_installation/tests/test_reviewer_isolation_denies_every_mpm_tool.py`.

## Status of the work these reviews examined

The code these reviews examined is **not** a completed security boundary, and
the documents say so. The bubblewrap namespace sandbox is proved and
committed but **inert** — it is not wired into the reviewer's actual execution
path — so the shell protection today is **best-effort**, and the tool-catalogue
protection is the only **enforced** layer. The residual shell routes, the
browser family, the capability loader and the host-side file tools are all
documented rather than glossed.
