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
| `decision-report.md` | the analysis behind the decision to split A1/A2 from A3, the enforcement table (what the host enforces versus what the OS enforces), and the finite convergence gate. |
| `final-report.md` | the session's closing report: the two PRs, the compatibility decisions, the adversarial results, the remaining risks and the live-environment isolation record. |

## What the evidence says, in one paragraph

Across twenty-three rounds on the A3 branch and five on the P7 branch, **no
demonstrated production exposure** was found. Every divergent or
reason-mismatched form the reviews measured was **refused by the host** — the
matcher fails closed on every one — and no binary was reached and no live
database was opened. The findings were **fidelity and evidence** defects, not
enforcement holes: the guard's model of the host's command matcher disagreed
with the host on forms the corpus did not pin.

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
