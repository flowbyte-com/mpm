# docs/archive/

Historical and reference material kept for the record, not surfaced as
authoritative documentation.

## What lives here

Files in this directory are **historical artifacts** — design briefs,
RFCs, release notes, audits, concept vocabularies, and superseded
architectural specs that captured a specific moment in MPM's
evolution. They are not maintained, not linked from current docs, and
may reference features, schemas, or design decisions that have since
changed.

The structure is intentionally **flat** — every file is at the root of
this directory. Subdirectory nesting is unnecessary because the archive
is read by humans when looking for a specific artifact, not browsed by
path. Filenames carry the metadata (date prefix, `-design` for specs,
`-phase-N` for implementation plans, etc.).

If you're reading current MPM documentation, you want:

- [`../README.md`](../README.md) — the substrate overview
- [`../INSTALL.md`](../INSTALL.md) — MPM install procedure
- [`../architecture.md`](../architecture.md) — canonical architectural principles
- [`../concept-vocabulary-authoring.md`](../concept-vocabulary-authoring.md) — active authoring convention
- [`../../agent_installation/INSTALL.md`](../../agent_installation/INSTALL.md) — per-agent installation

## Contents

### Release notes

| File | Origin | What it captures |
|---|---|---|
| `RELEASE-NOTES-mpm-alpha.md` | 2026-08-08 | First public alpha — what shipped, what was deferred |

### Audits

| File | Origin | What it captures |
|---|---|---|
| `RETRIEVAL_CORRECTNESS_AUDIT_2026-08-26.md` | 2026-08-26 | Post-fix verification of HybridSearch vector-only candidate preservation |

### Pre-implementation design briefs (RFCs)

| File | Origin | What it captures |
|---|---|---|
| `phase3-design.md` | Phase 3 | Adaptive Retrieval & Progressive Materialization design pre-implementation |
| `work-primitive-design.md` | 2026-08-23 | Work primitive design — pre-implementation spec |
| `cli-design.md` | GREENLIT 2026-07-29 | CLI Refactor RFC — Cognitive Interface (No Substrate Changes) |
| `pointer-architecture-design.md` | 2026-08-21 | Pointer architecture phase 1 design |
| `pointer-architecture-phase-1.md` | 2026-08-21 | Pointer architecture phase 1 implementation plan |
| `artifact-provenance-design.md` | 2026-08-08 | Artifact provenance design |
| `artifact-provenance.md` | 2026-08-08 | Artifact provenance implementation plan |
| `epistemic-cascades-design.md` | 2026-08-04 | Epistemic cascades design |
| `epistemic-cascades.md` | 2026-08-04 | Epistemic cascades implementation plan |
| `adaptive-drain-yielding-design.md` | 2026-08-05 | Adaptive drain yielding design |
| `adaptive-drain-yielding.md` | 2026-08-05 | Adaptive drain yielding implementation |
| `capability-evidence-model.md` | 2026-08-05 | Capability evidence model spec |
| `skill-health-design.md` | 2026-08-05 | Skill health design |
| `skills-in-mpm-design.md` | 2026-07-25 | Skills-in-MPM design |
| `skills-in-mpm.md` | 2026-07-25 | Skills-in-MPM implementation plan |
| `audit-gap-closure-design.md` | 2026-07-23 | Audit gap closure design |
| `cascade-yield-budget-design.md` | 2026-08-08 | Cascade yield budget design |
| `unix-epoch-timestamps-design.md` | 2026-07-30 | Unix-epoch timestamps design |
| `unix-epoch-timestamps.md` | 2026-07-30 | Unix-epoch timestamps implementation |
| `mpm-mcp-evidence-confidence-sync-design.md` | 2026-06-19 | mpm-mcp evidence/confidence sync design |
| `confidence-evidence-foundation-design.md` | 2026-06-16 | Confidence/evidence foundation design |
| `confidence-evidence-foundation.md` | 2026-06-16 | Confidence/evidence foundation implementation |
| `claude-code-route-hook-design.md` | 2026-06-16 | Claude Code route hook design |
| `claude-code-route-hook.md` | 2026-06-16 | Claude Code route hook implementation |
| `claude-mpm-plugin-design.md` | 2026-06-15 | claude-mpm plugin design |
| `claude-mpm-plugin.md` | 2026-06-15 | claude-mpm plugin implementation plan |
| `mcp-tool-port.md` | 2026-06-15 | MCP tool port plan |
| `strip-web-layer.md` | 2026-07-16 | Web layer strip-down plan |

### Specs

| File | Origin | What it captures |
|---|---|---|
| `directives.md` | Multi-framework | Directives architecture & multi-framework scoping (was operator-facing contract; archived after the canonical contract moved into runtime) |
| `capability-lifecycle.md` | 2026-08-05 | Capability lifecycle technical specification |
| `telemetry-binary-design.md` | 2026-08-21 | Telemetry binary format design |
| `telemetry-binary.md` | 2026-08-21 | Telemetry binary format implementation |
| `mpm-retrieval-projection.md` | 2026-08-27 | MPM retrieval projection plan |
| `work-primitive.md` | 2026-08-23 | Work primitive implementation |

### Wishlist / incremental work

| File | Origin | What it captures |
|---|---|---|
| `shared-epistemology.md` | Multi-agent wishlist | Core ATTACH architecture shipped (commits `18226c8`, `b37cab0`); remaining items are incremental UX work |

### Concept vocabularies

| File | Origin | What it captures |
|---|---|---|
| `f71-challenge-restoration.md` | F7.1 alpha-p1 fix | Synonym-rich anchor set for cross-session archaeology of the challenge/restoration pair |
| `f81-cancel-verification.md` | F8.1 fix | Synonym-rich anchor set for cancel/verification pair |

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
`git mv <path> docs/archive/<filename>`. Preserve the original
filename. Update the table above.

If a doc's content has been **superseded** by a current doc, link the
two at the top of the current doc rather than relying on
archival-by-absence.
