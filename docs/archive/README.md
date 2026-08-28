# docs/archive/

Historical and reference material kept for the record, not surfaced as
authoritative documentation.

## What lives here

Files in this directory are **historical artifacts** — design briefs,
RFCs, release notes, audits, and concept vocabularies that captured a
specific moment in MPM's evolution. They are not maintained, not
linked from current docs, and may reference features, schemas, or
design decisions that have since changed.

If you're reading current MPM documentation, you want:

- [`../README.md`](../README.md) — the substrate overview
- [`../INSTALL.md`](../INSTALL.md) — MPM install procedure
- [`../architecture.md`](../architecture.md) — canonical architectural principles
- [`../architecture/directives.md`](../architecture/directives.md) — directives contract
- [`../architecture/epistemic-cascades.md`](../architecture/epistemic-cascades.md) — outbox/operator guide
- [`../../agent_installation/INSTALL.md`](../../agent_installation/INSTALL.md) — per-agent installation

## Contents

### Top level

| File | Origin | What it captures |
|---|---|---|
| `RELEASE-NOTES-mpm-alpha.md` | 2026-08-08 | First public alpha — what shipped, what was deferred |
| `RETRIEVAL_CORRECTNESS_AUDIT_2026-08-26.md` | 2026-08-26 | Post-fix verification of HybridSearch vector-only candidate preservation |
| `phase3-design.md` | Phase 3 design brief | Adaptive Retrieval & Progressive Materialization design pre-implementation |
| `work-primitive-design.md` | 2026-08-23 | Work primitive design — pre-implementation spec |

### architecture/

| File | Origin | What it captures |
|---|---|---|
| `shared-epistemology.md` | Multi-agent wishlist | Core ATTACH architecture shipped (commits `18226c8`, `b37cab0`); remaining items are incremental UX work, not architectural gaps |

### concept-vocabularies/

| File | Origin | What it captures |
|---|---|---|
| `f71-challenge-restoration.md` | F7.1 alpha-p1 fix | Synonym-rich anchor set for cross-session archaeology of the challenge/restoration pair |
| `f81-cancel-verification.md` | F8.1 fix | Synonym-rich anchor set for cancel/verification pair |

### superpowers/plans/

| File | Origin | What it captures |
|---|---|---|
| `2026-08-21-pointer-architecture-phase-1.md` | 2026-08-21 | Pointer architecture phase 1 implementation plan |

## Why archive, not delete

Three reasons:

1. **Historical record.** RFCs and design briefs capture the reasoning
   behind decisions that the current code can't fully express. When a
   later contributor asks "why is X shaped this way?" the answer often
   lives in a doc like this.
2. **Concept vocabularies anchor prior fixes.** The F7.1 and F8.1
   vocabularies are used as synonyms when reasoning about prior work
   in cross-session archaeology. They're a fixed reference, not active
   design.
3. **Audit evidence.** The retrieval correctness audit is a
   post-fix-verification artifact; it's how we proved a specific bug
   is gone. Keep it.

These files are not deleted because deletion is irreversible and the
storage cost is negligible. They are not surfaced as current
documentation because doing so implies maintenance.

## Adding to this archive

When a doc is no longer current — because the work is shipped, the
design is superseded, or the audit is closed — move it here with
`git mv <path> docs/archive/`. Preserve the original filename. Update
the table above.

If a doc's content has been **superseded** by a current doc, link the
two at the top of the current doc rather than relying on
archival-by-absence.
