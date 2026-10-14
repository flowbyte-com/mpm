# Work tracking and issue classification

**GitHub Issues is the authoritative MPM work backlog.** Not this file, and
not [`CLOSEOUT.md`](./CLOSEOUT.md).

## Where work is tracked

| What | Where |
|---|---|
| Open work, bugs, enhancements, verification gaps | [GitHub Issues](https://github.com/flowbyte-com/mpm/issues) |
| Historical stabilisation record (2026-09-13 → 2026-10-13) | [`CLOSEOUT.md`](./CLOSEOUT.md) — frozen, not updated |
| Architecture, behavioural contracts, CLI reference | [`SPEC.md`](./SPEC.md) |
| Measurement write-ups behind a decision | [`RETRIEVAL-BENCHMARK.md`](./RETRIEVAL-BENCHMARK.md), [`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md), [`NOMIC-MIGRATION.md`](./NOMIC-MIGRATION.md), [`RECALL-MUTATION.md`](./RECALL-MUTATION.md) |

`CLOSEOUT.md` §7 links to the issues that emerged from it. That is the only
duplication, and it is one-directional — issues do not link back as a work
list.

If a finding is not in an issue, it is not tracked. If it is only in a
document, it is a record of something that happened, not a commitment.

## How findings are classified

Every finding resolves to exactly one of these. The distinction matters
because it determines whether something should be *fixed* at all.

| Class | Meaning | What closes it |
|---|---|---|
| **Confirmed open defect** | Incorrect behaviour, demonstrated with evidence. | A fix plus a regression test that fails against the pre-fix code. |
| **Verification gap** | Behaviour nobody has proven either way. | A test that establishes the answer — either result is a valid outcome. |
| **Deferred enhancement** | Valuable, not necessary for correctness. | A decision to do it, or a decision to close it with reasoning. |
| **Deliberate design decision / known limitation** | Documented trade-off, not a bug. | Nothing. Do not re-report. |
| **Already fixed** | Real once, no longer. | Nothing. Verify before re-reporting. |

Two rules that keep the backlog small:

- **Do not file an issue for a design decision.** A measured, documented
  trade-off is not an unfinished bug. `CLOSEOUT.md` §4 holds the current
  set; §7.3 lists which were deliberately not converted.
- **Do not file an issue for something already fixed.** Verify against the
  implementation and its tests first. Documentation describing a pre-fix
  world is a documentation defect, not evidence that the defect survives —
  and those are two different issues when both are true.

## Reporting a new issue

Reproducible evidence is the whole ask. An issue without it will be closed
or asked to come back.

Include:

- **Current behaviour** — what actually happens, not what you expected.
- **Expected behaviour** — or, if the design is genuinely undecided, say
  that. Issues here deliberately avoid prescribing an implementation where
  the right answer is a product decision.
- **Evidence** — exact file paths and line numbers, the code or SQL, test
  names, benchmark figures with their method, and a command to reproduce.
- **Impact** — who is affected and what it costs them.
- **Acceptance criteria** — specific conditions that would let it close.
- **Related work** — existing issues, documents, commits.

Useful distinctions to get right:

- **A failure that nothing errored on** is still a defect. Silent no-ops are
  the expensive class; an error is a gift.
- **A test that passes because it skipped** is not coverage. `CLOSEOUT.md`
  §1.6 and §1.8 document two separate occasions where a green suite hid
  that the fixes were untested. Check for `SKIP`, not just the exit code.
- **Documentation describing code** is a correctness surface.
  `CLOSEOUT.md` §1.9 fixed a user-facing string that had become false after
  a migration, and recorded the general lesson. When a doc and the
  implementation disagree, the implementation is right — file the doc.

## Labels

The repository uses the default GitHub labels. Applied usage:

| Label | For |
|---|---|
| `bug` | Confirmed open defect, demonstrated incorrect behaviour |
| `enhancement` | Deferred enhancement, or a verification gap with real cost |
| `documentation` | A document that misstates the implementation, or a missing operator workflow |

Priority is not a label. It is stated in the issue body under **Priority**,
with the justification attached, so the reasoning survives a label change.

## Integration verification levels

Three axes, deliberately kept separate — collapsing them is how "runtime
loading is verified" gets claimed on the strength of evidence that only
proves registration. See `CLOSEOUT.md` §3.1.

| Axis | What it proves |
|---|---|
| **Historical provenance** | The adapter's code path executed and reached the binary. `framework_name` is self-reported, so this does not prove the host loaded anything. |
| **Present installation state** | The payload is staged on disk and byte-current with source. |
| **Direct host verification** | A positive load receipt from the host itself. **This is the only axis that proves runtime loading.** |

No load-receipt mechanism exists, so no adapter's runtime loading is
verified. `mpm onboard` emits this caveat unconditionally
(`cmd/mpm/onboard_cmds.go:1024-1034`), and
`TestOnboardNeverClaimsUnverifiedRuntime` fails if it stops doing so.

This is not a code defect. It is recorded as a limitation because it is
genuinely unproven, and it is the reason onboarding reports a filesystem
fact and says so plainly rather than reporting a runtime fact.